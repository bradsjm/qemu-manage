// Package supervisor owns one runtime child per VM, serves authenticated
// control over the per-VM Unix socket, persists runtime metadata for that
// child, and optionally exposes loopback monitoring for its live observations.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/store"
)

const defaultStartTimeout = 15 * time.Second

var supervisorIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type Service struct {
	Store        *store.Store
	Registry     *backend.Registry
	Preflight    func(context.Context, *model.Config, store.Paths) error
	Clock        func() time.Time
	StartTimeout time.Duration
	BuildVersion string
}

func NewService(st *store.Store, registry *backend.Registry) *Service {
	return &Service{Store: st, Registry: registry, Clock: time.Now, StartTimeout: defaultStartTimeout}
}

type SuperviseOptions struct {
	BootMenu    bool
	Debug       bool
	DebugWriter io.Writer
}

func (s *Service) Supervise(ctx context.Context, name, expectedID string, ready io.Writer, options SuperviseOptions) (resultErr error) {
	restoreUmask := setUmaskPrivate()
	defer restoreUmask()
	if s == nil || s.Store == nil || s.Registry == nil {
		return errors.New("runtime: supervisor is not configured")
	}
	if ready == nil {
		return errors.New("runtime: readiness writer is nil")
	}
	readySent := false
	defer func() {
		if !readySent && resultErr != nil && supervisorIDPattern.MatchString(expectedID) {
			message := resultErr.Error()
			_ = WriteReady(ready, ReadyMessage{Version: ProtocolVersion, ID: expectedID, OK: false, Error: &message})
		}
		if closer, ok := ready.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	if !supervisorIDPattern.MatchString(expectedID) {
		return errors.New("runtime: expected ID must be 32 lowercase hexadecimal characters")
	}

	// Lock acquisition and durable runtime ownership.
	nameLock, err := s.Store.LockName(name)
	if err != nil {
		return err
	}
	nameLocked := true
	defer func() {
		if nameLocked {
			resultErr = errors.Join(resultErr, nameLock.Close())
		}
	}()
	config, err := nameLock.Load()
	if err != nil {
		return err
	}
	if config.ID != expectedID {
		return fmt.Errorf("runtime: immutable ID mismatch for %q", name)
	}
	paths := s.Store.Paths(config)
	if err := ensurePrivateDirectory(paths.RuntimeDir); err != nil {
		return fmt.Errorf("runtime: runtime directory: %w", err)
	}
	if err := ensurePrivateDirectory(filepath.Dir(paths.QEMULog)); err != nil {
		return fmt.Errorf("runtime: log directory: %w", err)
	}
	lifetime, acquired, err := nameLock.TryLockLifetime(config)
	if err != nil {
		return fmt.Errorf("runtime: lifetime lock: %w", err)
	}
	if !acquired {
		return errors.New("runtime: already running")
	}
	defer lifetime.Close()

	var (
		run             *supervisedRun
		sink            *serialLogSink
		monitor         *monitoringServer
		metricsListener *net.TCPListener
	)
	finalized := false
	startupIntent := make(chan struct{})
	defer func() {
		if finalized || resultErr == nil {
			return
		}
		intentionalStartup := false
		select {
		case <-startupIntent:
			intentionalStartup = true
		default:
		}
		monitor.stop()
		metricsListener = nil
		if run != nil {
			forceErr := run.force(context.Background())
			resultErr = errors.Join(resultErr, forceErr)
			if forceErr != nil {
				intentionalStartup = false
			}
		}
		if sink != nil {
			resultErr = errors.Join(resultErr, sink.abort())
		}
		cleanupErr := CleanupRuntime(paths)
		resultErr = errors.Join(resultErr, cleanupErr)
		if intentionalStartup && cleanupErr == nil {
			if err := WriteLastExit(paths.LastExitMetadata, LastExitMetadata{Version: metadataVersion, ID: expectedID, Timestamp: s.now(), ExitCode: 0}); err == nil {
				resultErr = nil
				return
			} else {
				resultErr = err
			}
		}
		resultErr = s.recordFailure(paths, expectedID, 1, resultErr)
	}()

	if err := CleanupRuntime(paths); err != nil {
		return fmt.Errorf("runtime: stale cleanup: %w", err)
	}
	config, err = nameLock.Load()
	if err != nil {
		return err
	}
	if config.ID != expectedID {
		return fmt.Errorf("runtime: immutable ID changed for %q", name)
	}
	paths = s.Store.Paths(config)

	debugf(options.Debug, options.DebugWriter, "preflight name=%q backend=%q runtime_dir=%q control_socket=%q qmp=%q qmp_command=%q qga=%q console=%q monitor=%q qemu_log=%q serial_log=%q serial_log_pipe=%q",
		config.Name,
		config.Backend,
		paths.RuntimeDir,
		paths.ControlSocket,
		paths.QMP,
		paths.QMPCommand,
		paths.QGA,
		paths.Console,
		paths.Monitor,
		paths.QEMULog,
		paths.SerialLog,
		paths.SerialLogPipe)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	lifecycleSignal := make(chan os.Signal, 1)
	startTimeout := s.StartTimeout
	if startTimeout <= 0 {
		startTimeout = defaultStartTimeout
	}
	startCtx, cancelStart := context.WithTimeout(ctx, startTimeout)
	defer cancelStart()
	go func() {
		select {
		case sig := <-signals:
			select {
			case lifecycleSignal <- sig:
			default:
			}
			close(startupIntent)
			cancelStart()
		case <-startCtx.Done():
		}
	}()

	// Runtime reservation and backend startup.
	if s.Preflight != nil {
		if err := s.Preflight(startCtx, config, paths); err != nil {
			return err
		}
	}
	metricsListener, err = listenMonitoring(config)
	if err != nil {
		return err
	}
	defer func() {
		if metricsListener != nil {
			_ = metricsListener.Close()
		}
	}()

	startedAt := s.now()
	debugf(options.Debug, options.DebugWriter, "runtime metadata started_at=%q boot_menu=%t", startedAt.Format(time.RFC3339Nano), options.BootMenu)
	if err := WriteRuntimeMetadata(paths.RuntimeMetadata, RuntimeMetadata{
		Version:       metadataVersion,
		ID:            config.ID,
		Name:          config.Name,
		SupervisorPID: os.Getpid(),
		StartedAt:     startedAt,
		BootMenu:      options.BootMenu,
	}); err != nil {
		return fmt.Errorf("runtime: write metadata: %w", err)
	}
	if file, ok := ready.(*os.File); ok {
		if err := closeOnExecFile(file); err != nil {
			return fmt.Errorf("runtime: readiness descriptor: %w", err)
		}
	}
	if err := os.Remove(paths.ControlSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("runtime: remove stale control socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.ControlSocket, Net: "unix"})
	if err != nil {
		return fmt.Errorf("runtime: bind control socket: %w", err)
	}
	defer func() {
		if listener != nil {
			_ = listener.Close()
		}
	}()
	if err := closeOnExec(listener); err != nil {
		return fmt.Errorf("runtime: control descriptor: %w", err)
	}
	if err := os.Chmod(paths.ControlSocket, 0o600); err != nil {
		return fmt.Errorf("runtime: protect control socket: %w", err)
	}
	implementation, err := s.Registry.Lookup(string(config.Backend))
	if err != nil {
		return err
	}
	runtimePaths := backendPaths(paths)
	debugf(options.Debug, options.DebugWriter, "backend=%q", config.Backend)
	command, err := implementation.Render(config, runtimePaths, backend.RenderOptions{BootMenu: options.BootMenu})
	if err != nil {
		return fmt.Errorf("qemu: render: %w", err)
	}
	debugf(options.Debug, options.DebugWriter, "rendered argv=%s extra_args_count=%d", formatManagedCommand(command, len(config.QEMU.ExtraArgs)), len(config.QEMU.ExtraArgs))
	warningWriter := options.DebugWriter
	if warningWriter == nil {
		warningWriter = io.Discard
	}
	sink, err = startSerialLogSink(paths.SerialLogPipe, paths.SerialLog, warningWriter)
	if err != nil {
		return err
	}
	instance, err := implementation.Start(startCtx, config, runtimePaths, command)
	if err != nil {
		return fmt.Errorf("qemu: start: %w", err)
	}
	run = newSupervisedRun(instance, time.Duration(config.ShutdownTimeoutSeconds)*time.Second)
	if err := nameLock.Close(); err != nil {
		return fmt.Errorf("runtime: release name lock: %w", err)
	}
	nameLocked = false
	if err := startCtx.Err(); err != nil {
		return fmt.Errorf("qemu: startup canceled: %w", err)
	}
	backendState, err := instance.Status(startCtx)
	if err != nil || (backendState != model.RunStateRunning && backendState != model.RunStatePaused) {
		if err == nil {
			err = fmt.Errorf("unexpected initial backend state %q", backendState)
		}
		return fmt.Errorf("qemu: readiness: %w", err)
	}
	hash, err := model.Hash(config)
	if err != nil {
		return fmt.Errorf("config: hash: %w", err)
	}
	debugf(options.Debug, options.DebugWriter, "backend_pid=%d backend_state=%q config_hash=%q", instance.PID(), backendState, hash)
	var vnc *backend.VNCEndpoint
	if endpoint, ok := instance.VNCEndpoint(); ok {
		endpoint := endpoint
		vnc = &endpoint
	}
	run.mu.Lock()
	run.status = Status{State: backendState, Backend: config.Backend, SupervisorPID: os.Getpid(), BackendPID: instance.PID(), StartedAt: startedAt, RunningConfigSHA256: hash, VNC: vnc}
	run.mu.Unlock()
	monitor, err = s.startMonitoring(startCtx, metricsListener, instance, config, startedAt, backendState, warningWriter)
	if err != nil {
		return err
	}
	select {
	case <-run.done:
		run.mu.Lock()
		exit := run.exit
		run.mu.Unlock()
		return fmt.Errorf("qemu: backend exited during startup with code %d: %v", exit.Code, exit.Err)
	default:
	}

	// Readiness and service publication.
	if err := ClearFailedLastExit(paths.LastExitMetadata); err != nil {
		return err
	}
	if err := startCtx.Err(); err != nil {
		return fmt.Errorf("qemu: startup canceled: %w", err)
	}
	run.mu.Lock()
	if run.terminal {
		exit := run.exit
		run.mu.Unlock()
		return fmt.Errorf("qemu: backend exited during startup with code %d: %v", exit.Code, exit.Err)
	}
	if err := WriteReady(ready, ReadyMessage{Version: ProtocolVersion, ID: expectedID, OK: true}); err != nil {
		run.mu.Unlock()
		return fmt.Errorf("runtime: report readiness: %w", err)
	}
	readySent = true
	if closer, ok := ready.(io.Closer); ok {
		_ = closer.Close()
	}
	debugf(options.Debug, options.DebugWriter, "ready id=%q backend_pid=%d", expectedID, instance.PID())
	run.mu.Unlock()
	control := s.startControlServer(listener, expectedID, run)

	// Terminal finalization.
	internalFailure := waitForTermination(ctx, signals, lifecycleSignal, run)
	if err := sink.finish(); err != nil {
		_, _ = fmt.Fprintf(warningWriter, "serial log: sink failed: %v\n", err)
	}
	monitor.stop()
	metricsListener = nil
	control.stop(run)
	listener = nil
	forceErr := run.completedForceError()
	resultErr = s.finalizeRun(paths, expectedID, run, internalFailure, forceErr, options)
	finalized = true
	return resultErr
}

func waitForTermination(ctx context.Context, signals, lifecycleSignal <-chan os.Signal, run *supervisedRun) (internalFailure bool) {
	select {
	case <-run.done:
	case <-ctx.Done():
		internalFailure = true
		_ = run.force(context.Background())
	case <-lifecycleSignal:
		if err := run.stop(context.Background(), false, run.defaultTimeout); err != nil {
			_ = run.force(context.Background())
		}
	case <-signals:
		if err := run.stop(context.Background(), false, run.defaultTimeout); err != nil {
			_ = run.force(context.Background())
		}
	}
	<-run.done
	return internalFailure
}

func terminalOutcome(exit backend.Exit, intentional, internalFailure bool, forceErr error) (exitCode int, text string) {
	exitCode = exit.Code
	if exit.Err != nil {
		text = exit.Err.Error()
	}
	if exit.Err != nil && exitCode == 0 {
		exitCode = 1
	}
	if internalFailure {
		return 1, "supervisor context canceled"
	}
	if forceErr != nil {
		return 1, forceErr.Error()
	}
	if intentional || (exit.Err == nil && exitCode == 0) {
		return 0, ""
	}
	if exitCode != 0 && text == "" {
		text = fmt.Sprintf("backend exited with code %d", exitCode)
	}
	return exitCode, text
}

func (s *Service) finalizeRun(paths store.Paths, id string, run *supervisedRun, internalFailure bool, forceErr error, options SuperviseOptions) error {
	run.mu.Lock()
	exit := run.exit
	intentional := run.intentional
	run.mu.Unlock()

	exitCode, text := terminalOutcome(exit, intentional, internalFailure, forceErr)
	cleanupErr := CleanupRuntime(paths)
	if cleanupErr != nil {
		exitCode = 1
		if text == "" {
			text = cleanupErr.Error()
		} else {
			text = errors.Join(errors.New(text), cleanupErr).Error()
		}
	}
	resultErr := error(nil)
	if err := WriteLastExit(paths.LastExitMetadata, LastExitMetadata{Version: metadataVersion, ID: id, Timestamp: s.now(), ExitCode: exitCode, Error: text}); err != nil {
		resultErr = fmt.Errorf("runtime: write last exit: %w", err)
	} else if exitCode != 0 {
		resultErr = fmt.Errorf("qemu: backend exited with code %d: %s", exitCode, text)
	}
	debugf(options.Debug, options.DebugWriter, "terminal exit_code=%d intentional=%t internal_failure=%t forced=%t", exitCode, intentional, internalFailure, forceErr != nil)
	return resultErr
}

func (s *Service) recordFailure(paths store.Paths, id string, code int, cause error) error {
	writeErr := WriteLastExit(paths.LastExitMetadata, LastExitMetadata{Version: metadataVersion, ID: id, Timestamp: s.now(), ExitCode: code, Error: cause.Error()})
	return errors.Join(cause, writeErr)
}

func (s *Service) now() time.Time {
	clock := s.Clock
	if clock == nil {
		clock = time.Now
	}
	return clock().UTC()
}

func backendPaths(paths store.Paths) backend.RuntimePaths {
	return backend.RuntimePaths{
		VMDir:         paths.VMDir,
		QMP:           paths.QMP,
		QMPCommand:    paths.QMPCommand,
		QGA:           paths.QGA,
		Console:       paths.Console,
		Monitor:       paths.Monitor,
		QEMULog:       paths.QEMULog,
		SerialLogPipe: paths.SerialLogPipe,
	}
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path is not a real directory")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("mode is %04o, want 0700", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("directory is not owned by current user")
	}
	return nil
}
