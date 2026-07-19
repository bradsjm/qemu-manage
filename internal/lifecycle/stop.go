package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/supervisor"
)

// InvalidStopTimeoutError reports a stop timeout that cannot be represented by
// the supervisor protocol.
type InvalidStopTimeoutError struct {
	Timeout time.Duration
}

func (e *InvalidStopTimeoutError) Error() string {
	return fmt.Sprintf("runtime: stop timeout %s must be a whole number of seconds from 1s through 1h", e.Timeout)
}

// ShutdownTimeoutError means the authenticated supervisor accepted the stop
// request, but the guest did not shut down before the requested deadline.
type ShutdownTimeoutError struct {
	Message string
}

func (e *ShutdownTimeoutError) Error() string {
	if e.Message == "" {
		return "runtime: shutdown timed out"
	}
	return "runtime: shutdown timed out: " + e.Message
}

// TransportError means lifecycle control could not reach or exchange a valid
// response with the authenticated supervisor.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return "runtime: supervisor control: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// SupervisorError preserves a non-timeout error returned by the authenticated
// supervisor.
type SupervisorError struct {
	Code    supervisor.ErrorCode
	Message string
}

func (e *SupervisorError) Error() string {
	return fmt.Sprintf("runtime: supervisor %s: %s", e.Code, e.Message)
}

// IsShutdownTimeout reports whether err is an authenticated shutdown timeout.
func IsShutdownTimeout(err error) bool {
	var target *ShutdownTimeoutError
	return errors.As(err, &target)
}

// IsTransportError reports whether err is a supervisor transport failure.
func IsTransportError(err error) bool {
	var target *TransportError
	return errors.As(err, &target)
}

// Stop asks the authenticated supervisor to stop cfg. It never signals a PID
// or modifies runtime metadata.
func (s *Service) Stop(ctx context.Context, cfg *model.Config, timeout time.Duration, force bool) error {
	if s == nil || s.Store == nil {
		return errors.New("runtime: lifecycle service has no store")
	}
	if cfg == nil {
		return errors.New("runtime: cannot stop a nil config")
	}

	timeoutSeconds, err := stopTimeoutSeconds(cfg, timeout)
	if err != nil {
		return err
	}

	_, err = supervisor.Control(ctx, s.Store.Paths(cfg).ControlSocket, supervisor.Request{
		Version:        supervisor.ProtocolVersion,
		ID:             cfg.ID,
		Command:        supervisor.CommandStop,
		Force:          force,
		TimeoutSeconds: &timeoutSeconds,
	})
	if err == nil {
		return nil
	}
	var responseErr *supervisor.ResponseError
	if !errors.As(err, &responseErr) {
		lock, acquired, lockErr := s.Store.TryLifetime(cfg.ID)
		if lockErr != nil {
			return &TransportError{Err: errors.Join(err, fmt.Errorf("inspect lifetime lock: %w", lockErr))}
		}
		if !acquired {
			return &TransportError{Err: err}
		}
		if closeErr := lock.Close(); closeErr != nil {
			return &TransportError{Err: errors.Join(err, fmt.Errorf("release lifetime lock: %w", closeErr))}
		}
		return nil
	}
	switch responseErr.Code {
	case supervisor.ErrorShutdownTimeout:
		return &ShutdownTimeoutError{Message: responseErr.Message}
	case supervisor.ErrorNotRunning:
		return nil
	default:
		return &SupervisorError{Code: responseErr.Code, Message: responseErr.Message}
	}
}

func stopTimeoutSeconds(cfg *model.Config, timeout time.Duration) (int, error) {
	if timeout == 0 {
		if cfg.ShutdownTimeoutSeconds < 1 || cfg.ShutdownTimeoutSeconds > 3600 {
			return 0, &InvalidStopTimeoutError{Timeout: time.Duration(cfg.ShutdownTimeoutSeconds) * time.Second}
		}
		return cfg.ShutdownTimeoutSeconds, nil
	}
	if timeout < time.Second || timeout > time.Hour || timeout%time.Second != 0 {
		return 0, &InvalidStopTimeoutError{Timeout: timeout}
	}
	return int(timeout / time.Second), nil
}
