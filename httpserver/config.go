package httpserver

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/swiftstack/sortedmap"

	"github.com/swiftstack/ProxyFS/conf"
	"github.com/swiftstack/ProxyFS/fs"
	"github.com/swiftstack/ProxyFS/headhunter"
	"github.com/swiftstack/ProxyFS/inode"
)

type ExtentMapElementStruct struct {
	FileOffset    uint64 `json:"file_offset"`
	ContainerName string `json:"container_name"`
	ObjectName    string `json:"object_name"`
	ObjectOffset  uint64 `json:"object_offset"`
	Length        uint64 `json:"length"`
}

type jobState uint8

const (
	jobRunning jobState = iota
	jobHalted
	jobCompleted
)

type jobTypeType uint8

const (
	fsckJobType jobTypeType = iota
	scrubJobType
	limitJobType
)

type jobStruct struct {
	id        uint64
	volume    *volumeStruct
	jobHandle fs.JobHandle
	state     jobState
	startTime time.Time
	endTime   time.Time
}

// JobStatusJSONPackedStruct describes all the possible fields returned in JSON-encoded job GET body
type JobStatusJSONPackedStruct struct {
	StartTime string   `json:"start time"`
	HaltTime  string   `json:"halt time"`
	DoneTime  string   `json:"done time"`
	ErrorList []string `json:"error list"`
	InfoList  []string `json:"info list"`
}

type volumeStruct struct {
	sync.Mutex
	name                   string
	fsMountHandle          fs.MountHandle
	inodeVolumeHandle      inode.VolumeHandle
	headhunterVolumeHandle headhunter.VolumeHandle
	fsckActiveJob          *jobStruct
	fsckJobs               sortedmap.LLRBTree // Key == jobStruct.id, Value == *jobStruct
	scrubActiveJob         *jobStruct
	scrubJobs              sortedmap.LLRBTree // Key == jobStruct.id, Value == *jobStruct
}

type globalsStruct struct {
	sync.Mutex
	active            bool
	jobHistoryMaxSize uint32
	whoAmI            string
	ipAddr            string
	tcpPort           uint16
	ipAddrTCPPort     string
	netListener       net.Listener
	wg                sync.WaitGroup
	confMap           conf.ConfMap
	volumeLLRB        sortedmap.LLRBTree // Key == volumeStruct.name, Value == *volumeStruct
}

var globals globalsStruct

func Up(confMap conf.ConfMap) (err error) {
	var (
		ok              bool
		primaryPeerList []string
		volume          *volumeStruct
		volumeList      []string
		volumeName      string
	)

	globals.confMap = confMap

	globals.jobHistoryMaxSize, err = confMap.FetchOptionValueUint32("HTTPServer", "JobHistoryMaxSize")
	if nil != err {
		/*
			TODO: Eventually change this to:
				err = fmt.Errorf("confMap.FetchOptionValueString(\"HTTPServer\", \"JobHistoryMaxSize\") failed: %v", err)
				return
		*/
		globals.jobHistoryMaxSize = 5
	}

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

	globals.volumeLLRB = sortedmap.NewLLRBTree(sortedmap.CompareString, nil)

	for _, volumeName = range volumeList {
		primaryPeerList, err = confMap.FetchOptionValueStringSlice("Volume:"+volumeName, "PrimaryPeer")
		if nil != err {
			err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"%s\", \"PrimaryPeer\") failed: %v", volumeName, err)
			return
		}

		if 0 == len(primaryPeerList) {
			continue
		} else if 1 == len(primaryPeerList) {
			if globals.whoAmI == primaryPeerList[0] {
				volume = &volumeStruct{
					name:           volumeName,
					fsckActiveJob:  nil,
					fsckJobs:       sortedmap.NewLLRBTree(sortedmap.CompareUint64, nil),
					scrubActiveJob: nil,
					scrubJobs:      sortedmap.NewLLRBTree(sortedmap.CompareUint64, nil),
				}

				volume.fsMountHandle, err = fs.Mount(volume.name, 0)
				if nil != err {
					return
				}

				volume.inodeVolumeHandle, err = inode.FetchVolumeHandle(volume.name)
				if nil != err {
					return
				}

				volume.headhunterVolumeHandle, err = headhunter.FetchVolumeHandle(volume.name)
				if nil != err {
					return
				}

				ok, err = globals.volumeLLRB.Put(volumeName, volume)
				if nil != err {
					err = fmt.Errorf("statsLLRB.Put(%v,) failed: %v", volumeName, err)
					return
				}
				if !ok {
					err = fmt.Errorf("statsLLRB.Put(%v,) returned ok == false", volumeName)
					return
				}
			}
		} else {
			err = fmt.Errorf("Volume \"%v\" cannot have multiple PrimaryPeer values", volumeName)
		}
	}

	globals.ipAddr, err = confMap.FetchOptionValueString("Peer:"+globals.whoAmI, "PrivateIPAddr")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueString(\"<whoAmI>\", \"PrivateIPAddr\") failed: %v", err)
		return
	}

	globals.tcpPort, err = confMap.FetchOptionValueUint16("HTTPServer", "TCPPort")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueUint16(\"HTTPServer\", \"TCPPort\") failed: %v", err)
		return
	}

	globals.ipAddrTCPPort = net.JoinHostPort(globals.ipAddr, strconv.Itoa(int(globals.tcpPort)))

	globals.netListener, err = net.Listen("tcp", globals.ipAddrTCPPort)
	if nil != err {
		err = fmt.Errorf("net.Listen(\"tcp\", \"%s\") failed: %v", globals.ipAddrTCPPort, err)
		return
	}

	globals.active = true
	globals.wg.Add(1)
	go serveHTTP()

	err = nil
	return
}

func PauseAndContract(confMap conf.ConfMap) (err error) {
	var (
		ipAddr          string
		numVolumes      int
		ok              bool
		primaryPeerList []string
		tcpPort         uint16
		volumeIndex     int
		volumeList      []string
		volumeMap       map[string]bool
		volumeName      string
		volumeNameAsKey sortedmap.Key
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

	ipAddr, err = confMap.FetchOptionValueString("Peer:"+whoAmI, "PrivateIPAddr")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueString(\"<whoAmI>\", \"PrivateIPAddr\") failed: %v", err)
		return
	}
	if ipAddr != globals.ipAddr {
		err = fmt.Errorf("confMap change not allowed to alter [<whoAmI>]PrivateIPAddr")
		return
	}

	tcpPort, err = confMap.FetchOptionValueUint16("HTTPServer", "TCPPort")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueString(\"HTTPServer\", \"TCPPort\") failed: %v", err)
		return
	}
	if tcpPort != globals.tcpPort {
		err = fmt.Errorf("confMap change not allowed to alter [HTTPServer]TCPPort")
		return
	}

	globals.Lock()
	defer globals.Unlock()

	globals.active = false

	err = stopRunningJobs()
	if nil != err {
		globals.active = true
		return
	}

	volumeList, err = confMap.FetchOptionValueStringSlice("FSGlobals", "VolumeList")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"FSGlobals\", \"VolumeList\") failed: %v", err)
		return
	}

	volumeMap = make(map[string]bool)

	for _, volumeName = range volumeList {
		primaryPeerList, err = confMap.FetchOptionValueStringSlice("Volume:"+volumeName, "PrimaryPeer")
		if nil != err {
			err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"%s\", \"PrimaryPeer\") failed: %v", volumeName, err)
			return
		}

		if 0 == len(primaryPeerList) {
			continue
		} else if 1 == len(primaryPeerList) {
			if globals.whoAmI == primaryPeerList[0] {
				volumeMap[volumeName] = true
			}
		} else {
			err = fmt.Errorf("Volume \"%v\" cannot have multiple PrimaryPeer values", volumeName)
		}
	}

	volumeIndex = 0

	for {
		numVolumes, err = globals.volumeLLRB.Len()
		if nil != err {
			err = fmt.Errorf("globals.volumeLLRB.Len() failed: %v", err)
			return
		}

		if volumeIndex == numVolumes {
			err = nil
			return
		}

		volumeNameAsKey, _, ok, err = globals.volumeLLRB.GetByIndex(volumeIndex)
		if nil != err {
			err = fmt.Errorf("globals.volumeLLRB.GetByIndex(%v) failed: %v", volumeIndex, err)
			return
		}
		if !ok {
			err = fmt.Errorf("globals.volumeLLRB.GetByIndex(%v) returned ok == false", volumeIndex)
			return
		}

		volumeName, ok = volumeNameAsKey.(string)
		if !ok {
			err = fmt.Errorf("volumeNameAsKey.(string) for index %v returned ok == false", volumeIndex)
			return
		}

		_, ok = volumeMap[volumeName]

		if ok {
			volumeIndex++
		} else {
			ok, err = globals.volumeLLRB.DeleteByIndex(volumeIndex)
			if nil != err {
				err = fmt.Errorf("globals.volumeLLRB.DeleteByIndex(%v) failed: %v", volumeIndex, err)
				return
			}
			if !ok {
				err = fmt.Errorf("globals.volumeLLRB.DeleteByIndex(%v) returned ok == false", volumeIndex)
				return
			}
		}
	}
}

func ExpandAndResume(confMap conf.ConfMap) (err error) {
	var (
		ok              bool
		primaryPeerList []string
		volume          *volumeStruct
		volumeList      []string
		volumeName      string
	)

	globals.Lock()
	defer globals.Unlock()

	globals.jobHistoryMaxSize, err = confMap.FetchOptionValueUint32("HTTPServer", "JobHistoryMaxSize")
	if nil != err {
		/*
			TODO: Eventually change this to:
				err = fmt.Errorf("confMap.FetchOptionValueString(\"HTTPServer\", \"JobHistoryMaxSize\") failed: %v", err)
				return
		*/
		globals.jobHistoryMaxSize = 5
	}

	volumeList, err = confMap.FetchOptionValueStringSlice("FSGlobals", "VolumeList")
	if nil != err {
		err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"FSGlobals\", \"VolumeList\") failed: %v", err)
		return
	}

	for _, volumeName = range volumeList {
		primaryPeerList, err = confMap.FetchOptionValueStringSlice("Volume:"+volumeName, "PrimaryPeer")
		if nil != err {
			err = fmt.Errorf("confMap.FetchOptionValueStringSlice(\"%s\", \"PrimaryPeer\") failed: %v", volumeName, err)
			return
		}

		if 0 == len(primaryPeerList) {
			continue
		} else if 1 == len(primaryPeerList) {
			if globals.whoAmI == primaryPeerList[0] {
				_, ok, err = globals.volumeLLRB.GetByKey(volumeName)
				if nil != err {
					err = fmt.Errorf("globals.volumeLLRB.GetByKey(%v)) failed: %v", volumeName, err)
					return
				}
				if !ok {
					volume = &volumeStruct{
						name:          volumeName,
						fsckActiveJob: nil,
						fsckJobs:      sortedmap.NewLLRBTree(sortedmap.CompareUint64, nil),
					}

					volume.fsMountHandle, err = fs.Mount(volume.name, 0)
					if nil != err {
						return
					}

					volume.inodeVolumeHandle, err = inode.FetchVolumeHandle(volume.name)
					if nil != err {
						return
					}

					volume.headhunterVolumeHandle, err = headhunter.FetchVolumeHandle(volume.name)
					if nil != err {
						return
					}

					ok, err = globals.volumeLLRB.Put(volumeName, volume)
					if nil != err {
						err = fmt.Errorf("statsLLRB.Put(%v,) failed: %v", volumeName, err)
						return
					}
					if !ok {
						err = fmt.Errorf("statsLLRB.Put(%v,) returned ok == false", volumeName)
						return
					}
				}
			}
		} else {
			err = fmt.Errorf("Volume \"%v\" cannot have multiple PrimaryPeer values", volumeName)
		}
	}

	globals.active = true

	globals.confMap = confMap

	err = nil
	return
}

func Down() (err error) {
	globals.Lock()
	_ = stopRunningJobs()
	_ = globals.netListener.Close()
	globals.Unlock()

	globals.wg.Wait()

	err = nil
	return
}

func stopRunningJobs() (err error) {
	var (
		numVolumes    int
		ok            bool
		volume        *volumeStruct
		volumeAsValue sortedmap.Value
		volumeIndex   int
	)

	numVolumes, err = globals.volumeLLRB.Len()
	if nil != err {
		err = fmt.Errorf("globals.volumeLLRB.Len() failed: %v", err)
		return
	}
	for volumeIndex = 0; volumeIndex < numVolumes; volumeIndex++ {
		_, volumeAsValue, ok, err = globals.volumeLLRB.GetByIndex(volumeIndex)
		if nil != err {
			err = fmt.Errorf("globals.volumeLLRB.GetByIndex(%v) failed: %v", volumeIndex, err)
			return
		}
		if !ok {
			err = fmt.Errorf("globals.volumeLLRB.GetByIndex(%v) returned ok == false", volumeIndex)
			return
		}
		volume, ok = volumeAsValue.(*volumeStruct)
		if !ok {
			err = fmt.Errorf("volumeAsValue.(*volumeStruct) for index %v returned ok == false", volumeIndex)
			return
		}
		volume.Lock()
		if nil != volume.fsckActiveJob {
			volume.fsckActiveJob.jobHandle.Cancel()
			volume.fsckActiveJob.state = jobHalted
			volume.fsckActiveJob.endTime = time.Now()
			volume.fsckActiveJob = nil
		}
		if nil != volume.scrubActiveJob {
			volume.scrubActiveJob.jobHandle.Cancel()
			volume.scrubActiveJob.state = jobHalted
			volume.scrubActiveJob.endTime = time.Now()
			volume.scrubActiveJob = nil
		}
		volume.Unlock()
	}

	err = nil
	return
}
