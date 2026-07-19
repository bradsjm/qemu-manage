package lifecycle

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/store"
	"github.com/bradsjm/qemu-manage/internal/supervisor"
)

func lifecycleTestConfig(name string) *model.Config {
	return &model.Config{
		SchemaVersion:          model.SchemaVersion,
		ID:                     "0123456789abcdef0123456789abcdef",
		Name:                   name,
		Backend:                model.BackendQEMU,
		Architecture:           "aarch64",
		UUID:                   "123e4567-e89b-42d3-a456-426614174000",
		CPUs:                   2,
		MemoryMiB:              2048,
		RestartPolicy:          model.RestartNever,
		ShutdownTimeoutSeconds: 180,
		Firmware:               model.FirmwareConfig{Code: "code.fd", Variables: "vars.fd"},
		Disks:                  []model.DiskConfig{{Path: "disk.qcow2", Format: "qcow2", Serial: "disk0", BootIndex: 0}},
		Network:                model.NetworkConfig{Mode: model.NetworkUser, MAC: "02:00:00:00:00:01", Forwards: []model.PortForward{}},
		QEMU:                   model.QEMUConfig{Binary: "/usr/bin/false", ImageTool: "/usr/bin/false", Machine: "virt", ExtraArgs: []string{}},
		Autostart:              model.AutostartConfig{Scope: model.AutostartNone},
	}
}

func lifecycleFixture(t *testing.T) (*Service, *model.Config, store.Paths) {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(filepath.Join(root, "vms"), filepath.Join(root, "run"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := lifecycleTestConfig("vm")
	if err := st.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	return NewService(st), cfg, st.Paths(cfg)
}

func serveLifecycleStatus(t *testing.T, socketPath string, status supervisor.Status) <-chan error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		defer listener.Close()
		connection, err := listener.AcceptUnix()
		if err != nil {
			errCh <- err
			return
		}
		defer connection.Close()
		request, err := supervisor.DecodeRequest(connection)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- supervisor.EncodeResponse(connection, &supervisor.Response{
			Version: supervisor.ProtocolVersion,
			ID:      request.ID,
			OK:      true,
			Status:  &status,
		})
	}()
	return errCh
}

func TestStatusPropagatesSupervisorVNCEndpoint(t *testing.T) {
	service, cfg, paths := lifecycleFixture(t)
	status := supervisor.Status{
		State:               model.RunStateRunning,
		Backend:             model.BackendQEMU,
		SupervisorPID:       11,
		BackendPID:          22,
		StartedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		RunningConfigSHA256: strings.Repeat("b", 64),
		VNC:                 &backend.VNCEndpoint{Host: "127.0.0.1", Port: 5902},
	}
	errCh := serveLifecycleStatus(t, paths.ControlSocket, status)
	result, err := service.Status(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != model.RunStateRunning || result.PID != status.BackendPID || result.VNC == nil || *result.VNC != *status.VNC {
		t.Fatalf("result = %+v", result)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestStatusOmitsVNCEndpointWhenNotReady(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name      string
		setup     func(t *testing.T, service *Service, cfg *model.Config, paths store.Paths)
		wantState model.RunState
	}{
		{
			name:      "stopped without supervisor",
			setup:     func(t *testing.T, service *Service, cfg *model.Config, paths store.Paths) {},
			wantState: model.RunStateStopped,
		},
		{
			name: "starting without supervisor",
			setup: func(t *testing.T, service *Service, cfg *model.Config, paths store.Paths) {
				service.Clock = func() time.Time { return now }
				nameLock, err := service.Store.LockName(cfg.Name)
				if err != nil {
					t.Fatal(err)
				}
				lifetime, err := nameLock.LockLifetime(cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lifetime.Close() })
				if err := nameLock.Close(); err != nil {
					t.Fatal(err)
				}
				if err := supervisor.WriteRuntimeMetadata(paths.RuntimeMetadata, supervisor.RuntimeMetadata{
					Version:       1,
					ID:            cfg.ID,
					Name:          cfg.Name,
					SupervisorPID: 99,
					StartedAt:     now.Add(-5 * time.Second),
				}); err != nil {
					t.Fatal(err)
				}
			},
			wantState: model.RunStateStarting,
		},
		{
			name: "failed without supervisor",
			setup: func(t *testing.T, service *Service, cfg *model.Config, paths store.Paths) {
				if err := supervisor.WriteLastExit(paths.LastExitMetadata, supervisor.LastExitMetadata{
					Version:   1,
					ID:        cfg.ID,
					Timestamp: now,
					ExitCode:  1,
					Error:     "boom",
				}); err != nil {
					t.Fatal(err)
				}
			},
			wantState: model.RunStateFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, cfg, paths := lifecycleFixture(t)
			service.Clock = func() time.Time { return now }
			tt.setup(t, service, cfg, paths)
			result, err := service.Status(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if result.State != tt.wantState {
				t.Fatalf("state = %q, want %q", result.State, tt.wantState)
			}
			if result.VNC != nil {
				t.Fatalf("unexpected vnc = %+v", *result.VNC)
			}
		})
	}
}
