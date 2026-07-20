//go:build darwin

package monitoring

import (
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestCollectProcessWithUsesXNUProcInfoABI(t *testing.T) {
	calls := 0
	call := func(trap, callNumber, pid, flavor, arg, buffer, size uintptr) (uintptr, uintptr, syscall.Errno) {
		calls++
		if trap != unix.SYS_PROC_INFO || pid != 42 || arg != 0 {
			t.Fatalf("call %d common args = %#x, %#x, %#x", calls, trap, pid, arg)
		}
		switch calls {
		case 1:
			if callNumber != procInfoCallPIDRUsage || flavor != rusageInfoV4Flavor || size != unsafe.Sizeof(rusageInfoV4{}) {
				t.Fatalf("rusage args = call %#x flavor %#x size %d", callNumber, flavor, size)
			}
			usage := (*rusageInfoV4)(unsafe.Pointer(buffer))
			usage.UserTime = 1_500_000_000
			usage.SystemTime = 250_000_000
			usage.ResidentSize = 10
			usage.WiredSize = 11
			usage.PhysicalFootprint = 12
			usage.LifetimeMaxPhysicalFootprint = 13
			usage.DiskIOBytesRead = 14
			usage.DiskIOBytesWritten = 15
			usage.PageIns = 16
			usage.PackageIdleWakeups = 17
			usage.InterruptWakeups = 18
			usage.Instructions = 19
			usage.Cycles = 20
			return 0, 0, 0
		case 2:
			if callNumber != procInfoCallPIDInfo || flavor != procPIDTaskInfo || size != unsafe.Sizeof(procTaskInfo{}) {
				t.Fatalf("task args = call %#x flavor %#x size %d", callNumber, flavor, size)
			}
			task := (*procTaskInfo)(unsafe.Pointer(buffer))
			task.ThreadCount = 21
			return size, 0, 0
		default:
			t.Fatalf("unexpected syscall %d", calls)
			return 0, 0, syscall.EINVAL
		}
	}
	stats, err := collectProcessWith(42, call)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || stats.UserCPUSeconds != 1.5 || stats.SystemCPUSeconds != 0.25 ||
		stats.ResidentMemoryBytes != 10 || stats.WiredMemoryBytes != 11 ||
		stats.PhysicalFootprintBytes != 12 || stats.PhysicalFootprintPeakBytes != 13 ||
		stats.DiskReadBytes != 14 || stats.DiskWrittenBytes != 15 || stats.PageIns != 16 ||
		stats.IdleWakeups != 17 || stats.InterruptWakeups != 18 || stats.Instructions != 19 ||
		stats.Cycles != 20 || stats.Threads != 21 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestCollectProcessWithValidatesTaskByteCount(t *testing.T) {
	calls := 0
	_, err := collectProcessWith(42, func(_, _, _, _, _, _, size uintptr) (uintptr, uintptr, syscall.Errno) {
		calls++
		if calls == 2 {
			return size - 1, 0, 0
		}
		return 0, 0, 0
	})
	if err == nil {
		t.Fatal("short task response accepted")
	}
}
