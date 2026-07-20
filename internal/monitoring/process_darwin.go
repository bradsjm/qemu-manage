//go:build darwin

package monitoring

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const processStatsSupported = true

const (
	procInfoCallPIDInfo   = uintptr(0x2)
	procInfoCallPIDRUsage = uintptr(0x9)
	rusageInfoV4Flavor    = uintptr(4)
	procPIDTaskInfo       = uintptr(4)
)

// procInfoCall matches the proc_info syscall ABI used by collectProcessWith tests
type procInfoCall func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno)

// rusageInfoV4 mirrors the macOS RUSAGE_INFO_V4 payload returned by proc_info
type rusageInfoV4 struct {
	UUID                         [16]byte
	UserTime                     uint64
	SystemTime                   uint64
	PackageIdleWakeups           uint64
	InterruptWakeups             uint64
	PageIns                      uint64
	WiredSize                    uint64
	ResidentSize                 uint64
	PhysicalFootprint            uint64
	ProcessStartAbsoluteTime     uint64
	ProcessExitAbsoluteTime      uint64
	ChildUserTime                uint64
	ChildSystemTime              uint64
	ChildPackageIdleWakeups      uint64
	ChildInterruptWakeups        uint64
	ChildPageIns                 uint64
	ChildElapsedAbsoluteTime     uint64
	DiskIOBytesRead              uint64
	DiskIOBytesWritten           uint64
	CPUTimeQoSDefault            uint64
	CPUTimeQoSMaintenance        uint64
	CPUTimeQoSBackground         uint64
	CPUTimeQoSUtility            uint64
	CPUTimeQoSLegacy             uint64
	CPUTimeQoSUserInitiated      uint64
	CPUTimeQoSUserInteractive    uint64
	BilledSystemTime             uint64
	ServicedSystemTime           uint64
	LogicalWrites                uint64
	LifetimeMaxPhysicalFootprint uint64
	Instructions                 uint64
	Cycles                       uint64
	BilledEnergy                 uint64
	ServicedEnergy               uint64
	IntervalMaxPhysicalFootprint uint64
	RunnableTime                 uint64
}

type procTaskInfo struct {
	VirtualSize       uint64
	ResidentSize      uint64
	TotalUser         uint64
	TotalSystem       uint64
	ThreadsUser       uint64
	ThreadsSystem     uint64
	Policy            int32
	Faults            int32
	PageIns           int32
	CopyOnWriteFaults int32
	MessagesSent      int32
	MessagesReceived  int32
	SyscallsMach      int32
	SyscallsUnix      int32
	ContextSwitches   int32
	ThreadCount       int32
	RunningThreads    int32
	Priority          int32
}

func collectProcess(pid int) (ProcessStats, error) {
	return collectProcessWith(pid, unix.Syscall6)
}

func collectProcessWith(pid int, call procInfoCall) (ProcessStats, error) {
	if pid <= 0 {
		return ProcessStats{}, fmt.Errorf("process stats: invalid PID %d", pid)
	}
	var usage rusageInfoV4
	// First fetch the rusage payload for cumulative CPU, memory, wakeup, and I/O counters.
	_, _, errno := call(
		unix.SYS_PROC_INFO,
		procInfoCallPIDRUsage,
		uintptr(pid),
		rusageInfoV4Flavor,
		0,
		uintptr(unsafe.Pointer(&usage)),
		unsafe.Sizeof(usage),
	)
	runtime.KeepAlive(&usage)
	if errno != 0 {
		return ProcessStats{}, fmt.Errorf("process stats: PID rusage: %w", errno)
	}

	var task procTaskInfo
	// Then fetch PROC_PIDTASKINFO for task-specific counters including the live thread count.
	copied, _, errno := call(
		unix.SYS_PROC_INFO,
		procInfoCallPIDInfo,
		uintptr(pid),
		procPIDTaskInfo,
		0,
		uintptr(unsafe.Pointer(&task)),
		unsafe.Sizeof(task),
	)
	runtime.KeepAlive(&task)
	if errno != 0 {
		return ProcessStats{}, fmt.Errorf("process stats: PID task info: %w", errno)
	}
	if copied != unsafe.Sizeof(task) {
		return ProcessStats{}, fmt.Errorf("process stats: PID task info returned %d bytes, want %d", copied, unsafe.Sizeof(task))
	}
	if task.ThreadCount < 0 {
		return ProcessStats{}, fmt.Errorf("process stats: negative thread count %d", task.ThreadCount)
	}

	return ProcessStats{
		UserCPUSeconds:             float64(usage.UserTime) / 1e9,
		SystemCPUSeconds:           float64(usage.SystemTime) / 1e9,
		ResidentMemoryBytes:        usage.ResidentSize,
		WiredMemoryBytes:           usage.WiredSize,
		PhysicalFootprintBytes:     usage.PhysicalFootprint,
		PhysicalFootprintPeakBytes: usage.LifetimeMaxPhysicalFootprint,
		DiskReadBytes:              usage.DiskIOBytesRead,
		DiskWrittenBytes:           usage.DiskIOBytesWritten,
		PageIns:                    usage.PageIns,
		IdleWakeups:                usage.PackageIdleWakeups,
		InterruptWakeups:           usage.InterruptWakeups,
		Instructions:               usage.Instructions,
		Cycles:                     usage.Cycles,
		Threads:                    uint64(task.ThreadCount),
	}, nil
}
