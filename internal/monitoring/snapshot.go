package monitoring

import (
	"sync/atomic"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

// CollectorStatus reports how trustworthy a collector's latest attempt is.
type CollectorStatus string

const (
	// CollectorOK means the latest attempt succeeded and its payload is current.
	CollectorOK CollectorStatus = "ok"
	// CollectorPending means the initial attempt has not completed yet.
	CollectorPending CollectorStatus = "pending"
	// CollectorUnsupported means the VM or host platform cannot provide this
	// observation and callers should treat the payload as permanently absent.
	CollectorUnsupported CollectorStatus = "unsupported"
	// CollectorFailed means the latest attempt failed. The snapshot may still
	// retain payload from an earlier success.
	CollectorFailed CollectorStatus = "failed"
	// CollectorStale means the last successful payload aged past its freshness
	// window and is retained only for diagnosis.
	CollectorStale CollectorStatus = "stale"
)

// CollectorState records the latest attempt for one logical collector key in
// Snapshot.Collectors. LastSuccess is retained across later failures or stale
// transitions so callers can age the last known-good payload.
type CollectorState struct {
	// Status classifies the latest attempt.
	Status CollectorStatus
	// Code is a stable machine-readable failure or unsupported reason. It is
	// empty when Status is CollectorOK or CollectorPending.
	Code string
	// ObservedAt is when the latest attempt finished, successful or not.
	ObservedAt time.Time
	// LastSuccess is when this collector last completed successfully. It is zero
	// only before the first success.
	LastSuccess time.Time
	// Duration is the runtime of the latest attempt, not the age of its payload.
	Duration time.Duration
}

// VMIdentity captures the immutable per-run metadata copied from validated
// config and supervisor state into every snapshot.
type VMIdentity struct {
	// ID is the immutable VM identifier from the durable config.
	ID string
	// Name is the operator-facing VM name from the durable config.
	Name string
	// Backend names the runtime backend producing the observations.
	Backend string
	// Architecture is the guest architecture rendered into the backend command.
	Architecture string
	// CPUs is the configured vCPU count for the guest.
	CPUs int
	// MemoryMiB is the configured guest memory size in MiB.
	MemoryMiB int
	// GuestAgent reports whether guest-agent collectors are expected to succeed.
	GuestAgent bool
	// StartedAt is the supervisor start time for the current run.
	StartedAt time.Time
	// BuildVersion is the qemu-manage build version rendered by the info route.
	BuildVersion string
}

// QMPState holds the latest QMP-derived observation payload.
type QMPState struct {
	// Version is the QEMU version returned by the latest successful QMP query.
	Version backend.QEMUVersion
	// State is the latest successful QMP run-state string, or the seeded
	// fallback before QMP succeeds.
	State string
	// Events is the bounded set of cumulative QMP event counters observed during
	// the current supervisor lifetime.
	Events backend.QEMUEventCounters
	// Blocks is the latest successful block query payload. Nil means no
	// successful block observation has completed yet.
	Blocks []backend.QEMUBlockDevice
}

// ProcessState holds backend-process identity and locally sampled host metrics.
type ProcessState struct {
	// PID is the backend process PID owned by the supervisor.
	PID int
	// Stats is the last successful host-process sample. It remains unchanged when
	// later process-collector attempts fail.
	Stats ProcessStats
}

// GuestState holds the latest guest-agent payload and live ping timing.
type GuestState struct {
	// Observation is the latest successful guest-agent snapshot. Nil pointers
	// inside the backend payload mean the guest omitted or does not support a
	// specific value.
	Observation backend.GuestObservation
	// ProbeDuration is the runtime of the last live guest ping. It is zero until
	// a ping completes.
	ProbeDuration time.Duration
	// ProbeAt is when the last live guest ping completed. It is zero until a
	// ping completes.
	ProbeAt time.Time
}

// Snapshot is the immutable monitoring payload published by Service.
// Callers must treat each returned instance as a point-in-time view and obtain
// a fresh copy instead of mutating shared service state.
type Snapshot struct {
	// ObservedAt is when the cache was last updated by any collector.
	ObservedAt time.Time
	// VM is the fixed identity for the current VM run.
	VM VMIdentity
	// QMP is the latest QMP-derived payload.
	QMP QMPState
	// Process is the latest host-process payload for the backend PID.
	Process ProcessState
	// Guest is the latest guest-agent payload and ping timing.
	Guest GuestState
	// Collectors tracks stable collector keys such as qmp, block, process, and
	// guest_*.
	Collectors map[string]CollectorState
}

type snapshotStore struct {
	value atomic.Pointer[Snapshot]
}

func (s *snapshotStore) load() *Snapshot          { return s.value.Load() }
func (s *snapshotStore) store(snapshot *Snapshot) { s.value.Store(snapshot) }

// cloneSnapshot deep-copies the published snapshot so readers never share mutable maps or slices
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

// cloneUintMap copies string-to-counter maps used inside snapshot payloads
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

// cloneBlocks deep-copies block-device slices and their nested operation maps
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

// cloneGuestObservation deep-copies guest payload slices, maps, and nested address lists
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

// cloneFloatMap copies string-to-float maps used in guest CPU payloads
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
