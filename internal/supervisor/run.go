package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
)

// gracefulAttempt tracks one graceful shutdown attempt, including the request handoff and optional guest-agent path
type gracefulAttempt struct {
	done     chan struct{}
	request  chan struct{}
	err      error
	accepted bool
}

// supervisedRun tracks the full lifecycle of one supervised backend child, from startup state through shutdown and exit
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
	return r.stopWithProgress(ctx, force, timeout, nil)
}

func (r *supervisedRun) stopWithProgress(ctx context.Context, force bool, timeout time.Duration, onProgress func(StopProgress)) error {
	if force {
		if onProgress != nil {
			onProgress(StopProgressForcing)
		}
		return r.force(ctx)
	}
	r.mu.Lock()
	if r.forceStarted {
		done := r.forceDone
		r.mu.Unlock()
		if onProgress != nil {
			onProgress(StopProgressForcing)
		}
		return r.waitForce(ctx, done)
	}
	// Share one graceful attempt across callers so the backend sees at most one
	// QGA/ACPI shutdown request before anyone escalates to a hard stop.
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

	// Wait for the graceful request to be accepted, unless another caller has
	// already escalated to the forced stop path.
	select {
	case <-attempt.request:
		r.mu.Lock()
		accepted := attempt.accepted
		r.mu.Unlock()
		if accepted && onProgress != nil {
			onProgress(StopProgressAcknowledged)
		}
	case <-forceDone:
		if onProgress != nil {
			onProgress(StopProgressForcing)
		}
		return r.waitForce(ctx, forceDone)
	case <-ctx.Done():
		return ctx.Err()
	}

	// Once the graceful request is in flight, wait for completion unless the
	// shutdown has to escalate to the hard-stop path.
	select {
	case <-attempt.done:
		r.mu.Lock()
		err := attempt.err
		r.mu.Unlock()
		return err
	case <-forceDone:
		if onProgress != nil {
			onProgress(StopProgressForcing)
		}
		return r.waitForce(ctx, forceDone)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *supervisedRun) runGraceful(ctx context.Context, attempt *gracefulAttempt) {
	// RequestShutdown tries the guest agent when available and otherwise falls
	// back to QMP system_powerdown for the ACPI path.
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
			// Once the graceful handoff is settled, ForceStop owns the backend's
			// final hard-stop escalation.
			if requestDone != nil {
				// An in-flight graceful RequestShutdown must publish/close
				// attempt.request before ForceStop begins. That handoff keeps
				// guest-agent/QMP shutdown work from overlapping child teardown.
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

// trackConnection records live control connections so shutdown can close any
// non-stop clients before waiting for handlers to exit
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

// untrackConnection removes a finished control connection from the shutdown set
func (r *supervisedRun) untrackConnection(connection *net.UnixConn) {
	r.mu.Lock()
	delete(r.connections, connection)
	r.mu.Unlock()
	r.handlers.Done()
}

// closeNonStopConnections drops non-stop control clients so server shutdown does
// not hang on idle status connections
func (r *supervisedRun) closeNonStopConnections() {
	r.mu.Lock()
	for connection, stop := range r.connections {
		if !stop {
			_ = connection.Close()
		}
	}
	r.mu.Unlock()
}
