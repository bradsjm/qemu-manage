package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/store"
	"github.com/bradsjm/qemu-manage/internal/supervisor"
)

const startupObservationWindow = 15 * time.Second

// Service observes VM lifecycle state through the authenticated supervisor and
// the lifetime lock it owns for the duration of a run.
type Service struct {
	Store *store.Store
	Clock func() time.Time
}

func NewService(st *store.Store) *Service {
	return &Service{Store: st, Clock: time.Now}
}

// StatusResult is the lifecycle information available to the CLI. PID is the
// backend PID reported by the authenticated supervisor, never a persisted or
// probed process identifier.
type StatusResult struct {
	Name                string
	State               model.RunState
	PID                 int
	Backend             model.Backend
	CurrentConfigSHA256 string
	RunningConfigSHA256 string
	VNC                 *backend.VNCEndpoint
	Error               string
}

// Status returns the supervisor's authenticated view when it is reachable and
// otherwise derives a conservative state solely from strict metadata and the
// lifetime lock.
func (s *Service) Status(ctx context.Context, cfg *model.Config) (StatusResult, error) {
	result, _, err := s.status(ctx, cfg)
	return result, err
}

func (s *Service) status(ctx context.Context, cfg *model.Config) (StatusResult, bool, error) {
	if s == nil || s.Store == nil {
		return StatusResult{}, false, errors.New("runtime: lifecycle service has no store")
	}
	if cfg == nil {
		return StatusResult{}, false, errors.New("runtime: nil config")
	}
	currentHash, err := model.Hash(cfg)
	if err != nil {
		return StatusResult{}, false, fmt.Errorf("config: hash current config: %w", err)
	}
	result := StatusResult{
		Name:                cfg.Name,
		Backend:             cfg.Backend,
		CurrentConfigSHA256: currentHash,
	}
	paths := s.Store.Paths(cfg)
	response, controlErr := supervisor.Control(ctx, paths.ControlSocket, supervisor.Request{
		Version: supervisor.ProtocolVersion,
		ID:      cfg.ID,
		Command: supervisor.CommandStatus,
	})
	if controlErr == nil {
		if response.Status == nil {
			return result, false, errors.New("runtime: authenticated supervisor returned no status")
		}
		result.State = response.Status.State
		result.PID = response.Status.BackendPID
		result.Backend = response.Status.Backend
		result.RunningConfigSHA256 = response.Status.RunningConfigSHA256
		result.VNC = response.Status.VNC
		return result, true, nil
	}
	var protocolErr *supervisor.ResponseError
	if errors.As(controlErr, &protocolErr) {
		return result, true, fmt.Errorf("runtime: authenticated supervisor status: %w", controlErr)
	}
	if ctx.Err() != nil {
		return result, false, fmt.Errorf("runtime: status canceled: %w", ctx.Err())
	}
	if !controlUnavailable(controlErr) {
		return result, false, fmt.Errorf("runtime: invalid supervisor status exchange: %w", controlErr)
	}

	lock, acquired, err := s.Store.TryLifetime(cfg.ID)
	if err != nil {
		return result, false, fmt.Errorf("runtime: inspect lifetime lock: %w", err)
	}
	if acquired {
		if err := lock.Close(); err != nil {
			return result, false, fmt.Errorf("runtime: release lifetime lock: %w", err)
		}
		lastExit, err := supervisor.ReadLastExit(paths.LastExitMetadata)
		if errors.Is(err, fs.ErrNotExist) {
			result.State = model.RunStateStopped
			return result, false, nil
		}
		if err != nil {
			result.State = model.RunStateFailed
			result.Error = fmt.Sprintf("invalid last-exit metadata: %v", err)
			return result, false, nil
		}
		if lastExit.ID != cfg.ID {
			result.State = model.RunStateFailed
			result.Error = fmt.Sprintf("last-exit metadata ID %q does not match config ID %q", lastExit.ID, cfg.ID)
			return result, false, nil
		}
		if lastExit.ExitCode == 0 {
			result.State = model.RunStateStopped
			return result, false, nil
		}
		result.State = model.RunStateFailed
		result.Error = lastExit.Error
		return result, false, nil
	}

	result.State = model.RunStateFailed
	runtimeMetadata, err := supervisor.ReadRuntimeMetadata(paths.RuntimeMetadata)
	if err != nil {
		result.Error = fmt.Sprintf("lifetime lock is held but runtime metadata is invalid: %v", err)
		return result, true, nil
	}
	if runtimeMetadata.ID != cfg.ID || runtimeMetadata.Name != cfg.Name {
		result.Error = fmt.Sprintf("lifetime lock is held but runtime metadata identity is %q/%q, want %q/%q", runtimeMetadata.Name, runtimeMetadata.ID, cfg.Name, cfg.ID)
		return result, true, nil
	}
	now := time.Now()
	if s.Clock != nil {
		now = s.Clock()
	}
	if runtimeMetadata.StartedAt.After(now) {
		result.Error = fmt.Sprintf("runtime metadata started_at %s is in the future", runtimeMetadata.StartedAt.Format(time.RFC3339Nano))
		return result, true, nil
	}
	if now.Before(runtimeMetadata.StartedAt.Add(startupObservationWindow)) {
		result.State = model.RunStateStarting
		result.Error = ""
		return result, true, nil
	}
	result.Error = "supervisor holds the lifetime lock but authenticated control is unavailable after the startup window"
	return result, true, nil
}

// DeleteAllowed permits deletion only when the lifetime lock proves there is no
// live supervisor. Persisted failure evidence remains visible through Status,
// but does not make an offline VM undeletable.
func (s *Service) DeleteAllowed(ctx context.Context, cfg *model.Config) error {
	result, lockHeld, err := s.status(ctx, cfg)
	if err != nil {
		return err
	}
	if lockHeld {
		return fmt.Errorf("runtime: VM %q is %s; stop it and restore authenticated supervisor control before deleting", cfg.Name, result.State)
	}
	switch result.State {
	case model.RunStateStopped, model.RunStateFailed:
		return nil
	default:
		return fmt.Errorf("runtime: VM %q is %s; stop it before deleting", cfg.Name, result.State)
	}
}

func controlUnavailable(err error) bool {
	var networkErr *net.OpError
	return errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
