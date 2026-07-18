package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"qemu-manage/internal/backend"
	"qemu-manage/internal/model"
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

	qmpMu sync.RWMutex
	qmp   *QMPClient

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

	cmd := exec.Command(command.Path, command.Args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("qemu: start: %w", err)
	}

	i := &instance{
		process:   cmd.Process,
		qgaPath:   paths.QGA,
		useQGA:    config.GuestAgent.Enabled,
		done:      make(chan struct{}),
		published: make(chan backend.Exit, 1),
		forceDone: make(chan struct{}),
	}
	go i.reap(cmd, logFile)

	timeout := b.StartTimeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	qmp, err := waitForQMP(readyCtx, paths.QMP)
	if err != nil {
		_ = i.ForceStop(context.Background())
		return nil, fmt.Errorf("qemu: QMP readiness: %w", err)
	}
	i.qmpMu.Lock()
	select {
	case <-i.done:
		i.qmpMu.Unlock()
		_ = qmp.Close()
		return nil, fmt.Errorf("qemu: exited before QMP readiness")
	default:
		i.qmp = qmp
		i.qmpMu.Unlock()
	}

	state, err := qmp.Status(readyCtx)
	if err != nil || (state != model.RunStateRunning && state != model.RunStatePaused) {
		_ = i.ForceStop(context.Background())
		if err != nil {
			return nil, fmt.Errorf("qemu: QMP readiness: %w", err)
		}
		return nil, fmt.Errorf("qemu: QMP readiness returned state %q", state)
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

func (i *instance) Status(ctx context.Context) (model.RunState, error) {
	qmp, err := i.qmpClient()
	if err != nil {
		return model.RunStateFailed, err
	}
	return qmp.Status(ctx)
}

func (i *instance) RequestShutdown(ctx context.Context) error {
	if i.useQGA {
		qgaCtx, cancel := qgaResponsivenessContext(ctx)
		err := GuestShutdown(qgaCtx, i.qgaPath)
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
