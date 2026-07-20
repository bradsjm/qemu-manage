package monitoring

import (
	"sync/atomic"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

type CollectorStatus string

const (
	CollectorOK          CollectorStatus = "ok"
	CollectorPending     CollectorStatus = "pending"
	CollectorUnsupported CollectorStatus = "unsupported"
	CollectorFailed      CollectorStatus = "failed"
	CollectorStale       CollectorStatus = "stale"
)

type CollectorState struct {
	Status      CollectorStatus
	Code        string
	ObservedAt  time.Time
	LastSuccess time.Time
	Duration    time.Duration
}

type VMIdentity struct {
	ID           string
	Name         string
	Backend      string
	Architecture string
	CPUs         int
	MemoryMiB    int
	GuestAgent   bool
	StartedAt    time.Time
	BuildVersion string
}

type QMPState struct {
	Version backend.QEMUVersion
	State   string
	Events  backend.QEMUEventCounters
	Blocks  []backend.QEMUBlockDevice
}

type ProcessState struct {
	PID   int
	Stats ProcessStats
}

type GuestState struct {
	Observation   backend.GuestObservation
	ProbeDuration time.Duration
	ProbeAt       time.Time
}

type Snapshot struct {
	ObservedAt time.Time
	VM         VMIdentity
	QMP        QMPState
	Process    ProcessState
	Guest      GuestState
	Collectors map[string]CollectorState
}

type snapshotStore struct {
	value atomic.Pointer[Snapshot]
}

func (s *snapshotStore) load() *Snapshot          { return s.value.Load() }
func (s *snapshotStore) store(snapshot *Snapshot) { s.value.Store(snapshot) }

func cloneSnapshot(source *Snapshot) *Snapshot {
	if source == nil {
		return &Snapshot{Collectors: make(map[string]CollectorState)}
	}
	clone := *source
	clone.Collectors = make(map[string]CollectorState, len(source.Collectors))
	for key, state := range source.Collectors {
		clone.Collectors[key] = state
	}
	clone.QMP.Events.Lifecycle = cloneUintMap(source.QMP.Events.Lifecycle)
	clone.QMP.Events.BlockIO = append([]backend.QEMUBlockIOError(nil), source.QMP.Events.BlockIO...)
	clone.QMP.Blocks = cloneBlocks(source.QMP.Blocks)
	clone.Guest.Observation = cloneGuestObservation(source.Guest.Observation)
	return &clone
}

func cloneUintMap(source map[string]uint64) map[string]uint64 {
	if source == nil {
		return nil
	}
	clone := make(map[string]uint64, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneBlocks(source []backend.QEMUBlockDevice) []backend.QEMUBlockDevice {
	if source == nil {
		return nil
	}
	clone := append([]backend.QEMUBlockDevice(nil), source...)
	for index := range clone {
		clone[index].FailedOperations = cloneUintMap(source[index].FailedOperations)
		clone[index].InvalidOperations = cloneUintMap(source[index].InvalidOperations)
	}
	return clone
}

func cloneGuestObservation(source backend.GuestObservation) backend.GuestObservation {
	clone := source
	clone.Info.Capabilities = make(map[string]bool, len(source.Info.Capabilities))
	for key, value := range source.Info.Capabilities {
		clone.Info.Capabilities[key] = value
	}
	clone.Results = make(map[string]backend.ObservationResult, len(source.Results))
	for key, value := range source.Results {
		clone.Results[key] = value
	}
	clone.CPU = append([]backend.GuestCPU(nil), source.CPU...)
	for index := range clone.CPU {
		clone.CPU[index].Seconds = cloneFloatMap(source.CPU[index].Seconds)
	}
	clone.VCPUs = append([]backend.GuestVCPU(nil), source.VCPUs...)
	clone.Filesystems = append([]backend.GuestFilesystem(nil), source.Filesystems...)
	clone.Networks = append([]backend.GuestNetworkInterface(nil), source.Networks...)
	for index := range clone.Networks {
		clone.Networks[index].Addresses = append([]backend.GuestIPAddress(nil), source.Networks[index].Addresses...)
	}
	clone.Disks = append([]backend.GuestDisk(nil), source.Disks...)
	return clone
}

func cloneFloatMap(source map[string]float64) map[string]float64 {
	if source == nil {
		return nil
	}
	clone := make(map[string]float64, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
