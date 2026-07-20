package backend

import (
	"context"
	"time"
)

// MonitoringInstance is an optional capability implemented by backends that can
// expose QEMU and guest-agent observations to a supervisor-owned monitor.
type MonitoringInstance interface {
	CollectQEMU(context.Context) QEMUObservation
	CollectGuest(context.Context) GuestObservation
	PingGuest(context.Context) GuestProbe
}

type ObservationResult struct {
	Code string
	Err  error
}

func (r ObservationResult) OK() bool { return r.Err == nil && r.Code == "" }

type QEMUVersion struct {
	Major   int
	Minor   int
	Micro   int
	Package string
}

type QEMUObservation struct {
	Version QEMUVersion
	State   string
	Events  QEMUEventCounters
	Blocks  []QEMUBlockDevice
	QMP     ObservationResult
	Block   ObservationResult
}

type QEMUEventCounters struct {
	Lifecycle map[string]uint64  `json:"lifecycle"`
	BlockIO   []QEMUBlockIOError `json:"block_io_errors"`
}

type QEMUBlockIOError struct {
	Device    string `json:"device"`
	Operation string `json:"operation"`
	NoSpace   bool   `json:"nospace"`
	Count     uint64 `json:"count"`
}

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

type GuestInfo struct {
	Version      string
	Capabilities map[string]bool
}

type GuestProbe struct {
	Result ObservationResult
}

type GuestCPU struct {
	CPU     int                `json:"cpu"`
	Seconds map[string]float64 `json:"seconds"`
}

type GuestLoad struct {
	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`
}

type GuestVCPU struct {
	LogicalID int  `json:"logical_id"`
	Online    bool `json:"online"`
}

type GuestFilesystem struct {
	Mountpoint string  `json:"mountpoint"`
	Type       string  `json:"fstype"`
	SizeBytes  *uint64 `json:"size_bytes,omitempty"`
	UsedBytes  *uint64 `json:"used_bytes,omitempty"`
}

type GuestIPAddress struct {
	Address string `json:"address"`
	Family  string `json:"family"`
	Prefix  int    `json:"prefix"`
}

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
	QMP  time.Duration
	Ping time.Duration
}
