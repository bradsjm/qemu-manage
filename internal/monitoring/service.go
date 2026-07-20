package monitoring

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

const (
	qmpInterval      = 10 * time.Second
	guestInterval    = 30 * time.Second
	qmpStaleAfter    = 30 * time.Second
	guestStaleAfter  = 90 * time.Second
	collectorTimeout = 5 * time.Second
)

type ProcessCollector func(int) (ProcessStats, error)

type Options struct {
	Instance   backend.MonitoringInstance
	PID        int
	VM         VMIdentity
	Clock      func() time.Time
	Process    ProcessCollector
	QMPTicks   <-chan time.Time
	GuestTicks <-chan time.Time
}

type Service struct {
	instance   backend.MonitoringInstance
	pid        int
	vm         VMIdentity
	clock      func() time.Time
	pingMu     sync.Mutex
	ping       *pingCall
	process    ProcessCollector
	qmpTicks   <-chan time.Time
	guestTicks <-chan time.Time

	mu    sync.Mutex
	store snapshotStore
}

func New(options Options) *Service {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	process := options.Process
	if process == nil {
		process = collectProcess
	}
	return &Service{instance: options.Instance, pid: options.PID, vm: options.VM, clock: clock, process: process, qmpTicks: options.QMPTicks, guestTicks: options.GuestTicks}
}

func (s *Service) Seed(ctx context.Context, fallbackState string) {
	now := s.clock().UTC()
	snapshot := &Snapshot{
		ObservedAt: now,
		VM:         s.vm,
		QMP:        QMPState{State: fallbackState},
		Process:    ProcessState{PID: s.pid},
		Collectors: map[string]CollectorState{
			"qmp": {Status: CollectorPending}, "block": {Status: CollectorPending},
			"process": {Status: CollectorPending}, "guest_info": {Status: CollectorPending},
		},
	}
	if !s.vm.GuestAgent {
		snapshot.Collectors["guest_info"] = CollectorState{Status: CollectorUnsupported, Code: "guest_agent_not_configured"}
	}
	s.store.store(snapshot)
	s.refreshQMP(ctx)
	s.refreshProcess()
}

func (s *Service) Snapshot() *Snapshot {
	snapshot := s.store.load()
	if snapshot == nil {
		return nil
	}
	return markStale(cloneSnapshot(snapshot), s.clock().UTC())
}

func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		qmpTicks, guestTicks, stop := s.tickSources()
		defer stop()
		s.refreshGuest(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-qmpTicks:
				s.refreshQMP(ctx)
				s.refreshProcess()
			case <-guestTicks:
				s.refreshGuest(ctx)
			}
		}
	}()
	return done
}

func (s *Service) tickSources() (<-chan time.Time, <-chan time.Time, func()) {
	if s.qmpTicks != nil && s.guestTicks != nil {
		return s.qmpTicks, s.guestTicks, func() {}
	}
	qmpTicker := time.NewTicker(qmpInterval)
	guestTicker := time.NewTicker(guestInterval)
	qmp := s.qmpTicks
	if qmp == nil {
		qmp = qmpTicker.C
	}
	guest := s.guestTicks
	if guest == nil {
		guest = guestTicker.C
	}
	return qmp, guest, func() { qmpTicker.Stop(); guestTicker.Stop() }
}

func (s *Service) refreshQMP(parent context.Context) {
	started := s.clock().UTC()
	ctx, cancel := context.WithTimeout(parent, collectorTimeout)
	observation := s.instance.CollectQEMU(ctx)
	cancel()
	finished := s.clock().UTC()
	s.update(func(snapshot *Snapshot) {
		state := collectorFromResult(observation.QMP, started, finished)
		if state.Status == CollectorOK {
			snapshot.QMP.Version = observation.Version
			snapshot.QMP.State = observation.State
			snapshot.QMP.Events = cloneQEMUEvents(observation.Events)
		}
		snapshot.Collectors["qmp"] = state
		blockState := collectorFromResult(observation.Block, started, finished)
		if state.Status != CollectorOK && observation.Block.OK() {
			blockState = CollectorState{Status: CollectorFailed, Code: state.Code, ObservedAt: finished, Duration: finished.Sub(started)}
		}
		if blockState.Status == CollectorOK {
			snapshot.QMP.Blocks = cloneBlocks(observation.Blocks)
		}
		snapshot.Collectors["block"] = preserveSuccess(snapshot.Collectors["block"], blockState)
	})
}

func (s *Service) refreshProcess() {
	started := s.clock().UTC()
	stats, err := s.process(s.pid)
	finished := s.clock().UTC()
	s.update(func(snapshot *Snapshot) {
		state := CollectorState{Status: CollectorOK, ObservedAt: finished, LastSuccess: finished, Duration: finished.Sub(started)}
		switch {
		case errors.Is(err, ErrProcessStatsUnsupported):
			state.Status = CollectorUnsupported
		case err != nil:
			state.Status, state.Code = CollectorFailed, "process_stats_unavailable"
		default:
			snapshot.Process.Stats = stats
		}
		snapshot.Collectors["process"] = preserveSuccess(snapshot.Collectors["process"], state)
	})
}

func (s *Service) refreshGuest(parent context.Context) {
	started := s.clock().UTC()
	ctx, cancel := context.WithTimeout(parent, collectorTimeout)
	observation := s.instance.CollectGuest(ctx)
	cancel()
	finished := s.clock().UTC()
	s.update(func(snapshot *Snapshot) {
		if result, ok := observation.Results["info"]; ok && !result.OK() {
			state := collectorFromResult(result, started, finished)
			snapshot.Collectors["guest_info"] = preserveSuccess(snapshot.Collectors["guest_info"], state)
			return
		}
		snapshot.Guest.Observation = cloneGuestObservation(observation)
		snapshot.Collectors["guest_info"] = CollectorState{Status: CollectorOK, ObservedAt: finished, LastSuccess: finished, Duration: finished.Sub(started)}
		for key, result := range observation.Results {
			if key == "info" {
				continue
			}
			state := collectorFromResult(result, started, finished)
			snapshot.Collectors["guest_"+key] = preserveSuccess(snapshot.Collectors["guest_"+key], state)
		}
	})
}

func (s *Service) update(change func(*Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := cloneSnapshot(s.store.load())
	change(snapshot)
	snapshot.ObservedAt = s.clock().UTC()
	s.store.store(snapshot)
}

func collectorFromResult(result backend.ObservationResult, started, finished time.Time) CollectorState {
	state := CollectorState{Status: CollectorOK, ObservedAt: finished, LastSuccess: finished, Duration: finished.Sub(started)}
	if !result.OK() {
		state.Status, state.Code, state.LastSuccess = CollectorFailed, result.Code, time.Time{}
		if result.Code == "guest_agent_not_configured" || result.Code == "guest_agent_command_disabled" {
			state.Status = CollectorUnsupported
		}
	}
	return state
}

func preserveSuccess(previous, current CollectorState) CollectorState {
	if current.Status != CollectorOK && !previous.LastSuccess.IsZero() {
		current.LastSuccess = previous.LastSuccess
	}
	return current
}

func markStale(snapshot *Snapshot, now time.Time) *Snapshot {
	for key, state := range snapshot.Collectors {
		threshold := qmpStaleAfter
		if len(key) >= 6 && key[:6] == "guest_" {
			threshold = guestStaleAfter
		}
		if (state.Status == CollectorOK || state.Status == CollectorFailed) && !state.LastSuccess.IsZero() && now.Sub(state.LastSuccess) > threshold {
			state.Status = CollectorStale
			if key == "qmp" {
				state.Code = "qmp_stale"
			}
			snapshot.Collectors[key] = state
		}
	}
	return snapshot
}

func cloneQEMUEvents(source backend.QEMUEventCounters) backend.QEMUEventCounters {
	return backend.QEMUEventCounters{Lifecycle: cloneUintMap(source.Lifecycle), BlockIO: append([]backend.QEMUBlockIOError(nil), source.BlockIO...)}
}
