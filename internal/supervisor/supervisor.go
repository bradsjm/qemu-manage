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
	"sync"
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
}

func NewService(st *store.Store, registry *backend.Registry) *Service {
	return &Service{Store: st, Registry: registry, Clock: time.Now, StartTimeout: defaultStartTimeout}
}

type SuperviseOptions struct {
	BootMenu    bool
	Debug       bool
	DebugWriter io.Writer
}

type gracefulAttempt struct {
	done     chan struct{}
	request  chan struct{}
	err      error
	accepted bool
}

type supervisedRun struct {
	mu             sync.Mutex
	instance       backend.Instance
	status         Status
	exit           backend.Exit
	done           chan struct{}
	defaultTimeout time.Duration
	intentional    bool
	stopping       bool
	terminal       bool

	graceful       *gracefulAttempt
	gracefulCancel context.CancelFunc

	forceStarted bool
	forceDone    chan struct{}
	forceErr     error
	handlers     sync.WaitGroup
	connections  map[*net.UnixConn]bool
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
		run  *supervisedRun
		sink *serialLogSink
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

	if s.Preflight != nil {
		if err := s.Preflight(startCtx, config, paths); err != nil {
			return err
		}
	}

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
	defer listener.Close()
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
	select {
	case <-run.done:
		run.mu.Lock()
		exit := run.exit
		run.mu.Unlock()
		return fmt.Errorf("qemu: backend exited during startup with code %d: %v", exit.Code, exit.Err)
	default:
	}
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

	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() {
		s.serve(serveCtx, listener, expectedID, run)
		close(serveDone)
	}()
	internalFailure := false
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
	if err := sink.finish(); err != nil {
		_, _ = fmt.Fprintf(warningWriter, "serial log: sink failed: %v\n", err)
	}
	cancelServe()
	_ = listener.Close()
	<-serveDone
	run.closeNonStopConnections()
	run.handlers.Wait()
	forceErr := run.completedForceError()

	run.mu.Lock()
	exit := run.exit
	intentional := run.intentional
	run.mu.Unlock()
	exitCode := exit.Code
	text := ""
	if exit.Err != nil {
		text = exit.Err.Error()
	}
	if exit.Err != nil && exitCode == 0 {
		exitCode = 1
	}
	if internalFailure {
		exitCode, text = 1, "supervisor context canceled"
	} else if forceErr != nil {
		exitCode, text = 1, forceErr.Error()
	} else if intentional || (exit.Err == nil && exitCode == 0) {
		exitCode, text = 0, ""
	} else if exitCode != 0 && text == "" {
		text = fmt.Sprintf("backend exited with code %d", exitCode)
	}
	cleanupErr := CleanupRuntime(paths)
	if cleanupErr != nil {
		exitCode = 1
		if text == "" {
			text = cleanupErr.Error()
		} else {
			text = errors.Join(errors.New(text), cleanupErr).Error()
		}
	}
	if err := WriteLastExit(paths.LastExitMetadata, LastExitMetadata{Version: metadataVersion, ID: expectedID, Timestamp: s.now(), ExitCode: exitCode, Error: text}); err != nil {
		resultErr = fmt.Errorf("runtime: write last exit: %w", err)
	} else if exitCode != 0 {
		resultErr = fmt.Errorf("qemu: backend exited with code %d: %s", exitCode, text)
	}
	debugf(options.Debug, options.DebugWriter, "terminal exit_code=%d intentional=%t internal_failure=%t forced=%t", exitCode, intentional, internalFailure, forceErr != nil)
	finalized = true
	return resultErr
}

func newSupervisedRun(instance backend.Instance, timeout time.Duration) *supervisedRun {
	r := &supervisedRun{
		instance: instance, done: make(chan struct{}), defaultTimeout: timeout,
		forceDone: make(chan struct{}), connections: make(map[*net.UnixConn]bool),
	}
	go func() {
		exit, ok := <-instance.Wait()
		if !ok {
			exit = backend.Exit{Code: 1, Err: errors.New("backend wait channel closed without an exit result")}
		}
		r.mu.Lock()
		r.exit = exit
		r.terminal = true
		close(r.done)
		r.mu.Unlock()
	}()
	return r
}

func (s *Service) serve(ctx context.Context, listener *net.UnixListener, id string, run *supervisedRun) {
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		run.trackConnection(connection)
		go func() {
			defer run.untrackConnection(connection)
			s.handleConnection(connection, id, run)
		}()
	}
}

func (s *Service) handleConnection(connection *net.UnixConn, id string, run *supervisedRun) {
	defer connection.Close()
	uid, err := peerUID(connection)
	if err != nil || uid != uint32(os.Getuid()) {
		_ = EncodeResponse(connection, failure(id, ErrorUnauthorized, "control connection is not authorized"))
		return
	}
	request, err := DecodeRequest(connection)
	if err != nil {
		_ = EncodeResponse(connection, failure(id, ErrorInvalidRequest, err.Error()))
		return
	}
	if request.ID != id {
		_ = EncodeResponse(connection, failure(id, ErrorInvalidRequest, "request ID does not match running VM"))
		return
	}
	if request.Command == CommandStop {
		run.markStopConnection(connection)
	}
	switch request.Command {
	case CommandStatus:
		status, err := run.currentStatus(context.Background())
		if err != nil {
			_ = EncodeResponse(connection, failure(id, ErrorInternal, err.Error()))
			return
		}
		_ = EncodeResponse(connection, &Response{Version: ProtocolVersion, ID: id, OK: true, Status: &status})
	case CommandStop:
		timeout := run.defaultTimeout
		if request.TimeoutSeconds != nil {
			timeout = time.Duration(*request.TimeoutSeconds) * time.Second
		}
		err := run.stop(context.Background(), request.Force, timeout)
		if err != nil {
			code := ErrorInternal
			if errors.Is(err, errShutdownTimeout) {
				code = ErrorShutdownTimeout
			}
			_ = EncodeResponse(connection, failure(id, code, err.Error()))
			return
		}
		_ = EncodeResponse(connection, &Response{Version: ProtocolVersion, ID: id, OK: true})
	}
}

func (r *supervisedRun) currentStatus(ctx context.Context) (Status, error) {
	r.mu.Lock()
	if r.stopping {
		status := r.status
		r.mu.Unlock()
		return status, nil
	}
	r.mu.Unlock()
	state, err := r.instance.Status(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("qemu: status: %w", err)
	}
	r.mu.Lock()
	if !r.stopping {
		r.status.State = state
	}
	status := r.status
	r.mu.Unlock()
	return status, nil
}

var errShutdownTimeout = errors.New("graceful shutdown timed out")

func (r *supervisedRun) stop(ctx context.Context, force bool, timeout time.Duration) error {
	if force {
		return r.force(ctx)
	}
	r.mu.Lock()
	if r.forceStarted {
		done := r.forceDone
		r.mu.Unlock()
		return r.waitForce(ctx, done)
	}
	attempt := r.graceful
	if attempt == nil {
		attempt = &gracefulAttempt{done: make(chan struct{}), request: make(chan struct{})}
		r.graceful = attempt
		deadlineCtx, cancel := context.WithTimeout(context.Background(), timeout)
		r.gracefulCancel = cancel
		go r.runGraceful(deadlineCtx, attempt)
	}
	forceDone := r.forceDone
	r.mu.Unlock()
	select {
	case <-attempt.done:
		r.mu.Lock()
		err := attempt.err
		r.mu.Unlock()
		return err
	case <-forceDone:
		return r.waitForce(ctx, forceDone)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *supervisedRun) runGraceful(ctx context.Context, attempt *gracefulAttempt) {
	err := r.instance.RequestShutdown(ctx)
	r.mu.Lock()
	attempt.accepted = err == nil
	close(attempt.request)
	if r.forceStarted {
		forceDone := r.forceDone
		r.mu.Unlock()
		<-forceDone
		r.mu.Lock()
		attempt.err = r.forceErr
		close(attempt.done)
		r.mu.Unlock()
		return
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = errShutdownTimeout
		}
		attempt.err = err
		if r.graceful == attempt {
			r.graceful = nil
			r.gracefulCancel = nil
		}
		close(attempt.done)
		r.mu.Unlock()
		return
	}
	r.intentional = true
	r.stopping = true
	r.status.State = model.RunStateStopping
	r.mu.Unlock()

	select {
	case <-r.done:
	case <-ctx.Done():
		r.mu.Lock()
		forced := r.forceStarted
		forceDone := r.forceDone
		r.mu.Unlock()
		if forced {
			<-forceDone
			r.mu.Lock()
			err = r.forceErr
			r.mu.Unlock()
		} else {
			err = errShutdownTimeout
		}
	case <-r.forceDone:
		r.mu.Lock()
		err = r.forceErr
		r.mu.Unlock()
	}
	r.mu.Lock()
	attempt.err = err
	close(attempt.done)
	r.mu.Unlock()
}

func (r *supervisedRun) force(ctx context.Context) error {
	r.mu.Lock()
	if !r.forceStarted {
		r.forceStarted = true
		r.intentional = true
		r.stopping = true
		r.status.State = model.RunStateStopping
		requestDone := (<-chan struct{})(nil)
		if r.graceful != nil {
			requestDone = r.graceful.request
		}
		if r.gracefulCancel != nil {
			r.gracefulCancel()
		}
		go func() {
			if requestDone != nil {
				<-requestDone
			}
			err := r.instance.ForceStop(context.Background())
			<-r.done
			r.mu.Lock()
			r.forceErr = err
			close(r.forceDone)
			r.mu.Unlock()
		}()
	}
	done := r.forceDone
	r.mu.Unlock()
	return r.waitForce(ctx, done)
}

func (r *supervisedRun) waitForce(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		r.mu.Lock()
		err := r.forceErr
		r.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *supervisedRun) completedForceError() error {
	r.mu.Lock()
	started := r.forceStarted
	done := r.forceDone
	r.mu.Unlock()
	if !started {
		return nil
	}
	<-done
	r.mu.Lock()
	err := r.forceErr
	r.mu.Unlock()
	return err
}

func (r *supervisedRun) trackConnection(connection *net.UnixConn) {
	r.mu.Lock()
	r.connections[connection] = false
	r.handlers.Add(1)
	r.mu.Unlock()
}

func (r *supervisedRun) markStopConnection(connection *net.UnixConn) {
	r.mu.Lock()
	if _, ok := r.connections[connection]; ok {
		r.connections[connection] = true
	}
	r.mu.Unlock()
}

func (r *supervisedRun) untrackConnection(connection *net.UnixConn) {
	r.mu.Lock()
	delete(r.connections, connection)
	r.mu.Unlock()
	r.handlers.Done()
}

func (r *supervisedRun) closeNonStopConnections() {
	r.mu.Lock()
	for connection, stop := range r.connections {
		if !stop {
			_ = connection.Close()
		}
	}
	r.mu.Unlock()
}

func failure(id string, code ErrorCode, message string) *Response {
	return &Response{Version: ProtocolVersion, ID: id, OK: false, Error: &ProtocolError{Code: code, Message: message}}
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
