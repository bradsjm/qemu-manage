package monitoring

import "errors"

var ErrProcessStatsUnsupported = errors.New("process statistics are unsupported on this platform")

type ProcessStats struct {
	UserCPUSeconds             float64
	SystemCPUSeconds           float64
	ResidentMemoryBytes        uint64
	WiredMemoryBytes           uint64
	PhysicalFootprintBytes     uint64
	PhysicalFootprintPeakBytes uint64
	DiskReadBytes              uint64
	DiskWrittenBytes           uint64
	PageIns                    uint64
	IdleWakeups                uint64
	InterruptWakeups           uint64
	Instructions               uint64
	Cycles                     uint64
	Threads                    uint64
}
