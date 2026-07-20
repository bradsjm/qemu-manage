package backend

import (
	"context"
	"time"
)

// MonitoringInstance is an optional capability implemented by backends that can
// expose supervisor-polled QMP and guest-agent observations.
//
// Each call returns one self-contained snapshot; transport or protocol failures
// are encoded in the returned ObservationResult values.
type MonitoringInstance interface {
	// CollectQEMU gathers one QMP-backed snapshot of backend state, event
	// counters, and block statistics.
	CollectQEMU(context.Context) QEMUObservation
	// CollectGuest gathers one QGA-backed snapshot of guest information and
	// metric families.
	CollectGuest(context.Context) GuestObservation
	// PingGuest issues the lightweight guest-ping probe used for reachability.
	PingGuest(context.Context) GuestProbe
}

// ObservationResult describes the outcome of one monitoring protocol call.
// Code is empty only on success. Err carries the underlying failure when one
// exists; unsupported or disabled features may report a non-empty Code with a
// nil Err.
type ObservationResult struct {
	Code string
	Err  error
}

// OK reports whether the protocol call completed without a failure or
// unsupported condition.
func (r ObservationResult) OK() bool { return r.Err == nil && r.Code == "" }

// QEMUVersion is the version tuple reported by QMP query-version.
type QEMUVersion struct {
	Major   int
	Minor   int
	Micro   int
	Package string
}

// QEMUObservation is one QMP refresh.
//
// State is the query-status run state, Events are supervisor-lifetime counters
// from the accepted QMP event stream, Blocks are cumulative query-blockstats
// devices, QMP records connection, status, or event-query failure, and Block
// records query-blockstats or query-block failure.
type QEMUObservation struct {
	Version QEMUVersion
	State   string
	Events  QEMUEventCounters
	Blocks  []QEMUBlockDevice
	QMP     ObservationResult
	Block   ObservationResult
}

// QEMUEventCounters contains the bounded QMP event aggregates preserved for the
// lifetime of the backend process under supervision.
type QEMUEventCounters struct {
	Lifecycle map[string]uint64  `json:"lifecycle"`
	BlockIO   []QEMUBlockIOError `json:"block_io_errors"`
}

// QEMUBlockIOError is the cumulative count for one BLOCK_IO_ERROR aggregate
// key. Device may be empty when QEMU reported no associated device.
type QEMUBlockIOError struct {
	Device    string `json:"device"`
	Operation string `json:"operation"`
	NoSpace   bool   `json:"nospace"`
	Count     uint64 `json:"count"`
}

// QEMUBlockDevice is one named device from QMP query-blockstats and query-block.
//
// Pointer fields are nil when QMP omitted the value. Numeric counters and
// durations are the cumulative values reported by QMP for the lifetime of the
// backend process, while FailedOperations and InvalidOperations contain only the
// operation keys QMP reported.
type QEMUBlockDevice struct {
	Device            string            `json:"device"`
	ReadBytes         *uint64           `json:"read_bytes,omitempty"`
	WriteBytes        *uint64           `json:"write_bytes,omitempty"`
	UnmapBytes        *uint64           `json:"unmap_bytes,omitempty"`
	ReadOperations    *uint64           `json:"read_operations,omitempty"`
	WriteOperations   *uint64           `json:"write_operations,omitempty"`
	FlushOperations   *uint64           `json:"flush_operations,omitempty"`
	UnmapOperations   *uint64           `json:"unmap_operations,omitempty"`
	ReadSeconds       *float64          `json:"read_seconds,omitempty"`
	WriteSeconds      *float64          `json:"write_seconds,omitempty"`
	FlushSeconds      *float64          `json:"flush_seconds,omitempty"`
	UnmapSeconds      *float64          `json:"unmap_seconds,omitempty"`
	FailedOperations  map[string]uint64 `json:"failed_operations,omitempty"`
	InvalidOperations map[string]uint64 `json:"invalid_operations,omitempty"`
	IdleSeconds       *float64          `json:"idle_seconds,omitempty"`
	IOStatus          *string           `json:"io_status,omitempty"`
}

// GuestObservation is one QGA refresh.
//
// Info comes from guest-info. The slice and pointer fields hold the decoded
// data from the individual guest-agent metric families; nil pointers indicate
// that the family omitted the value or that the collector did not produce one
// for this refresh. Results records the per-family outcome codes keyed by
// collector name.
type GuestObservation struct {
	Info        GuestInfo
	CPU         []GuestCPU
	Load        *GuestLoad
	VCPUs       []GuestVCPU
	ClockOffset *float64
	Frozen      *bool
	Filesystems []GuestFilesystem
	Networks    []GuestNetworkInterface
	Disks       []GuestDisk
	Results     map[string]ObservationResult
}

// GuestInfo is the guest-info identity and capability set returned by QGA.
type GuestInfo struct {
	Version      string
	Capabilities map[string]bool
}

// GuestProbe is the result of a lightweight guest-ping reachability check.
type GuestProbe struct {
	Result ObservationResult
}

// GuestCPU is one guest-get-cpustats record. Seconds is the per-mode cumulative
// CPU time map reported by QGA for that logical CPU.
type GuestCPU struct {
	CPU     int                `json:"cpu"`
	Seconds map[string]float64 `json:"seconds"`
}

// GuestLoad contains the guest-get-load averages. Nil fields indicate that QGA
// omitted that window from the response.
type GuestLoad struct {
	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`
}

// GuestVCPU is one guest-get-vcpus record keyed by the guest logical CPU ID.
type GuestVCPU struct {
	LogicalID int  `json:"logical_id"`
	Online    bool `json:"online"`
}

// GuestFilesystem is one guest-get-fsinfo record. SizeBytes and UsedBytes are
// nil when QGA omitted the capacity values.
type GuestFilesystem struct {
	Mountpoint string  `json:"mountpoint"`
	Type       string  `json:"fstype"`
	SizeBytes  *uint64 `json:"size_bytes,omitempty"`
	UsedBytes  *uint64 `json:"used_bytes,omitempty"`
}

// GuestIPAddress is one canonicalized guest-network-get-interfaces address.
type GuestIPAddress struct {
	Address string `json:"address"`
	Family  string `json:"family"`
	Prefix  int    `json:"prefix"`
}

// GuestNetworkInterface is one guest-network-get-interfaces record.
//
// AddressesPresent distinguishes an explicit empty address list from a missing
// ip-addresses field. The counter pointers are the cumulative interface values
// reported by QGA and are nil when the statistics block or individual field was
// absent.
type GuestNetworkInterface struct {
	Name             string           `json:"name"`
	AddressesPresent bool             `json:"-"`
	Addresses        []GuestIPAddress `json:"addresses,omitempty"`
	ReceiveBytes     *uint64          `json:"receive_bytes,omitempty"`
	TransmitBytes    *uint64          `json:"transmit_bytes,omitempty"`
	ReceivePackets   *uint64          `json:"receive_packets,omitempty"`
	TransmitPackets  *uint64          `json:"transmit_packets,omitempty"`
	ReceiveErrors    *uint64          `json:"receive_errors,omitempty"`
	TransmitErrors   *uint64          `json:"transmit_errors,omitempty"`
	ReceiveDropped   *uint64          `json:"receive_dropped,omitempty"`
	TransmitDropped  *uint64          `json:"transmit_dropped,omitempty"`
}

// GuestDisk is one guest-get-diskstats record.
//
// Pointer fields are nil when the guest agent omitted the metric. The sector,
// operation, merge, and duration values are the cumulative counters QGA
// reported for that device, while IOInFlight is the instantaneous in-flight I/O
// count.
type GuestDisk struct {
	Device                  string   `json:"device"`
	ReadSectors             *uint64  `json:"read_sectors,omitempty"`
	WriteSectors            *uint64  `json:"write_sectors,omitempty"`
	DiscardSectors          *uint64  `json:"discard_sectors,omitempty"`
	ReadOperations          *uint64  `json:"read_operations,omitempty"`
	WriteOperations         *uint64  `json:"write_operations,omitempty"`
	DiscardOperations       *uint64  `json:"discard_operations,omitempty"`
	FlushOperations         *uint64  `json:"flush_operations,omitempty"`
	ReadMergedOperations    *uint64  `json:"read_merged_operations,omitempty"`
	WriteMergedOperations   *uint64  `json:"write_merged_operations,omitempty"`
	DiscardMergedOperations *uint64  `json:"discard_merged_operations,omitempty"`
	ReadSeconds             *float64 `json:"read_seconds,omitempty"`
	WriteSeconds            *float64 `json:"write_seconds,omitempty"`
	DiscardSeconds          *float64 `json:"discard_seconds,omitempty"`
	FlushSeconds            *float64 `json:"flush_seconds,omitempty"`
	WeightedIOSeconds       *float64 `json:"weighted_io_seconds,omitempty"`
	IOInFlight              *uint64  `json:"io_in_flight,omitempty"`
}

// MonitoringTimeouts are the protocol deadlines used by the concrete backend.
// They are exported so the supervisor's live probe can share the same contract.
type MonitoringTimeouts struct {
	// QMP bounds one QMP-backed monitoring refresh.
	QMP time.Duration
	// Ping bounds the lightweight guest-ping reachability probe.
	Ping time.Duration
}
