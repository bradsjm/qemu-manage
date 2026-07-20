package monitoring

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

type fakeMonitoringInstance struct {
	mu         sync.Mutex
	qemu       []backend.QEMUObservation
	guest      []backend.GuestObservation
	qemuCalls  int
	guestCalls int
}

func (f *fakeMonitoringInstance) CollectQEMU(context.Context) backend.QEMUObservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.qemuCalls
	f.qemuCalls++
	if len(f.qemu) == 0 {
		return backend.QEMUObservation{}
	}
	if index >= len(f.qemu) {
		return f.qemu[len(f.qemu)-1]
	}
	return f.qemu[index]
}
func (f *fakeMonitoringInstance) CollectGuest(context.Context) backend.GuestObservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.guestCalls
	f.guestCalls++
	if len(f.guest) == 0 {
		return backend.GuestObservation{Results: map[string]backend.ObservationResult{"info": {Code: "guest_agent_not_configured"}}}
	}
	if index >= len(f.guest) {
		return f.guest[len(f.guest)-1]
	}
	return f.guest[index]
}
func (*fakeMonitoringInstance) PingGuest(context.Context) backend.GuestProbe {
	return backend.GuestProbe{}
}

func TestServiceSeedsRefreshesAndRetainsLastGood(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	instance := &fakeMonitoringInstance{qemu: []backend.QEMUObservation{
		{State: "running", Version: backend.QEMUVersion{Major: 11}, Events: backend.QEMUEventCounters{Lifecycle: map[string]uint64{"reset": 1}}, Blocks: []backend.QEMUBlockDevice{{Device: "disk", ReadBytes: new(uint64)}}},
		{QMP: backend.ObservationResult{Code: "qmp_protocol_error", Err: errors.New("bad frame")}},
	}}
	qmpTicks := make(chan time.Time, 1)
	guestTicks := make(chan time.Time)
	service := New(Options{
		Instance: instance, PID: 42,
		VM:    VMIdentity{ID: "id", Name: "vm", GuestAgent: false, StartedAt: now.Add(-time.Minute)},
		Clock: clock, Process: func(int) (ProcessStats, error) { return ProcessStats{}, ErrProcessStatsUnsupported },
		QMPTicks: qmpTicks, GuestTicks: guestTicks,
	})
	service.Seed(context.Background(), "paused")
	snapshot := service.Snapshot()
	if snapshot.QMP.State != "running" || snapshot.Collectors["qmp"].Status != CollectorOK || snapshot.Collectors["process"].Status != CollectorUnsupported {
		t.Fatalf("seed snapshot = %#v", snapshot)
	}
	if snapshot.QMP.Events.Lifecycle["reset"] != 1 || len(snapshot.QMP.Blocks) != 1 {
		t.Fatalf("seed QMP data = %#v", snapshot.QMP)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := service.Start(ctx)
	qmpTicks <- now
	for {
		snapshot = service.Snapshot()
		if snapshot.Collectors["qmp"].Status == CollectorFailed {
			break
		}
		runtime.Gosched()
	}
	if snapshot.Collectors["qmp"].Status != CollectorFailed || snapshot.QMP.State != "running" || snapshot.QMP.Events.Lifecycle["reset"] != 1 {
		t.Fatalf("failed refresh did not retain last good: %#v", snapshot)
	}
	cancel()
	<-done
}

func TestServiceMarksCollectorsStale(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	instance := &fakeMonitoringInstance{qemu: []backend.QEMUObservation{{State: "running"}}}
	service := New(Options{Instance: instance, PID: 1, Clock: func() time.Time { return now }, Process: func(int) (ProcessStats, error) { return ProcessStats{}, nil }})
	service.Seed(context.Background(), "paused")
	now = now.Add(qmpStaleAfter + time.Second)
	snapshot := service.Snapshot()
	if snapshot.Collectors["qmp"].Status != CollectorStale || snapshot.Collectors["qmp"].Code != "qmp_stale" || snapshot.Collectors["process"].Status != CollectorStale {
		t.Fatalf("stale snapshot = %#v", snapshot.Collectors)
	}
}
