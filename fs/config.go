package fs

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/swiftstack/ProxyFS/conf"
	"github.com/swiftstack/ProxyFS/inode"
	"github.com/swiftstack/ProxyFS/logger"
)

type inFlightFileInodeDataStruct struct {
	inode.InodeNumber                 // Indicates the InodeNumber of a fileInode with unflushed
	volStruct          *volumeStruct  // Synchronized via volStruct's sync.Mutex
	control            chan bool      // Signal with true to flush (and exit), false to simply exit
	wg                 sync.WaitGroup // Client can know when done
	globalsListElement *list.Element  // Back-pointer to wrapper used to insert into globals.inFlightFileInodeDataList
}

// inFlightFileInodeDataControlBuffering specifies the inFlightFileInodeDataStruct.control channel buffer size
// Note: There are potentially multiple initiators of this signal
const inFlightFileInodeDataControlBuffering = 100

type mountStruct struct {
	id        MountID
	options   MountOptions
	volStruct *volumeStruct
}

type volumeStruct struct {
	sync.Mutex
	volumeName               string
	maxFlushTime             time.Duration
	FLockMap                 map[inode.InodeNumber]*list.List
	inFlightFileInodeDataMap map[inode.InodeNumber]*inFlightFileInodeDataStruct
	mountList                []MountID
	inode.VolumeHandle
}

type globalsStruct struct {
	sync.Mutex
	whoAmI                    string
	volumeMap                 map[string]*volumeStruct
	mountMap                  map[MountID]*mountStruct
	lastMountID               MountID
	inFlightFileInodeDataList *list.List
}

var globals globalsStruct

func Up(confMap conf.ConfMap) (err error) {
	var (
		flowControlName string
		primaryPeerList []string
		volume          *volumeStruct
		volumeList      []string
		volumeName      string
	)

	globals.whoAmI, err = confMap.FetchOptionValueString("Cluster", "WhoAmI")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueString(\"Cluster\", \"WhoAmI\") failed: %v", err)
		return
	}

	volumeList, err = confMap.FetchOptionValueStringSlice("FSGlobals", "VolumeList")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"FSGlobals\", \"VolumeList\") failed: %v", err)
		return
	}

	globals.volumeMap = make(map[string]*volumeStruct)

	for _, volumeName = range volumeList {
		primaryPeerList, err = confMap.FetchOptionValueStringSlice(volumeName, "PrimaryPeer")
		if nil != err {
			err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"%s\", \"PrimaryPeer\") failed: %v", volumeName, err)
			return
		}

		if 0 == len(primaryPeerList) {
			continue
		} else if 1 == len(primaryPeerList) {
			if globals.whoAmI == primaryPeerList[0] {
				volume = &volumeStruct{
					volumeName:               volumeName,
					FLockMap:                 make(map[inode.InodeNumber]*list.List),
					inFlightFileInodeDataMap: make(map[inode.InodeNumber]*inFlightFileInodeDataStruct),
					mountList:                make([]MountID, 0),
				}

				flowControlName, err = confMap.FetchOptionValueString(volumeName, "FlowControl")
				if nil != err {
					return
				}

				volume.maxFlushTime, err = confMap.FetchOptionValueDuration(flowControlName, "MaxFlushTime")
				if nil != err {
					return
				}

				volume.VolumeHandle, err = inode.FetchVolumeHandle(volumeName)
				if nil != err {
					return
				}

				globals.volumeMap[volumeName] = volume
			}
		} else {
			err = fmt.Errorf("%v.PrimaryPeer cannot be multi-valued", volumeName)
			return
		}
	}

	globals.mountMap = make(map[MountID]*mountStruct)
	globals.lastMountID = MountID(0)
	globals.inFlightFileInodeDataList = list.New()

	return
}

func PauseAndContract(confMap conf.ConfMap) (err error) {
	var (
		id                MountID
		ok                bool
		primaryPeerList   []string
		removedVolumeList []string
		updatedVolumeMap  map[string]bool // key == volumeName; value ignored
		volume            *volumeStruct
		volumeList        []string
		volumeName        string
		whoAmI            string
	)

	whoAmI, err = confMap.FetchOptionValueString("Cluster", "WhoAmI")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueString(\"Cluster\", \"WhoAmI\") failed: %v", err)
		return
	}
	if whoAmI != globals.whoAmI {
		err = fmt.Errorf("confMap change not allowed to alter [Cluster]WhoAmI")
		return
	}

	volumeList, err = confMap.FetchOptionValueStringSlice("FSGlobals", "VolumeList")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"FSGlobals\", \"VolumeList\") failed: %v", err)
		return
	}

	updatedVolumeMap = make(map[string]bool)

	for _, volumeName = range volumeList {
		primaryPeerList, err = confMap.FetchOptionValueStringSlice(volumeName, "PrimaryPeer")
		if nil != err {
			err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"%s\", \"PrimaryPeer\") failed: %v", volumeName, err)
			return
		}

		if 0 == len(primaryPeerList) {
			continue
		} else if 1 == len(primaryPeerList) {
			if globals.whoAmI == primaryPeerList[0] {
				updatedVolumeMap[volumeName] = true
			}
		} else {
			err = fmt.Errorf("%v.PrimaryPeer cannot be multi-valued", volumeName)
			return
		}
	}

	removedVolumeList = make([]string, 0, len(globals.volumeMap))

	for volumeName = range globals.volumeMap {
		_, ok = updatedVolumeMap[volumeName]
		if !ok {
			removedVolumeList = append(removedVolumeList, volumeName)
		}
	}

	for _, volumeName = range removedVolumeList {
		volume = globals.volumeMap[volumeName]
		for _, id = range volume.mountList {
			delete(globals.mountMap, id)
		}
		volume.untrackInFlightFileInodeDataAll()
		globals.Lock()
		delete(globals.volumeMap, volumeName)
		globals.Unlock()
	}

	err = nil
	return
}

func ExpandAndResume(confMap conf.ConfMap) (err error) {
	var (
		flowControlName string
		ok              bool
		primaryPeerList []string
		volume          *volumeStruct
		volumeList      []string
		volumeName      string
		whoAmI          string
	)

	whoAmI, err = confMap.FetchOptionValueString("Cluster", "WhoAmI")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueString(\"Cluster\", \"WhoAmI\") failed: %v", err)
		return
	}
	if whoAmI != globals.whoAmI {
		err = fmt.Errorf("confMap change not allowed to alter [Cluster]WhoAmI")
		return
	}

	volumeList, err = confMap.FetchOptionValueStringSlice("FSGlobals", "VolumeList")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"FSGlobals\", \"VolumeList\") failed: %v", err)
		return
	}

	for _, volumeName = range volumeList {
		primaryPeerList, err = confMap.FetchOptionValueStringSlice(volumeName, "PrimaryPeer")
		if nil != err {
			err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"%s\", \"PrimaryPeer\") failed: %v", volumeName, err)
			return
		}

		if 0 == len(primaryPeerList) {
			continue
		} else if 1 == len(primaryPeerList) {
			if globals.whoAmI == primaryPeerList[0] {
				volume, ok = globals.volumeMap[volumeName]
				if !ok {
					volume = &volumeStruct{
						volumeName:               volumeName,
						FLockMap:                 make(map[inode.InodeNumber]*list.List),
						inFlightFileInodeDataMap: make(map[inode.InodeNumber]*inFlightFileInodeDataStruct),
						mountList:                make([]MountID, 0),
					}

					flowControlName, err = confMap.FetchOptionValueString(volumeName, "FlowControl")
					if nil != err {
						return
					}

					volume.maxFlushTime, err = confMap.FetchOptionValueDuration(flowControlName, "MaxFlushTime")
					if nil != err {
						return
					}

					volume.VolumeHandle, err = inode.FetchVolumeHandle(volumeName)
					if nil != err {
						return
					}

					globals.volumeMap[volumeName] = volume
				}
			}
		} else {
			err = fmt.Errorf("%v.PrimaryPeer cannot be multi-valued", volumeName)
			return
		}
	}

	err = nil
	return
}

func Down() (err error) {
	var (
		volume *volumeStruct
	)

	for _, volume = range globals.volumeMap {
		volume.untrackInFlightFileInodeDataAll()
	}

	if 0 < globals.inFlightFileInodeDataList.Len() {
		logger.Fatalf("fs.Down() has completed all un-mount's... but found non-empty globals.inFlightFileInodeDataList")
	}

	err = nil
	return
}
