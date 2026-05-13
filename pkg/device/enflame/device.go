/*
Copyright 2024 The HAMi Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package enflame

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/common"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type EnflameDevices struct{}

const (
	EnflameVGCUDevice     = "Enflame"
	EnflameVGCUCommonWord = "Enflame"
	// EnflameUseUUID annotation specifies a comma-separated list of Enflame UUIDs to use.
	EnflameUseUUID = "enflame.com/use-gpuuuid"
	// EnflameNoUseUUID annotation specifies a comma-separated list of Enflame UUIDs to exclude.
	EnflameNoUseUUID   = "enflame.com/nouse-gpuuuid"
	PodRequestGCUSize  = "enflame.com/gcu-request-size"
	PodAssignedGCUID   = "enflame.com/gcu-assigned-id"
	PodHasAssignedGCU  = "enflame.com/gcu-assigned"
	PodAssignedGCUIdx  = "enflame.com/gcu-assigned-index"
	PodAssignedGCUMin  = "enflame.com/gcu-assigned-minor"
	PodAssignedGCUTime = "enflame.com/gcu-assigned-time"
	AssignedContainers = "assigned-containers"
	GCUDrsCapacity     = "enflame.com/gcu-drs-capacity"

	SharedResourceName = "enflame.com/shared-gcu"
	CountNoSharedName  = "enflame.com/gcu-count"
)

type drsCapacitySpec struct {
	Devices  []drsDeviceSpec   `json:"devices"`
	Profiles map[string]string `json:"profiles"`
}

type drsDeviceSpec struct {
	Index    string `json:"index"`
	Minor    string `json:"minor"`
	Capacity any    `json:"capacity"`
}

type assignedContainerInfo struct {
	Allocated    bool   `json:"allocated"`
	Request      int32  `json:"request"`
	ProfileID    string `json:"profileID,omitempty"`
	ProfileName  string `json:"profileName,omitempty"`
	InstanceID   string `json:"instanceID,omitempty"`
	InstanceUUID string `json:"instanceUUID,omitempty"`
}

func InitEnflameDevice(config EnflameConfig) *EnflameDevices {
	EnflameResourceNameDRSGCU = config.ResourceNameDRSGCU
	if EnflameResourceNameDRSGCU == "" {
		EnflameResourceNameDRSGCU = config.ResourceNameVGCU
	}
	if EnflameResourceNameDRSGCU == "" {
		EnflameResourceNameDRSGCU = "enflame.com/drs-gcu"
	}
	EnflameResourceNameVGCU = config.ResourceNameVGCU
	EnflameResourceNameVGCUPercentage = config.ResourceNameVGCUPercentage
	_, ok := device.SupportDevices[EnflameVGCUDevice]
	if !ok {
		device.SupportDevices[EnflameVGCUDevice] = "hami.io/enflame-vgpu-devices-allocated"
	}
	return &EnflameDevices{}
}

func (dev *EnflameDevices) CommonWord() string {
	return EnflameVGCUCommonWord
}

func (dev *EnflameDevices) MutateAdmission(ctr *corev1.Container, p *corev1.Pod) (bool, error) {
	resourceCount := corev1.ResourceName(EnflameResourceNameDRSGCU)
	count, ok := ctr.Resources.Limits[resourceCount]
	if ok {
		if count.Value() <= 0 {
			return false, fmt.Errorf("%s must be greater than 0", EnflameResourceNameDRSGCU)
		}
	} else {
		count, ok = ctr.Resources.Requests[resourceCount]
		if !ok {
			return false, nil
		}
		if count.Value() <= 0 {
			return false, fmt.Errorf("%s must be greater than 0", EnflameResourceNameDRSGCU)
		}
	}
	if ctr.Resources.Limits == nil {
		ctr.Resources.Limits = corev1.ResourceList{}
	}
	if ctr.Resources.Requests == nil {
		ctr.Resources.Requests = corev1.ResourceList{}
	}
	if _, exists := ctr.Resources.Limits[resourceCount]; !exists {
		ctr.Resources.Limits[resourceCount] = count
	}
	if _, exists := ctr.Resources.Requests[resourceCount]; !exists {
		ctr.Resources.Requests[resourceCount] = count
	}
	return true, nil
}

func (dev *EnflameDevices) GetNodeDevices(n corev1.Node) ([]*device.DeviceInfo, error) {
	capacityRaw, ok := n.Annotations[GCUDrsCapacity]
	if !ok {
		return []*device.DeviceInfo{}, fmt.Errorf("annotation not found %s", GCUDrsCapacity)
	}
	spec := drsCapacitySpec{}
	if err := json.Unmarshal([]byte(capacityRaw), &spec); err != nil {
		return []*device.DeviceInfo{}, fmt.Errorf("failed to parse %s: %w", GCUDrsCapacity, err)
	}
	nodedevices := make([]*device.DeviceInfo, 0, len(spec.Devices))
	for idx, d := range spec.Devices {
		devIndex, err := strconv.Atoi(d.Index)
		if err != nil {
			devIndex = idx
		}
		capacity, err := parseDRSCapacity(d.Capacity)
		if err != nil || capacity <= 0 {
			return []*device.DeviceInfo{}, fmt.Errorf("invalid drs capacity on node %s", n.Name)
		}
		minor := strings.TrimSpace(d.Minor)
		if minor == "" {
			minor = strconv.Itoa(devIndex)
		}
		profiles := map[string]string{}
		for name, profileID := range spec.Profiles {
			profiles[name] = profileID
		}
		nodedevices = append(nodedevices, &device.DeviceInfo{
			Index:        uint(devIndex),
			ID:           fmt.Sprintf("%s-enflame-drs-%d", n.Name, devIndex),
			Count:        capacity,
			Devmem:       capacity,
			Devcore:      100,
			Type:         EnflameVGCUDevice,
			Numa:         0,
			Health:       true,
			DeviceVendor: EnflameVGCUCommonWord,
			CustomInfo: map[string]any{
				"minor":    minor,
				"index":    strconv.Itoa(devIndex),
				"profiles": profiles,
			},
		})
	}
	if len(nodedevices) == 0 {
		return []*device.DeviceInfo{}, fmt.Errorf("no drs devices found on node %s", n.Name)
	}
	return nodedevices, nil
}

func (dev *EnflameDevices) PatchAnnotations(pod *corev1.Pod, annoinput *map[string]string, pd device.PodDevices) map[string]string {
	devlist, ok := pd[EnflameVGCUDevice]
	if ok && len(devlist) > 0 {
		(*annoinput)[device.SupportDevices[EnflameVGCUDevice]] = device.EncodePodSingleDevice(devlist)
		(*annoinput)[PodHasAssignedGCU] = "false"
		(*annoinput)[PodAssignedGCUTime] = strconv.FormatInt(time.Now().UnixNano(), 10)

		assigned := map[string]assignedContainerInfo{}
		for ctridx, ctrDevices := range devlist {
			if len(ctrDevices) == 0 {
				continue
			}
			chosen := ctrDevices[0]
			ctrName := containerNameByIndex(pod, ctridx)
			profileName := readCustomInfoString(chosen.CustomInfo, "profileName")
			profileID := readCustomInfoString(chosen.CustomInfo, "profileID")
			assigned[ctrName] = assignedContainerInfo{
				Allocated:   false,
				Request:     chosen.Usedmem,
				ProfileID:   profileID,
				ProfileName: profileName,
			}

			if _, exists := (*annoinput)[PodAssignedGCUIdx]; !exists {
				if index := readCustomInfoString(chosen.CustomInfo, "index"); index != "" {
					(*annoinput)[PodAssignedGCUIdx] = index
					(*annoinput)[PodAssignedGCUID] = index
				}
			}
			if _, exists := (*annoinput)[PodAssignedGCUMin]; !exists {
				if minor := readCustomInfoString(chosen.CustomInfo, "minor"); minor != "" {
					(*annoinput)[PodAssignedGCUMin] = minor
				}
			}
			if _, exists := (*annoinput)[PodRequestGCUSize]; !exists && chosen.Usedmem > 0 {
				(*annoinput)[PodRequestGCUSize] = strconv.FormatInt(int64(chosen.Usedmem), 10)
			}
		}
		if len(assigned) > 0 {
			if payload, err := json.Marshal(assigned); err != nil {
				klog.ErrorS(err, "failed to marshal assigned containers", "pod", klog.KObj(pod))
			} else {
				(*annoinput)[AssignedContainers] = string(payload)
			}
		}
	}
	return *annoinput
}

func (dev *EnflameDevices) LockNode(n *corev1.Node, p *corev1.Pod) error {
	return nil
}

func (dev *EnflameDevices) ReleaseNodeLock(n *corev1.Node, p *corev1.Pod) error {
	return nil
}

func (dev *EnflameDevices) NodeCleanUp(nn string) error {
	return nil
}

func (dev *EnflameDevices) checkType(annos map[string]string, d device.DeviceUsage, n device.ContainerDeviceRequest) (bool, bool, bool) {
	if strings.Compare(n.Type, EnflameVGCUDevice) == 0 {
		return true, true, false
	}
	return false, false, false
}

func (dev *EnflameDevices) CheckHealth(devType string, n *corev1.Node) (bool, bool) {
	return true, true
}

func (dev *EnflameDevices) GenerateResourceRequests(ctr *corev1.Container) device.ContainerDeviceRequest {
	klog.Info("Start to count enflame devices for container ", ctr.Name)
	resourceCount := corev1.ResourceName(EnflameResourceNameDRSGCU)
	v, ok := ctr.Resources.Limits[resourceCount]
	if !ok {
		v, ok = ctr.Resources.Requests[resourceCount]
	}
	if ok {
		if n, ok := v.AsInt64(); ok && n > 0 {
			klog.Info("Found enflame devices")
			if n > math.MaxInt32 {
				klog.ErrorS(nil, "drs request is too large", "container", ctr.Name, "request", n)
				return device.ContainerDeviceRequest{}
			}
			return device.ContainerDeviceRequest{
				Nums:             1,
				Type:             EnflameVGCUDevice,
				Memreq:           int32(n),
				MemPercentagereq: 0,
				Coresreq:         0,
			}
		}
	}
	return device.ContainerDeviceRequest{}
}

func (dev *EnflameDevices) ScoreNode(node *corev1.Node, podDevices device.PodSingleDevice, previous []*device.DeviceUsage, policy string) float32 {
	return 0
}

func (dev *EnflameDevices) AddResourceUsage(pod *corev1.Pod, n *device.DeviceUsage, ctr *device.ContainerDevice) error {
	n.Used++
	n.Usedcores += ctr.Usedcores
	n.Usedmem += ctr.Usedmem
	return nil
}

func (enf *EnflameDevices) Fit(devices []*device.DeviceUsage, request device.ContainerDeviceRequest, pod *corev1.Pod, nodeInfo *device.NodeInfo, allocated *device.PodDevices) (bool, map[string]device.ContainerDevices, string) {
	k := request
	originReq := k.Nums
	klog.InfoS("Allocating device for container request", "pod", klog.KObj(pod), "card request", k)
	tmpDevs := make(map[string]device.ContainerDevices)
	reason := make(map[string]int)
	profileName, profileID, profileMatch := enf.selectProfileByRequest(devices, int(k.Memreq))
	if !profileMatch {
		reason[common.ModeNotFit]++
		return false, tmpDevs, common.GenReason(reason, len(devices))
	}
	for i := len(devices) - 1; i >= 0; i-- {
		dev := devices[i]
		klog.V(4).InfoS("scoring pod", "pod", klog.KObj(pod), "device", dev.ID, "Memreq", k.Memreq, "MemPercentagereq", k.MemPercentagereq, "Coresreq", k.Coresreq, "Nums", k.Nums, "device index", i)

		_, found, _ := enf.checkType(pod.GetAnnotations(), *dev, k)
		if !found {
			reason[common.CardTypeMismatch]++
			klog.V(5).InfoS(common.CardTypeMismatch, "pod", klog.KObj(pod), "device", dev.ID, dev.Type, k.Type)
			continue
		}
		if !device.CheckUUID(pod.GetAnnotations(), dev.ID, EnflameUseUUID, EnflameNoUseUUID, enf.CommonWord()) {
			reason[common.CardUUIDMismatch]++
			klog.V(5).InfoS(common.CardUUIDMismatch, "pod", klog.KObj(pod), "device", dev.ID, "current device info is:", *dev)
			continue
		}

		if k.Memreq <= 0 {
			reason[common.ModeNotFit]++
			continue
		}
		if dev.Count <= dev.Used {
			reason[common.CardTimeSlicingExhausted]++
			klog.V(5).InfoS(common.CardTimeSlicingExhausted, "pod", klog.KObj(pod), "device", dev.ID, "count", dev.Count, "used", dev.Used)
			continue
		}
		if dev.Totalmem-dev.Usedmem < k.Memreq {
			reason[common.CardInsufficientMemory]++
			klog.V(5).InfoS(common.CardInsufficientMemory, "pod", klog.KObj(pod), "device", dev.ID, "device index", i, "device total memory", dev.Totalmem, "device used memory", dev.Usedmem, "request memory", k.Memreq)
			continue
		}
		if k.Nums > 0 {
			klog.V(5).InfoS("find fit device", "pod", klog.KObj(pod), "device", dev.ID)
			k.Nums--
			tmpDevs[k.Type] = append(tmpDevs[k.Type], device.ContainerDevice{
				Idx:       int(dev.Index),
				UUID:      dev.ID,
				Type:      k.Type,
				Usedmem:   k.Memreq,
				Usedcores: 0,
				CustomInfo: map[string]any{
					"profileName": profileName,
					"profileID":   profileID,
					"minor":       readCustomInfoString(dev.CustomInfo, "minor"),
					"index":       readCustomInfoString(dev.CustomInfo, "index"),
				},
			})
		}
		if k.Nums == 0 {
			klog.V(4).InfoS("device allocate success", "pod", klog.KObj(pod), "allocate device", tmpDevs)
			return true, tmpDevs, ""
		}

	}
	if len(tmpDevs[k.Type]) > 0 {
		reason[common.AllocatedCardsInsufficientRequest] = len(tmpDevs[k.Type])
		klog.V(5).InfoS(common.AllocatedCardsInsufficientRequest, "pod", klog.KObj(pod), "request", originReq, "allocated", len(tmpDevs))
	}
	return false, tmpDevs, common.GenReason(reason, len(devices))
}

func (dev *EnflameDevices) GetResourceNames() device.ResourceNames {
	return device.ResourceNames{
		ResourceCountName:  EnflameResourceNameDRSGCU,
		ResourceMemoryName: "",
		ResourceCoreName:   "",
	}
}

func (dev *EnflameDevices) selectProfileByRequest(devices []*device.DeviceUsage, requestSize int) (string, string, bool) {
	if requestSize <= 0 {
		return "", "", false
	}
	matched := map[string]string{}
	for _, devUsage := range devices {
		profiles := parseProfilesFromCustomInfo(devUsage.CustomInfo)
		for profileName, profileID := range profiles {
			if parseProfileSize(profileName) == requestSize {
				matched[profileName] = profileID
			}
		}
	}
	if len(matched) == 0 {
		return "", "", false
	}
	names := make([]string, 0, len(matched))
	for name := range matched {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0], matched[names[0]], true
}

func parseProfilesFromCustomInfo(customInfo map[string]any) map[string]string {
	if customInfo == nil {
		return map[string]string{}
	}
	rawProfiles, ok := customInfo["profiles"]
	if !ok {
		return map[string]string{}
	}
	switch typed := rawProfiles.(type) {
	case map[string]string:
		return typed
	case map[string]any:
		res := map[string]string{}
		for name, profileID := range typed {
			profileIDStr, ok := profileID.(string)
			if !ok {
				continue
			}
			res[name] = profileIDStr
		}
		return res
	default:
		return map[string]string{}
	}
}

func parseProfileSize(profileName string) int {
	normalized := strings.ToLower(strings.TrimSpace(profileName))
	idx := strings.Index(normalized, "g")
	if idx <= 0 {
		return 0
	}
	size, err := strconv.Atoi(normalized[:idx])
	if err != nil {
		return 0
	}
	return size
}

func parseDRSCapacity(raw any) (int32, error) {
	switch typed := raw.(type) {
	case float64:
		return int32(typed), nil
	case int:
		return int32(typed), nil
	case int32:
		return typed, nil
	case int64:
		return int32(typed), nil
	case string:
		capacity, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, err
		}
		return int32(capacity), nil
	default:
		return 0, fmt.Errorf("unknown capacity type: %T", raw)
	}
}

func containerNameByIndex(pod *corev1.Pod, index int) string {
	if pod == nil {
		return fmt.Sprintf("container-%d", index)
	}
	initCount := len(pod.Spec.InitContainers)
	if index < initCount {
		return pod.Spec.InitContainers[index].Name
	}
	containerIdx := index - initCount
	if containerIdx >= 0 && containerIdx < len(pod.Spec.Containers) {
		return pod.Spec.Containers[containerIdx].Name
	}
	return fmt.Sprintf("container-%d", index)
}

func readCustomInfoString(customInfo map[string]any, key string) string {
	if customInfo == nil {
		return ""
	}
	raw, ok := customInfo[key]
	if !ok {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return typed
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.Itoa(int(typed))
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return fmt.Sprintf("%v", typed)
	}
}
