package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
)

const (
	qmpRetryInterval         = 25 * time.Millisecond
	qgaResponsivenessTimeout = 2 * time.Second
	quitWaitTimeout          = 5 * time.Second
)

type instance struct {
	process *os.Process
	qgaPath string
	useQGA  bool
	qgaGate chan struct{}

	qmpMu sync.RWMutex
	qmp   *QMPClient

	vncHost string
	vncPort uint16
	hasVNC  bool

	done      chan struct{}
	exit      backend.Exit
	published chan backend.Exit

	forceOnce sync.Once
	forceDone chan struct{}
	forceErr  error
}

// Start executes command directly and waits until its QMP endpoint reports a
// state in which the VM can be managed.
func (b *Backend) Start(ctx context.Context, config *model.Config, paths backend.RuntimePaths, command backend.Command) (backend.Instance, error) {
	if config == nil {
		return nil, fmt.Errorf("qemu: config is nil")
	}
	if command.Path == "" {
		return nil, fmt.Errorf("qemu: command path is empty")
	}

	logFile, err := os.OpenFile(paths.QEMULog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("qemu: open log: %w", err)
	}
	if err := logFile.Chmod(0o600); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("qemu: secure log: %w", err)
	}

	secretPrepared := false
	cleanupPreparedSecret := func(primary error) error {
		if secretPrepared {
			primary = errors.Join(primary, b.removeVNCSecret(paths.VNCSecret))
		}
		return primary
	}
	if config.VNC != nil {
		if err := writeVNCSecret(paths.VNCSecret, config.VNC.Password); err != nil {
			_ = logFile.Close()
			return nil, err
		}
		secretPrepared = true
	}

	cmd := exec.Command(command.Path, command.Args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, cleanupPreparedSecret(fmt.Errorf("qemu: start: %w", err))
	}

	i := &instance{
		process:   cmd.Process,
		qgaPath:   paths.QGA,
		useQGA:    config.GuestAgent.Enabled,
		qgaGate:   make(chan struct{}, 1),
		done:      make(chan struct{}),
		published: make(chan backend.Exit, 1),
		forceDone: make(chan struct{}),
	}
	i.qgaGate <- struct{}{}
	go i.reap(cmd, logFile)

	startupFailure := func(primary error) error {
		primary = errors.Join(primary, i.ForceStop(context.Background()))
		return cleanupPreparedSecret(primary)
	}

	timeout := b.StartTimeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	qmp, err := waitForQMP(readyCtx, paths.QMP)
	if err != nil {
		return nil, startupFailure(fmt.Errorf("qemu: QMP readiness: %w", err))
	}
	i.qmpMu.Lock()
	select {
	case <-i.done:
		i.qmpMu.Unlock()
		_ = qmp.Close()
		return nil, startupFailure(fmt.Errorf("qemu: exited before QMP readiness"))
	default:
		i.qmp = qmp
		i.qmpMu.Unlock()
	}

	state, err := qmp.Status(readyCtx)
	if err != nil {
		return nil, startupFailure(fmt.Errorf("qemu: QMP readiness: %w", err))
	}
	if state != model.RunStateRunning && state != model.RunStatePaused {
		return nil, startupFailure(fmt.Errorf("qemu: QMP readiness returned state %q", state))
	}
	if config.VNC != nil {
		info, err := qmp.QueryVNC(readyCtx)
		if err != nil {
			return nil, startupFailure(fmt.Errorf("qemu: VNC readiness: %w", err))
		}
		if !info.Enabled {
			return nil, startupFailure(errors.New("qemu: VNC readiness: VNC is disabled"))
		}
		if info.Family != "ipv4" {
			return nil, startupFailure(fmt.Errorf("qemu: VNC readiness returned family %q", info.Family))
		}
		if info.Auth != "vnc" {
			return nil, startupFailure(fmt.Errorf("qemu: VNC readiness returned auth %q", info.Auth))
		}
		if info.Host != config.VNC.Bind {
			return nil, startupFailure(fmt.Errorf("qemu: VNC readiness returned host %q, want %q", info.Host, config.VNC.Bind))
		}
		port64, err := strconv.ParseUint(info.Service, 10, 16)
		if err != nil || port64 == 0 {
			return nil, startupFailure(fmt.Errorf("qemu: VNC readiness returned invalid service %q", info.Service))
		}
		port := uint16(port64)
		if port < config.VNC.Port || port > config.VNC.PortTo {
			return nil, startupFailure(fmt.Errorf("qemu: VNC readiness returned port %d outside %d-%d", port, config.VNC.Port, config.VNC.PortTo))
		}
		i.vncHost = info.Host
		i.vncPort = port
		i.hasVNC = true
	}
	if secretPrepared {
		if err := b.removeVNCSecret(paths.VNCSecret); err != nil {
			return nil, errors.Join(err, i.ForceStop(context.Background()))
		}
		secretPrepared = false
	}
	return i, nil
}

type qmpConnectResult struct {
	client *QMPClient
	err    error
}

func waitForQMP(ctx context.Context, path string) (*QMPClient, error) {
	for {
		result := make(chan qmpConnectResult, 1)
		go func() {
			client, err := NewQMPClient(path)
			result <- qmpConnectResult{client: client, err: err}
		}()

		select {
		case attempt := <-result:
			if attempt.err == nil {
				return attempt.client, nil
			}
		case <-ctx.Done():
			go func() {
				attempt := <-result
				if attempt.client != nil {
					_ = attempt.client.Close()
				}
			}()
			return nil, ctx.Err()
		}

		timer := time.NewTimer(qmpRetryInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

func writeVNCSecret(path, password string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("qemu: VNC secret path must be absolute")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("qemu: create VNC secret: %w", err)
	}
	cleanupPath := true
	defer func() {
		if cleanupPath {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("qemu: secure VNC secret: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("qemu: inspect VNC secret: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("qemu: VNC secret is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("qemu: VNC secret mode is %04o, want 0600", info.Mode().Perm())
	}
	if _, err := file.WriteString(password); err != nil {
		return fmt.Errorf("qemu: write VNC secret: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("qemu: sync VNC secret: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("qemu: close VNC secret: %w", err)
	}
	cleanupPath = false
	return nil
}

func (b *Backend) removeVNCSecret(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("qemu: VNC secret path must be absolute")
	}
	removeFile := os.Remove
	if b.removeFile != nil {
		removeFile = b.removeFile
	}
	if err := removeFile(path); err != nil {
		return fmt.Errorf("qemu: remove VNC secret: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("qemu: open directory for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("qemu: sync directory: %w", err)
	}
	return nil
}

func (i *instance) reap(cmd *exec.Cmd, logFile *os.File) {
	err := cmd.Wait()
	code := 0
	if cmd.ProcessState == nil {
		code = 1
	} else {
		code = cmd.ProcessState.ExitCode()
	}
	i.exit = backend.Exit{Code: code, Err: err}

	i.qmpMu.Lock()
	if i.qmp != nil {
		_ = i.qmp.Close()
	}
	_ = logFile.Close()
	close(i.done)
	i.qmpMu.Unlock()
	i.published <- i.exit
	close(i.published)
}

func (i *instance) PID() int {
	return i.process.Pid
}

func (i *instance) VNCEndpoint() (backend.VNCEndpoint, bool) {
	if !i.hasVNC {
		return backend.VNCEndpoint{}, false
	}
	return backend.VNCEndpoint{Host: i.vncHost, Port: i.vncPort}, true
}

func (i *instance) Status(ctx context.Context) (model.RunState, error) {
	qmp, err := i.qmpClient()
	if err != nil {
		return model.RunStateFailed, err
	}
	return qmp.Status(ctx)
}

func (i *instance) qgaCommand(ctx context.Context, request GuestAgentRequest) (json.RawMessage, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-i.qgaGate:
	}
	defer func() { i.qgaGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return GuestAgentCommand(ctx, i.qgaPath, request)
}

func (i *instance) qgaShutdown(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-i.qgaGate:
	}
	defer func() { i.qgaGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return GuestShutdown(ctx, i.qgaPath)
}

func (i *instance) RequestShutdown(ctx context.Context) error {
	if i.useQGA {
		qgaCtx, cancel := qgaResponsivenessContext(ctx)
		err := i.qgaShutdown(qgaCtx)
		cancel()
		if err == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	qmp, err := i.qmpClient()
	if err != nil {
		return err
	}
	return qmp.SystemPowerdown(ctx)
}

func qgaResponsivenessContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := qgaResponsivenessTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout*2 {
			timeout = remaining / 2
		}
	}
	return context.WithTimeout(ctx, timeout)
}

func (i *instance) ForceStop(ctx context.Context) error {
	i.forceOnce.Do(func() {
		defer close(i.forceDone)
		i.forceErr = i.forceStop(ctx)
	})
	<-i.forceDone
	return i.forceErr
}

func (i *instance) forceStop(ctx context.Context) error {
	select {
	case <-i.done:
		return nil
	default:
	}

	quitDelivered := false
	if qmp, err := i.qmpClient(); err == nil {
		quitCtx, cancel := context.WithTimeout(ctx, quitWaitTimeout)
		err = qmp.Quit(quitCtx)
		cancel()
		quitDelivered = err == nil
	}
	if quitDelivered {
		timer := time.NewTimer(quitWaitTimeout)
		select {
		case <-i.done:
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}

	killErr := i.process.Kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	<-i.done
	if killErr != nil {
		return fmt.Errorf("qemu: kill process: %w", killErr)
	}
	return nil
}

func (i *instance) Wait() <-chan backend.Exit {
	return i.published
}

func (i *instance) qmpClient() (*QMPClient, error) {
	i.qmpMu.RLock()
	defer i.qmpMu.RUnlock()
	if i.qmp == nil {
		return nil, fmt.Errorf("qemu: QMP is unavailable")
	}
	return i.qmp, nil
}
