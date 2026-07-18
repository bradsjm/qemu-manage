package supervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"qemu-manage/internal/backend"
	"qemu-manage/internal/model"
	"qemu-manage/internal/store"
)

type fakeInstance struct {
	mu                        sync.Mutex
	state                     model.RunState
	statusErr                 error
	shutdown                  func(context.Context) error
	force                     func(context.Context) error
	exits                     chan backend.Exit
	shutdownCalls, forceCalls int
}

func newFakeInstance() *fakeInstance {
	return &fakeInstance{state: model.RunStateRunning, exits: make(chan backend.Exit, 1)}
}
func (f *fakeInstance) PID() int { return 4242 }
func (f *fakeInstance) Status(context.Context) (model.RunState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.statusErr
}
func (f *fakeInstance) RequestShutdown(ctx context.Context) error {
	f.mu.Lock()
	f.shutdownCalls++
	fn := f.shutdown
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}
func (f *fakeInstance) ForceStop(ctx context.Context) error {
	f.mu.Lock()
	f.forceCalls++
	fn := f.force
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}
func (f *fakeInstance) Wait() <-chan backend.Exit { return f.exits }
func (f *fakeInstance) exit(exit backend.Exit)    { f.exits <- exit; close(f.exits) }

type fakeBackend struct {
	instance *fakeInstance
	start    func() error
}

func (f *fakeBackend) Render(*model.Config, backend.RuntimePaths) (backend.Command, error) {
	return backend.Command{Path: "/fake/qemu"}, nil
}
func (f *fakeBackend) Start(context.Context, *model.Config, backend.RuntimePaths, backend.Command) (backend.Instance, error) {
	if f.start != nil {
		if err := f.start(); err != nil {
			return nil, err
		}
	}
	return f.instance, nil
}

type readinessWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *readinessWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"ok":true`) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func TestSupervisedRunStatusAndHash(t *testing.T) {
	instance := newFakeInstance()
	run := newSupervisedRun(instance, time.Second)
	run.status = Status{State: model.RunStateStarting, Backend: model.BackendQEMU, SupervisorPID: 1, BackendPID: instance.PID(), StartedAt: time.Now().UTC(), RunningConfigSHA256: strings.Repeat("a", 64)}
	status, err := run.currentStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != model.RunStateRunning || status.RunningConfigSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("status = %#v", status)
	}
	instance.exit(backend.Exit{})
}

func TestJoinedNormalStopsShareOneRequest(t *testing.T) {
	instance := newFakeInstance()
	requested := make(chan struct{})
	release := make(chan struct{})
	instance.shutdown = func(context.Context) error { close(requested); <-release; return nil }
	run := newSupervisedRun(instance, time.Hour)
	results := make(chan error, 2)
	go func() { results <- run.stop(context.Background(), false, time.Hour) }()
	<-requested
	go func() { results <- run.stop(context.Background(), false, time.Hour) }()
	close(release)
	instance.exit(backend.Exit{})
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	instance.mu.Lock()
	calls := instance.shutdownCalls
	instance.mu.Unlock()
	if calls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", calls)
	}
}

func TestRejectedShutdownCanBeRetried(t *testing.T) {
	instance := newFakeInstance()
	instance.shutdown = func(context.Context) error { return errors.New("guest rejected") }
	run := newSupervisedRun(instance, time.Hour)
	if err := run.stop(context.Background(), false, time.Hour); err == nil {
		t.Fatal("first stop unexpectedly succeeded")
	}
	instance.mu.Lock()
	instance.shutdown = func(context.Context) error { return nil }
	instance.mu.Unlock()
	result := make(chan error, 1)
	go func() { result <- run.stop(context.Background(), false, time.Hour) }()
	for {
		instance.mu.Lock()
		calls := instance.shutdownCalls
		instance.mu.Unlock()
		if calls == 2 {
			break
		}
		runtime.Gosched()
	}
	instance.exit(backend.Exit{})
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestGracefulTimeoutLeavesBackendRunning(t *testing.T) {
	instance := newFakeInstance()
	run := newSupervisedRun(instance, time.Hour)
	if err := run.stop(context.Background(), false, 0); !errors.Is(err, errShutdownTimeout) {
		t.Fatalf("stop error = %v", err)
	}
	select {
	case <-run.done:
		t.Fatal("timeout reaped backend")
	default:
	}
	instance.exit(backend.Exit{})
}

func TestForcePreemptsGracefulExactlyOnceAndWaitsForReap(t *testing.T) {
	for _, forceBeforeShutdownReturns := range []bool{true, false} {
		name := "after_request_returns"
		if forceBeforeShutdownReturns {
			name = "before_request_returns"
		}
		t.Run(name, func(t *testing.T) {
			instance := newFakeInstance()
			shutdownEntered := make(chan struct{})
			shutdownRelease := make(chan struct{})
			forceEntered := make(chan struct{})
			forceRelease := make(chan struct{})
			forceResult := errors.New("forced result")
			instance.shutdown = func(context.Context) error {
				close(shutdownEntered)
				<-shutdownRelease
				return nil
			}
			instance.force = func(context.Context) error {
				close(forceEntered)
				<-forceRelease
				return forceResult
			}
			run := newSupervisedRun(instance, time.Hour)
			normal := make(chan error, 1)
			forced := make(chan error, 2)
			go func() { normal <- run.stop(context.Background(), false, time.Hour) }()
			<-shutdownEntered

			if !forceBeforeShutdownReturns {
				close(shutdownRelease)
				run.mu.Lock()
				requestDone := run.graceful.request
				run.mu.Unlock()
				<-requestDone
			}
			go func() { forced <- run.stop(context.Background(), true, time.Hour) }()
			go func() { forced <- run.stop(context.Background(), true, time.Hour) }()
			for {
				run.mu.Lock()
				started := run.forceStarted
				run.mu.Unlock()
				if started {
					break
				}
				runtime.Gosched()
			}
			if forceBeforeShutdownReturns {
				close(shutdownRelease)
			}
			<-forceEntered
			close(forceRelease)
			select {
			case <-forced:
				t.Fatal("force returned before backend reap")
			case <-normal:
				t.Fatal("normal stop returned before force completed and backend reaped")
			default:
			}
			instance.exit(backend.Exit{})
			if err := <-normal; !errors.Is(err, forceResult) {
				t.Fatalf("normal stop error = %v, want shared force result", err)
			}
			for range 2 {
				if err := <-forced; !errors.Is(err, forceResult) {
					t.Fatalf("force error = %v, want %v", err, forceResult)
				}
			}
			instance.mu.Lock()
			calls := instance.forceCalls
			instance.mu.Unlock()
			if calls != 1 {
				t.Fatalf("force calls=%d", calls)
			}
		})
	}
}

func validSupervisorConfig() *model.Config {
	return &model.Config{SchemaVersion: 1, ID: testProtocolID, Name: "vm", Backend: model.BackendQEMU, Architecture: "aarch64", UUID: "123e4567-e89b-42d3-a456-426614174000", CPUs: 2, MemoryMiB: 512, RestartPolicy: model.RestartNever, ShutdownTimeoutSeconds: 30, Firmware: model.FirmwareConfig{Code: "code.fd", Variables: "vars.fd"}, Disks: []model.DiskConfig{{Path: "disk.qcow2", Format: "qcow2", Serial: "disk0", BootIndex: 0}}, Network: model.NetworkConfig{Mode: model.NetworkUser, MAC: "02:00:00:00:00:01", Forwards: []model.PortForward{}}, QEMU: model.QEMUConfig{Binary: "/fake/qemu", ExtraArgs: []string{}}, Autostart: model.AutostartConfig{Scope: model.AutostartNone}}
}

func supervisorFixture(t *testing.T, instance *fakeInstance) (*Service, *model.Config, store.Paths) {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "qm-s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove supervisor fixture: %v", err)
		}
	})
	st, err := store.New(filepath.Join(root, "data"), filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := validSupervisorConfig()
	if err := st.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	registry := backend.NewRegistry()
	if err := registry.RegisterInstance(string(model.BackendQEMU), &fakeBackend{instance: instance}); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, registry)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	service.Clock = func() time.Time { return fixed }
	return service, cfg, st.Paths(cfg)
}

func TestSuperviseReadinessClearsFailureAndPersistsNormalExit(t *testing.T) {
	instance := newFakeInstance()
	service, cfg, paths := supervisorFixture(t, instance)
	if err := os.Mkdir(paths.RuntimeDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if err := WriteLastExit(paths.LastExitMetadata, LastExitMetadata{Version: 1, ID: cfg.ID, Timestamp: time.Now().UTC(), ExitCode: 1, Error: "old"}); err != nil {
		t.Fatal(err)
	}
	writer := &readinessWriter{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- service.Supervise(context.Background(), cfg.Name, cfg.ID, writer) }()
	select {
	case <-writer.ready:
	case err := <-done:
		t.Fatalf("supervisor exited before readiness: %v", err)
	}
	if _, err := os.Stat(paths.LastExitMetadata); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failure not cleared: %v", err)
	}
	instance.exit(backend.Exit{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	last, err := ReadLastExit(paths.LastExitMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if last.ExitCode != 0 || last.Error != "" {
		t.Fatalf("last exit=%#v", last)
	}
	if _, err := os.Stat(paths.ControlSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket remains: %v", err)
	}
}

func TestSuperviseDuplicateLifetimeLockAndPreflightBeforeSocket(t *testing.T) {
	instance := newFakeInstance()
	service, cfg, paths := supervisorFixture(t, instance)
	checked := make(chan struct{})
	service.Preflight = func(_ context.Context, _ *model.Config, _ store.Paths) error {
		if _, err := os.Stat(paths.ControlSocket); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("socket existed during preflight: %v", err)
		}
		lock, acquired, err := service.Store.TryLifetime(cfg.ID)
		if err != nil {
			t.Errorf("try lifetime: %v", err)
		} else {
			if acquired {
				t.Error("lifetime lock not held during preflight")
			}
			if lock != nil {
				_ = lock.Close()
			}
		}
		close(checked)
		return errors.New("blocked")
	}
	err := service.Supervise(context.Background(), cfg.Name, cfg.ID, io.Discard)
	<-checked
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Stat(paths.ControlSocket); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("preflight created socket: %v", statErr)
	}
}

func TestUnsupportedPeerAuthenticationFailsClosed(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin peer credentials are covered on Darwin")
	}
	instance := newFakeInstance()
	run := newSupervisedRun(instance, time.Second)
	if _, err := peerUID(nil); err == nil {
		t.Fatal("unsupported platform authenticated a peer")
	}
	instance.exit(backend.Exit{})
	run.handlers.Wait()
}
