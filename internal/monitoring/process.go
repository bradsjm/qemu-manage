package monitoring

import "errors"

// ErrProcessStatsUnsupported reports that the current host platform cannot
// sample backend-process counters for monitoring.
var ErrProcessStatsUnsupported = errors.New("process statistics are unsupported on this platform")

// ProcessStats contains host-process counters sampled for the backend PID.
// Values are cumulative over the process lifetime unless noted otherwise. When
// the platform cannot expose these counters, the process collector returns
// ErrProcessStatsUnsupported and leaves every field zero.
type ProcessStats struct {
	// UserCPUSeconds is cumulative user-mode CPU time consumed by the process.
	UserCPUSeconds float64
	// SystemCPUSeconds is cumulative kernel-mode CPU time consumed by the
	// process.
	SystemCPUSeconds float64
	// ResidentMemoryBytes is the current resident memory footprint in bytes.
	ResidentMemoryBytes uint64
	// WiredMemoryBytes is the current wired memory footprint in bytes when the
	// host kernel exposes it.
	WiredMemoryBytes uint64
	// PhysicalFootprintBytes is the current physical footprint in bytes when the
	// host kernel exposes it.
	PhysicalFootprintBytes uint64
	// PhysicalFootprintPeakBytes is the peak physical footprint in bytes when the
	// host kernel exposes it.
	PhysicalFootprintPeakBytes uint64
	// DiskReadBytes is the cumulative number of bytes read by the process.
	DiskReadBytes uint64
	// DiskWrittenBytes is the cumulative number of bytes written by the process.
	DiskWrittenBytes uint64
	// PageIns is the cumulative number of page-in events attributed to the
	// process.
	PageIns uint64
	// IdleWakeups is the cumulative number of idle wakeups attributed to the
	// process.
	IdleWakeups uint64
	// InterruptWakeups is the cumulative number of interrupt wakeups attributed
	// to the process.
	InterruptWakeups uint64
	// Instructions is the cumulative number of retired instructions when the
	// host kernel exposes it.
	Instructions uint64
	// Cycles is the cumulative number of CPU cycles when the host kernel exposes
	// it.
	Cycles uint64
	// Threads is the current thread count for the process.
	Threads uint64
}
