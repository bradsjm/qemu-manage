package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/monitoring"
)

func listenMonitoring(config *model.Config) (*net.TCPListener, error) {
	if config.Metrics == nil {
		return nil, nil
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(config.Metrics.Port)})
	if err != nil {
		return nil, fmt.Errorf("runtime: bind metrics endpoint: %w", err)
	}
	if err := closeOnExec(listener); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("runtime: metrics descriptor: %w", err)
	}
	return listener, nil
}

type monitoringServer struct {
	once          sync.Once
	cancel        context.CancelFunc
	server        *http.Server
	listener      *net.TCPListener
	collectorDone <-chan struct{}
	serverDone    <-chan struct{}
}

func (s *Service) startMonitoring(ctx context.Context, listener *net.TCPListener, instance backend.Instance, config *model.Config, startedAt time.Time, state model.RunState, warningWriter io.Writer) (*monitoringServer, error) {
	if listener == nil {
		return nil, nil
	}
	monitoringInstance, ok := instance.(backend.MonitoringInstance)
	if !ok {
		return nil, fmt.Errorf("runtime: monitoring is unsupported by backend %q", config.Backend)
	}
	if warningWriter == nil {
		warningWriter = io.Discard
	}
	monitorService := monitoring.New(monitoring.Options{
		Instance: monitoringInstance,
		PID:      instance.PID(),
		VM: monitoring.VMIdentity{
			ID:           config.ID,
			Name:         config.Name,
			Backend:      string(config.Backend),
			Architecture: config.Architecture,
			CPUs:         config.CPUs,
			MemoryMiB:    config.MemoryMiB,
			GuestAgent:   config.GuestAgent.Enabled,
			StartedAt:    startedAt,
			BuildVersion: s.BuildVersion,
		},
		Clock: s.Clock,
	})
	monitorService.Seed(ctx, string(state))
	monitorContext, cancel := context.WithCancel(context.Background())
	collectorDone := monitorService.Start(monitorContext)
	server := &http.Server{
		Handler:           monitorService.Handler(),
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(warningWriter, "monitoring: HTTP server failed: %v\n", serveErr)
		}
	}()
	return &monitoringServer{cancel: cancel, server: server, listener: listener, collectorDone: collectorDone, serverDone: serverDone}, nil
}

// stop synchronously shuts down the monitoring collector and HTTP server. It
// owns both goroutines for shutdown and does not return while either can still
// access the backend or publish observations derived from it.
func (m *monitoringServer) stop() {
	if m == nil {
		return
	}
	m.once.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		if m.server != nil {
			_ = m.server.Close()
		}
		if m.listener != nil {
			_ = m.listener.Close()
		}
		if m.collectorDone != nil {
			<-m.collectorDone
		}
		if m.serverDone != nil {
			<-m.serverDone
		}
	})
}
