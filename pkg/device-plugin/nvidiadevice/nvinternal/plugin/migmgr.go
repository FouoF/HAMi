/*
 * Copyright (c) 2026, HAMi.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package plugin

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"

	"github.com/Project-HAMi/HAMi/pkg/device"
)

var profileNameToGIProfileID = map[string]int{
	"1g": nvml.GPU_INSTANCE_PROFILE_1_SLICE,
	"2g": nvml.GPU_INSTANCE_PROFILE_2_SLICE,
	"3g": nvml.GPU_INSTANCE_PROFILE_3_SLICE,
	"4g": nvml.GPU_INSTANCE_PROFILE_4_SLICE,
	"6g": nvml.GPU_INSTANCE_PROFILE_6_SLICE,
	"7g": nvml.GPU_INSTANCE_PROFILE_7_SLICE,
	"8g": nvml.GPU_INSTANCE_PROFILE_8_SLICE,
}

var profileNameToCIProfileID = map[string]int{
	"1g": nvml.COMPUTE_INSTANCE_PROFILE_1_SLICE,
	"2g": nvml.COMPUTE_INSTANCE_PROFILE_2_SLICE,
	"3g": nvml.COMPUTE_INSTANCE_PROFILE_3_SLICE,
	"4g": nvml.COMPUTE_INSTANCE_PROFILE_4_SLICE,
	"6g": nvml.COMPUTE_INSTANCE_PROFILE_6_SLICE,
	"7g": nvml.COMPUTE_INSTANCE_PROFILE_7_SLICE,
	"8g": nvml.COMPUTE_INSTANCE_PROFILE_8_SLICE,
}

// slotKey identifies a scheduler-allocated MIG slot: a specific position
// within a specific geometry template on a specific physical GPU. This is the
// identifier HAMi's scheduler uses (encoded as UUID[templateIdx-positionIdx])
// and is stable across destroy/recreate cycles.
type slotKey struct {
	GPUIndex    int
	TemplateIdx int
	PositionIdx int
}

// migInstance tracks the nvml-level identity of a MIG GI+CI pair bound to a
// slot. Absent means the slot's GI+CI have been destroyed (e.g. on task end)
// but we remember the profile and placement so we can recreate the instance
// at the same physical slice when the next task claims this slot.
type migInstance struct {
	Profile   string // slice group, e.g. "1g"
	Placement nvml.GpuInstancePlacement
	Present   bool
	GIID      uint32
	CIID      uint32
	MigUUID   string
}

// MigInstanceManager is the single authority over live MIG GI+CI state on a
// node. RebuildForGPU populates slot->instance mapping after a full
// nvidia-mig-parted apply; EnsureSlot creates on demand; Release destroys and
// marks the slot absent without losing its profile/placement.
type MigInstanceManager struct {
	mu        sync.Mutex
	gpuLocks  map[int]*sync.Mutex
	bySlot    map[slotKey]*migInstance
	byMigUUID map[string]slotKey
}

func NewMigInstanceManager() *MigInstanceManager {
	return &MigInstanceManager{
		gpuLocks:  make(map[int]*sync.Mutex),
		bySlot:    make(map[slotKey]*migInstance),
		byMigUUID: make(map[string]slotKey),
	}
}

func (m *MigInstanceManager) gpuLock(gpuIndex int) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lk, ok := m.gpuLocks[gpuIndex]
	if !ok {
		lk = &sync.Mutex{}
		m.gpuLocks[gpuIndex] = lk
	}
	return lk
}

func profileSliceKey(profile string) string {
	if idx := strings.Index(profile, "."); idx > 0 {
		return profile[:idx]
	}
	return profile
}

// RebuildForGPU reconstructs the slot map for a GPU that has just had its
// geometry applied. The geometry argument is the ordered list of profile
// entries from knownMigGeometries[templateIdx]; live nvml instances are
// enumerated, grouped by profile, and assigned to slots in declaration order.
//
// This is called right after ApplyMigTemplate when HAMi knows which template
// the card is shaped under. Callers must pass the same templateIdx the
// scheduler encoded in the allocation UUID.
func (m *MigInstanceManager) RebuildForGPU(gpuIndex, templateIdx int, geometry device.Geometry) error {
	lk := m.gpuLock(gpuIndex)
	lk.Lock()
	defer lk.Unlock()

	dev, ret := nvml.DeviceGetHandleByIndex(gpuIndex)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml get handle by index %d: %s", gpuIndex, nvml.ErrorString(ret))
	}

	// Forget any prior entries for this GPU.
	m.mu.Lock()
	for uuid, key := range m.byMigUUID {
		if key.GPUIndex == gpuIndex {
			delete(m.byMigUUID, uuid)
		}
	}
	for key := range m.bySlot {
		if key.GPUIndex == gpuIndex {
			delete(m.bySlot, key)
		}
	}
	m.mu.Unlock()

	giIDToMigUUID := enumerateMigDevicesByGI(dev)

	// Bucket live instances by profile slice key.
	liveByProfile := make(map[string][]liveInstance)
	for _, giProfileID := range []int{
		nvml.GPU_INSTANCE_PROFILE_1_SLICE,
		nvml.GPU_INSTANCE_PROFILE_2_SLICE,
		nvml.GPU_INSTANCE_PROFILE_3_SLICE,
		nvml.GPU_INSTANCE_PROFILE_4_SLICE,
		nvml.GPU_INSTANCE_PROFILE_6_SLICE,
		nvml.GPU_INSTANCE_PROFILE_7_SLICE,
		nvml.GPU_INSTANCE_PROFILE_8_SLICE,
	} {
		info, ret := dev.GetGpuInstanceProfileInfo(giProfileID)
		if ret != nvml.SUCCESS {
			continue
		}
		gis, ret := dev.GetGpuInstances(&info)
		if ret != nvml.SUCCESS || len(gis) == 0 {
			continue
		}
		for _, gi := range gis {
			giInfo, ret := gi.GetInfo()
			if ret != nvml.SUCCESS {
				continue
			}
			ciInfo, ret := gi.GetComputeInstanceProfileInfo(profileIDToCIProfileID(giProfileID), nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
			if ret != nvml.SUCCESS {
				continue
			}
			cis, ret := gi.GetComputeInstances(&ciInfo)
			if ret != nvml.SUCCESS || len(cis) == 0 {
				continue
			}
			ciData, ret := cis[0].GetInfo()
			if ret != nvml.SUCCESS {
				continue
			}
			migUUID, ok := giIDToMigUUID[giInfo.Id]
			if !ok {
				continue
			}
			profileKey := giProfileIDToSliceKey(giProfileID)
			liveByProfile[profileKey] = append(liveByProfile[profileKey], liveInstance{
				GIID:      giInfo.Id,
				CIID:      ciData.Id,
				Placement: giInfo.Placement,
				MigUUID:   migUUID,
			})
		}
	}
	// Stable sort each bucket by placement start so slot assignment is
	// reproducible across restarts.
	for k := range liveByProfile {
		sort.Slice(liveByProfile[k], func(i, j int) bool {
			return liveByProfile[k][i].Placement.Start < liveByProfile[k][j].Placement.Start
		})
	}

	// Walk the geometry in declaration order, assigning live instances to
	// slots. Any slot without a live instance is recorded as absent with
	// placement inferred from possible-placement enumeration on first use.
	posIdx := 0
	for _, tmpl := range geometry {
		sliceKey := profileSliceKey(tmpl.Name)
		for c := int32(0); c < tmpl.Count; c++ {
			key := slotKey{GPUIndex: gpuIndex, TemplateIdx: templateIdx, PositionIdx: posIdx}
			posIdx++
			var inst *migInstance
			if bucket := liveByProfile[sliceKey]; len(bucket) > 0 {
				li := bucket[0]
				liveByProfile[sliceKey] = bucket[1:]
				inst = &migInstance{
					Profile:   sliceKey,
					Placement: li.Placement,
					Present:   true,
					GIID:      li.GIID,
					CIID:      li.CIID,
					MigUUID:   li.MigUUID,
				}
				m.mu.Lock()
				m.byMigUUID[li.MigUUID] = key
				m.mu.Unlock()
			} else {
				inst = &migInstance{
					Profile: sliceKey,
					Present: false,
				}
			}
			m.mu.Lock()
			m.bySlot[key] = inst
			m.mu.Unlock()
		}
	}
	return nil
}

// EnsurePrimed calls RebuildForGPU if this (gpuIndex, templateIdx) pair has no
// slots in the map yet. Used to populate the slot map lazily on the first
// allocation after a plugin restart, without re-running a full rebuild on
// every allocation.
func (m *MigInstanceManager) EnsurePrimed(gpuIndex, templateIdx int, geometry device.Geometry) error {
	m.mu.Lock()
	primed := false
	for k := range m.bySlot {
		if k.GPUIndex == gpuIndex && k.TemplateIdx == templateIdx {
			primed = true
			break
		}
	}
	m.mu.Unlock()
	if primed {
		return nil
	}
	return m.RebuildForGPU(gpuIndex, templateIdx, geometry)
}

// PrepareGPU readies a GPU for on-demand slot creation under the given
// template, replacing the "full nvidia-mig-parted apply at first allocation"
// dance that resharded the entire card. It:
//
//   - Enables MIG mode on the card if not already enabled.
//   - Refuses the allocation (ErrTemplateConflict) if the card currently has
//     Present slots under a different templateIdx — the scheduler is supposed
//     to prevent this, but we defend at the boundary so we never destroy a
//     running task's GPU instance underfoot.
//   - Discards any absent-slot bookkeeping from a prior, now-idle template.
//   - Seeds the slot map for (gpuIndex, templateIdx) by adopting live GIs
//     that match the geometry (plugin-restart case) or starting empty (fresh
//     card case). Subsequent EnsureSlot calls create GIs on demand.
func (m *MigInstanceManager) PrepareGPU(gpuIndex, templateIdx int, geometry device.Geometry) error {
	dev, ret := nvml.DeviceGetHandleByIndex(gpuIndex)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml get handle by index %d: %s", gpuIndex, nvml.ErrorString(ret))
	}

	lk := m.gpuLock(gpuIndex)
	lk.Lock()
	if err := ensureMigModeEnabled(dev); err != nil {
		lk.Unlock()
		return err
	}

	// Scan current slot state for conflicts and for stale bookkeeping.
	m.mu.Lock()
	primedSameTmpl := false
	hasOtherTmpl := false
	otherTmplInUse := false
	for k, inst := range m.bySlot {
		if k.GPUIndex != gpuIndex {
			continue
		}
		if k.TemplateIdx == templateIdx {
			primedSameTmpl = true
			continue
		}
		hasOtherTmpl = true
		if inst.Present {
			otherTmplInUse = true
		}
	}
	m.mu.Unlock()

	if otherTmplInUse {
		lk.Unlock()
		return fmt.Errorf("gpu %d has live MIG instances under a different template; refusing template switch", gpuIndex)
	}
	if primedSameTmpl {
		lk.Unlock()
		return nil
	}
	if hasOtherTmpl {
		// Prior template is idle — drop its absent slots so the new template's
		// slot map can be seeded cleanly, and destroy any live GIs that belong
		// to the old shape before we adopt the new one.
		m.mu.Lock()
		for k := range m.bySlot {
			if k.GPUIndex == gpuIndex {
				delete(m.bySlot, k)
			}
		}
		for uuid, k := range m.byMigUUID {
			if k.GPUIndex == gpuIndex {
				delete(m.byMigUUID, uuid)
			}
		}
		m.mu.Unlock()
		if err := destroyAllMigInstances(dev); err != nil {
			lk.Unlock()
			return err
		}
	}
	lk.Unlock()

	// RebuildForGPU takes the gpuLock itself; call it without holding it here.
	// It adopts any remaining live GIs that fit the geometry, or seeds an
	// all-absent slot map on an unpartitioned card.
	return m.RebuildForGPU(gpuIndex, templateIdx, geometry)
}

// ensureMigModeEnabled turns on MIG mode via NVML when the card is currently
// in non-MIG mode. No-op when MIG mode is unsupported (non-MIG cards) so the
// caller can invoke it uniformly.
func ensureMigModeEnabled(dev nvml.Device) error {
	curMode, _, ret := dev.GetMigMode()
	if ret == nvml.ERROR_NOT_SUPPORTED {
		return nil
	}
	if ret != nvml.SUCCESS {
		return fmt.Errorf("get mig mode: %s", nvml.ErrorString(ret))
	}
	if curMode == nvml.DEVICE_MIG_ENABLE {
		return nil
	}
	if _, ret := dev.SetMigMode(nvml.DEVICE_MIG_ENABLE); ret != nvml.SUCCESS {
		return fmt.Errorf("enable mig mode: %s", nvml.ErrorString(ret))
	}
	return nil
}

// destroyAllMigInstances enumerates and destroys every GI+CI on the device.
// Used on template switches when no scheduler-allocated slot is in use.
func destroyAllMigInstances(dev nvml.Device) error {
	for _, giProfileID := range []int{
		nvml.GPU_INSTANCE_PROFILE_1_SLICE,
		nvml.GPU_INSTANCE_PROFILE_2_SLICE,
		nvml.GPU_INSTANCE_PROFILE_3_SLICE,
		nvml.GPU_INSTANCE_PROFILE_4_SLICE,
		nvml.GPU_INSTANCE_PROFILE_6_SLICE,
		nvml.GPU_INSTANCE_PROFILE_7_SLICE,
		nvml.GPU_INSTANCE_PROFILE_8_SLICE,
	} {
		info, ret := dev.GetGpuInstanceProfileInfo(giProfileID)
		if ret != nvml.SUCCESS {
			continue
		}
		gis, ret := dev.GetGpuInstances(&info)
		if ret != nvml.SUCCESS {
			continue
		}
		for _, gi := range gis {
			ciInfoRet := profileIDToCIProfileID(giProfileID)
			if ciInfo, r := gi.GetComputeInstanceProfileInfo(ciInfoRet, nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED); r == nvml.SUCCESS {
				if cis, r2 := gi.GetComputeInstances(&ciInfo); r2 == nvml.SUCCESS {
					for _, ci := range cis {
						if d := ci.Destroy(); d != nvml.SUCCESS {
							return fmt.Errorf("destroy compute instance: %s", nvml.ErrorString(d))
						}
					}
				}
			}
			if d := gi.Destroy(); d != nvml.SUCCESS {
				return fmt.Errorf("destroy gpu instance: %s", nvml.ErrorString(d))
			}
		}
	}
	return nil
}

// Release destroys the GI+CI bound to the given MIG UUID and marks the slot
// absent (preserving its profile and placement). Invoked by the podresources
// watcher when kubelet reports the device is no longer in use.
func (m *MigInstanceManager) Release(migUUID string) error {
	m.mu.Lock()
	key, ok := m.byMigUUID[migUUID]
	m.mu.Unlock()
	if !ok {
		klog.V(5).InfoS("release: unknown MIG UUID, skipping", "uuid", migUUID)
		return nil
	}

	lk := m.gpuLock(key.GPUIndex)
	lk.Lock()
	defer lk.Unlock()

	m.mu.Lock()
	inst, ok := m.bySlot[key]
	m.mu.Unlock()
	if !ok || !inst.Present {
		return nil
	}

	dev, ret := nvml.DeviceGetHandleByIndex(key.GPUIndex)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml get handle by index %d: %s", key.GPUIndex, nvml.ErrorString(ret))
	}
	gi, ret := dev.GetGpuInstanceById(int(inst.GIID))
	if ret == nvml.SUCCESS {
		if ci, r := gi.GetComputeInstanceById(int(inst.CIID)); r == nvml.SUCCESS {
			if d := ci.Destroy(); d != nvml.SUCCESS {
				return fmt.Errorf("destroy CI %d: %s", inst.CIID, nvml.ErrorString(d))
			}
		}
		if d := gi.Destroy(); d != nvml.SUCCESS {
			return fmt.Errorf("destroy GI %d: %s", inst.GIID, nvml.ErrorString(d))
		}
	}

	m.mu.Lock()
	inst.Present = false
	inst.GIID = 0
	inst.CIID = 0
	oldUUID := inst.MigUUID
	inst.MigUUID = ""
	delete(m.byMigUUID, oldUUID)
	m.mu.Unlock()
	klog.InfoS("released MIG instance", "uuid", migUUID, "gpu", key.GPUIndex, "slot", key.PositionIdx)
	return nil
}

// EnsureSlot returns the MIG UUID for the given slot, creating the underlying
// GI+CI on demand if the slot is currently absent. Called from the device
// plugin Allocate path before the MIG UUID is handed back to the container.
func (m *MigInstanceManager) EnsureSlot(gpuIndex, templateIdx, positionIdx int) (string, error) {
	key := slotKey{GPUIndex: gpuIndex, TemplateIdx: templateIdx, PositionIdx: positionIdx}
	m.mu.Lock()
	inst, ok := m.bySlot[key]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("slot %+v not known; was RebuildForGPU called?", key)
	}
	if inst.Present {
		return inst.MigUUID, nil
	}

	lk := m.gpuLock(gpuIndex)
	lk.Lock()
	defer lk.Unlock()

	// Re-check under lock.
	m.mu.Lock()
	inst = m.bySlot[key]
	m.mu.Unlock()
	if inst.Present {
		return inst.MigUUID, nil
	}

	giProfileID, ok := profileNameToGIProfileID[inst.Profile]
	if !ok {
		return "", fmt.Errorf("unsupported MIG profile %q", inst.Profile)
	}
	ciProfileID, ok := profileNameToCIProfileID[inst.Profile]
	if !ok {
		return "", fmt.Errorf("unsupported MIG CI profile %q", inst.Profile)
	}
	dev, ret := nvml.DeviceGetHandleByIndex(gpuIndex)
	if ret != nvml.SUCCESS {
		return "", fmt.Errorf("nvml get handle by index %d: %s", gpuIndex, nvml.ErrorString(ret))
	}
	giInfo, ret := dev.GetGpuInstanceProfileInfo(giProfileID)
	if ret != nvml.SUCCESS {
		return "", fmt.Errorf("get GI profile info: %s", nvml.ErrorString(ret))
	}

	placement := inst.Placement
	if placement.Size == 0 {
		// Slot was never realised (absent from the start) — pick a free
		// placement by enumerating possible placements and subtracting the
		// ones currently in use on this GPU.
		chosen, err := pickFreePlacement(dev, &giInfo, m.placementsInUse(gpuIndex))
		if err != nil {
			return "", err
		}
		placement = chosen
	}

	gi, ret := dev.CreateGpuInstanceWithPlacement(&giInfo, &placement)
	if ret != nvml.SUCCESS {
		return "", fmt.Errorf("create GI (profile=%s, start=%d): %s", inst.Profile, placement.Start, nvml.ErrorString(ret))
	}
	giData, ret := gi.GetInfo()
	if ret != nvml.SUCCESS {
		gi.Destroy()
		return "", fmt.Errorf("get GI info: %s", nvml.ErrorString(ret))
	}
	ciInfo, ret := gi.GetComputeInstanceProfileInfo(ciProfileID, nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
	if ret != nvml.SUCCESS {
		gi.Destroy()
		return "", fmt.Errorf("get CI profile info: %s", nvml.ErrorString(ret))
	}
	ci, ret := gi.CreateComputeInstance(&ciInfo)
	if ret != nvml.SUCCESS {
		gi.Destroy()
		return "", fmt.Errorf("create CI: %s", nvml.ErrorString(ret))
	}
	ciData, ret := ci.GetInfo()
	if ret != nvml.SUCCESS {
		ci.Destroy()
		gi.Destroy()
		return "", fmt.Errorf("get CI info: %s", nvml.ErrorString(ret))
	}
	migUUID, err := findMigUUIDForGI(dev, giData.Id)
	if err != nil {
		ci.Destroy()
		gi.Destroy()
		return "", err
	}

	m.mu.Lock()
	inst.Present = true
	inst.GIID = giData.Id
	inst.CIID = ciData.Id
	inst.Placement = placement
	inst.MigUUID = migUUID
	m.byMigUUID[migUUID] = key
	m.mu.Unlock()
	klog.InfoS("recreated MIG instance", "uuid", migUUID, "gpu", gpuIndex, "profile", inst.Profile, "placement", placement.Start, "gi", inst.GIID, "ci", inst.CIID)
	return migUUID, nil
}

// SlotMigUUID returns the currently-bound MIG UUID for a slot without
// creating one. Returns "" if the slot is absent or unknown.
func (m *MigInstanceManager) SlotMigUUID(gpuIndex, templateIdx, positionIdx int) string {
	key := slotKey{GPUIndex: gpuIndex, TemplateIdx: templateIdx, PositionIdx: positionIdx}
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.bySlot[key]
	if !ok || !inst.Present {
		return ""
	}
	return inst.MigUUID
}

func (m *MigInstanceManager) placementsInUse(gpuIndex int) map[uint32]uint32 {
	out := make(map[uint32]uint32)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, inst := range m.bySlot {
		if key.GPUIndex != gpuIndex || !inst.Present {
			continue
		}
		out[inst.Placement.Start] = inst.Placement.Size
	}
	return out
}

type liveInstance struct {
	GIID      uint32
	CIID      uint32
	Placement nvml.GpuInstancePlacement
	MigUUID   string
}

func enumerateMigDevicesByGI(dev nvml.Device) map[uint32]string {
	out := make(map[uint32]string)
	maxCount, ret := dev.GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		return out
	}
	for i := 0; i < maxCount; i++ {
		migDev, ret := dev.GetMigDeviceHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}
		giID, ret := migDev.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			continue
		}
		uuid, ret := migDev.GetUUID()
		if ret != nvml.SUCCESS {
			continue
		}
		out[uint32(giID)] = uuid
	}
	return out
}

func findMigUUIDForGI(dev nvml.Device, giID uint32) (string, error) {
	maxCount, ret := dev.GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		return "", fmt.Errorf("get max MIG device count: %s", nvml.ErrorString(ret))
	}
	for i := 0; i < maxCount; i++ {
		migDev, ret := dev.GetMigDeviceHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}
		gotGI, ret := migDev.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			continue
		}
		if uint32(gotGI) == giID {
			uuid, ret := migDev.GetUUID()
			if ret != nvml.SUCCESS {
				return "", fmt.Errorf("get MIG UUID: %s", nvml.ErrorString(ret))
			}
			return uuid, nil
		}
	}
	return "", fmt.Errorf("no MIG device found for GI %d", giID)
}

// pickFreePlacement returns a placement for the given GI profile that does
// not overlap with any of the placements already in use on this GPU.
func pickFreePlacement(dev nvml.Device, info *nvml.GpuInstanceProfileInfo, inUse map[uint32]uint32) (nvml.GpuInstancePlacement, error) {
	possible, ret := dev.GetGpuInstancePossiblePlacements(info)
	if ret != nvml.SUCCESS {
		return nvml.GpuInstancePlacement{}, fmt.Errorf("get possible placements: %s", nvml.ErrorString(ret))
	}
	for _, p := range possible {
		if !placementOverlaps(p, inUse) {
			return p, nil
		}
	}
	return nvml.GpuInstancePlacement{}, fmt.Errorf("no free placement for profile")
}

func placementOverlaps(p nvml.GpuInstancePlacement, inUse map[uint32]uint32) bool {
	for start, size := range inUse {
		if p.Start < start+size && start < p.Start+p.Size {
			return true
		}
	}
	return false
}

func profileIDToCIProfileID(giProfileID int) int {
	switch giProfileID {
	case nvml.GPU_INSTANCE_PROFILE_1_SLICE:
		return nvml.COMPUTE_INSTANCE_PROFILE_1_SLICE
	case nvml.GPU_INSTANCE_PROFILE_2_SLICE:
		return nvml.COMPUTE_INSTANCE_PROFILE_2_SLICE
	case nvml.GPU_INSTANCE_PROFILE_3_SLICE:
		return nvml.COMPUTE_INSTANCE_PROFILE_3_SLICE
	case nvml.GPU_INSTANCE_PROFILE_4_SLICE:
		return nvml.COMPUTE_INSTANCE_PROFILE_4_SLICE
	case nvml.GPU_INSTANCE_PROFILE_6_SLICE:
		return nvml.COMPUTE_INSTANCE_PROFILE_6_SLICE
	case nvml.GPU_INSTANCE_PROFILE_7_SLICE:
		return nvml.COMPUTE_INSTANCE_PROFILE_7_SLICE
	case nvml.GPU_INSTANCE_PROFILE_8_SLICE:
		return nvml.COMPUTE_INSTANCE_PROFILE_8_SLICE
	}
	return nvml.COMPUTE_INSTANCE_PROFILE_1_SLICE
}

func giProfileIDToSliceKey(giProfileID int) string {
	switch giProfileID {
	case nvml.GPU_INSTANCE_PROFILE_1_SLICE:
		return "1g"
	case nvml.GPU_INSTANCE_PROFILE_2_SLICE:
		return "2g"
	case nvml.GPU_INSTANCE_PROFILE_3_SLICE:
		return "3g"
	case nvml.GPU_INSTANCE_PROFILE_4_SLICE:
		return "4g"
	case nvml.GPU_INSTANCE_PROFILE_6_SLICE:
		return "6g"
	case nvml.GPU_INSTANCE_PROFILE_7_SLICE:
		return "7g"
	case nvml.GPU_INSTANCE_PROFILE_8_SLICE:
		return "8g"
	}
	return ""
}
