package monitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

const (
	jsonContentType       = "application/json; charset=utf-8"
	prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"
	pingTimeout           = 2 * time.Second
)

type pingCall struct {
	done     chan struct{}
	probe    backend.GuestProbe
	duration time.Duration
	at       time.Time
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		s.writeJSON(response, request, http.StatusMethodNotAllowed, map[string]any{"api_version": 1, "ok": false, "code": "method_not_allowed", "message": "method must be GET or HEAD"})
		return
	}
	snapshot := s.Snapshot()
	if snapshot == nil {
		s.writeJSON(response, request, http.StatusServiceUnavailable, map[string]any{"api_version": 1, "ok": false, "code": "snapshot_unavailable", "message": "monitoring snapshot is unavailable"})
		return
	}
	switch request.URL.Path {
	case "/metrics":
		s.write(response, request, http.StatusOK, prometheusContentType, renderMetrics(snapshot))
	case "/health":
		status, payload := renderHealth(snapshot)
		s.writeJSON(response, request, status, payload)
	case "/status":
		s.writeJSON(response, request, http.StatusOK, renderStatus(snapshot, s.clock().UTC()))
	case "/ping":
		status, payload := s.renderPing(request.Context(), snapshot)
		s.writeJSON(response, request, status, payload)
	case "/info":
		s.writeJSON(response, request, http.StatusOK, renderInfo(snapshot))
	default:
		s.writeJSON(response, request, http.StatusNotFound, map[string]any{"api_version": 1, "ok": false, "code": "not_found", "message": "route not found"})
	}
}

func (s *Service) writeJSON(response http.ResponseWriter, request *http.Request, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		s.write(response, request, http.StatusInternalServerError, jsonContentType, []byte("{\"api_version\":1,\"ok\":false,\"code\":\"render_failed\",\"message\":\"response rendering failed\"}\n"))
		return
	}
	s.write(response, request, status, jsonContentType, append(body, '\n'))
}

func (*Service) write(response http.ResponseWriter, request *http.Request, status int, contentType string, body []byte) {
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Content-Length", strconv.Itoa(len(body)))
	response.WriteHeader(status)
	if request.Method == http.MethodHead {
		return
	}
	_, _ = response.Write(body)
}

func renderHealth(snapshot *Snapshot) (int, map[string]any) {
	qmp := snapshot.Collectors["qmp"]
	process := snapshot.Collectors["process"]
	status, code, httpStatus := healthPolicy(snapshot.QMP.State, qmp, process)
	payload := map[string]any{
		"api_version": 1, "ok": httpStatus != http.StatusServiceUnavailable, "status": status,
		"state": snapshot.QMP.State, "observed_at": formatTime(snapshot.ObservedAt),
		"checks": map[string]any{"qmp": healthCheck(qmp), "process": healthCheck(process)},
	}
	if code != "" {
		payload["code"] = code
	}
	return httpStatus, payload
}

func healthPolicy(state string, qmp, process CollectorState) (string, string, int) {
	if qmp.Status == CollectorStale {
		return "unhealthy", "qmp_stale", http.StatusServiceUnavailable
	}
	if qmp.Status != CollectorOK {
		return "unhealthy", "qmp_unavailable", http.StatusServiceUnavailable
	}
	switch state {
	case "running":
		if processStatsSupported && (process.Status == CollectorFailed || process.Status == CollectorStale) {
			return "degraded", "process_stats_unavailable", http.StatusOK
		}
		return "healthy", "", http.StatusOK
	case "paused", "suspended", "debug":
		return "degraded", "", http.StatusOK
	case "inmigrate", "internal-error", "io-error", "postmigrate", "prelaunch", "finish-migrate", "restore-vm", "save-vm", "shutdown", "watchdog", "guest-panicked", "colo":
		return "unhealthy", "state_" + state, http.StatusServiceUnavailable
	default:
		return "unhealthy", "unsupported_state", http.StatusServiceUnavailable
	}
}

func healthCheck(state CollectorState) map[string]any {
	value := map[string]any{"status": state.Status, "observed_at": nullableTime(state.ObservedAt)}
	if state.Code != "" {
		value["code"] = state.Code
	}
	return value
}

func renderStatus(snapshot *Snapshot, now time.Time) map[string]any {
	healthStatus, healthCode, _ := healthPolicy(snapshot.QMP.State, snapshot.Collectors["qmp"], snapshot.Collectors["process"])
	health := map[string]any{"status": healthStatus}
	if healthCode != "" {
		health["code"] = healthCode
	}
	payload := map[string]any{
		"api_version": 1, "observed_at": formatTime(snapshot.ObservedAt), "health": health,
		"vm":  map[string]any{"id": snapshot.VM.ID, "name": snapshot.VM.Name, "backend": snapshot.VM.Backend, "architecture": snapshot.VM.Architecture, "state": snapshot.QMP.State, "started_at": formatTime(snapshot.VM.StartedAt), "uptime_seconds": nonnegativeSeconds(now.Sub(snapshot.VM.StartedAt))},
		"qmp": renderQMPStatus(snapshot), "process": renderProcessStatus(snapshot),
		"block_devices": snapshot.QMP.Blocks, "guest_agent": renderGuestAgentStatus(snapshot),
		"collectors": renderCollectors(snapshot.Collectors, now),
	}
	if guest := renderGuestStatus(snapshot); guest != nil {
		payload["guest"] = guest
	}
	return payload
}

func renderQMPStatus(snapshot *Snapshot) map[string]any {
	state := snapshot.Collectors["qmp"]
	return map[string]any{"up": state.Status == CollectorOK, "state": snapshot.QMP.State, "duration_seconds": state.Duration.Seconds(), "observed_at": nullableTime(state.ObservedAt), "version": map[string]any{"major": snapshot.QMP.Version.Major, "minor": snapshot.QMP.Version.Minor, "micro": snapshot.QMP.Version.Micro, "package": snapshot.QMP.Version.Package}, "events": snapshot.QMP.Events}
}

func renderProcessStatus(snapshot *Snapshot) map[string]any {
	state := snapshot.Collectors["process"]
	value := map[string]any{"stats_up": state.Status == CollectorOK, "pid": snapshot.Process.PID, "observed_at": nullableTime(state.ObservedAt)}
	if !state.LastSuccess.IsZero() {
		stats := snapshot.Process.Stats
		value["cpu_seconds"] = map[string]float64{"user": stats.UserCPUSeconds, "system": stats.SystemCPUSeconds}
		value["resident_memory_bytes"] = stats.ResidentMemoryBytes
		value["wired_memory_bytes"] = stats.WiredMemoryBytes
		value["physical_footprint_bytes"] = stats.PhysicalFootprintBytes
		value["physical_footprint_peak_bytes"] = stats.PhysicalFootprintPeakBytes
		value["disk_read_bytes"] = stats.DiskReadBytes
		value["disk_written_bytes"] = stats.DiskWrittenBytes
		value["pageins"] = stats.PageIns
		value["idle_wakeups"] = stats.IdleWakeups
		value["interrupt_wakeups"] = stats.InterruptWakeups
		value["instructions"] = stats.Instructions
		value["cycles"] = stats.Cycles
		value["threads"] = stats.Threads
	}
	return value
}

func renderGuestAgentStatus(snapshot *Snapshot) map[string]any {
	info := snapshot.Collectors["guest_info"]
	value := map[string]any{"configured": snapshot.VM.GuestAgent, "up": info.Status == CollectorOK, "observed_at": nullableTime(info.ObservedAt)}
	if snapshot.Guest.Observation.Info.Version != "" {
		value["version"] = snapshot.Guest.Observation.Info.Version
	}
	if !snapshot.Guest.ProbeAt.IsZero() {
		value["probe_duration_seconds"] = snapshot.Guest.ProbeDuration.Seconds()
		value["probe_observed_at"] = formatTime(snapshot.Guest.ProbeAt)
	}
	return value
}

func renderGuestStatus(snapshot *Snapshot) map[string]any {
	guest := snapshot.Guest.Observation
	if guest.Load == nil && guest.ClockOffset == nil && guest.Frozen == nil && len(guest.CPU) == 0 && len(guest.VCPUs) == 0 && len(guest.Filesystems) == 0 && len(guest.Networks) == 0 && len(guest.Disks) == 0 {
		return nil
	}
	value := map[string]any{}
	if len(guest.CPU) != 0 {
		value["cpus"] = guest.CPU
	}
	if guest.Load != nil {
		value["load"] = guest.Load
	}
	if len(guest.VCPUs) != 0 {
		value["vcpus"] = guest.VCPUs
	}
	if guest.ClockOffset != nil {
		value["clock_offset_seconds"] = *guest.ClockOffset
	}
	if guest.Frozen != nil {
		value["filesystems_frozen"] = *guest.Frozen
	}
	if len(guest.Filesystems) != 0 {
		value["filesystems"] = guest.Filesystems
	}
	if len(guest.Networks) != 0 {
		value["network_interfaces"] = renderGuestNetworks(guest.Networks)
	}
	if len(guest.Disks) != 0 {
		value["disks"] = guest.Disks
	}
	return value
}

func renderGuestNetworks(networks []backend.GuestNetworkInterface) []map[string]any {
	result := make([]map[string]any, 0, len(networks))
	for _, network := range networks {
		value := map[string]any{"name": network.Name}
		if network.AddressesPresent {
			value["addresses"] = network.Addresses
		}
		statistics := map[string]any{}
		for name, sample := range map[string]*uint64{"receive_bytes": network.ReceiveBytes, "transmit_bytes": network.TransmitBytes, "receive_packets": network.ReceivePackets, "transmit_packets": network.TransmitPackets, "receive_errors": network.ReceiveErrors, "transmit_errors": network.TransmitErrors, "receive_dropped": network.ReceiveDropped, "transmit_dropped": network.TransmitDropped} {
			if sample != nil {
				statistics[name] = *sample
			}
		}
		if len(statistics) != 0 {
			value["statistics"] = statistics
		}
		result = append(result, value)
	}
	return result
}

func renderCollectors(collectors map[string]CollectorState, now time.Time) map[string]any {
	result := make(map[string]any, len(collectors))
	for key, state := range collectors {
		value := map[string]any{"status": state.Status, "age_seconds": nil}
		if state.Code != "" {
			value["code"] = state.Code
		}
		if !state.LastSuccess.IsZero() {
			value["age_seconds"] = nonnegativeSeconds(now.Sub(state.LastSuccess))
		}
		result[key] = value
	}
	return result
}

func renderInfo(snapshot *Snapshot) map[string]any {
	capabilities := map[string]bool{}
	for name, key := range map[string]string{"ping": "guest-ping", "cpu": "guest-get-cpustats", "load": "guest-get-load", "vcpus": "guest-get-vcpus", "clock": "guest-get-time", "filesystem_freeze": "guest-fsfreeze-status", "filesystems": "guest-get-fsinfo", "network": "guest-network-get-interfaces", "disk": "guest-get-diskstats"} {
		if enabled, ok := snapshot.Guest.Observation.Info.Capabilities[key]; ok {
			capabilities[name] = enabled
		}
	}
	routes := []string{"/health", "/info", "/metrics", "/ping", "/status"}
	sort.Strings(routes)
	return map[string]any{
		"api_version": 1,
		"service":     map[string]any{"name": "qemu-manage", "version": snapshot.VM.BuildVersion, "metrics_format": "prometheus", "metrics_format_version": "0.0.4"},
		"vm":          map[string]any{"id": snapshot.VM.ID, "name": snapshot.VM.Name, "backend": snapshot.VM.Backend, "architecture": snapshot.VM.Architecture, "cpus": snapshot.VM.CPUs, "memory_mib": snapshot.VM.MemoryMiB},
		"qemu":        map[string]any{"version": map[string]any{"major": snapshot.QMP.Version.Major, "minor": snapshot.QMP.Version.Minor, "micro": snapshot.QMP.Version.Micro, "package": snapshot.QMP.Version.Package}, "accelerator": "hvf"},
		"guest_agent": map[string]any{"configured": snapshot.VM.GuestAgent, "version": snapshot.Guest.Observation.Info.Version, "capabilities": capabilities}, "routes": routes,
	}
}

func (s *Service) renderPing(ctx context.Context, snapshot *Snapshot) (int, map[string]any) {
	now := s.clock().UTC()
	if !snapshot.VM.GuestAgent {
		return pingFailure(http.StatusConflict, "guest_agent_not_configured", "guest agent is not configured", now)
	}
	if enabled, known := snapshot.Guest.Observation.Info.Capabilities["guest-ping"]; known && !enabled {
		return pingFailure(http.StatusConflict, "guest_agent_command_disabled", "guest agent ping command is disabled", now)
	}
	call := s.joinPing(ctx)
	select {
	case <-ctx.Done():
		return pingFailure(http.StatusServiceUnavailable, "guest_agent_timeout", "guest agent ping timed out", s.clock().UTC())
	case <-call.done:
	}
	if call.probe.Result.OK() {
		return http.StatusOK, map[string]any{"api_version": 1, "ok": true, "guest_agent": "available", "round_trip_seconds": call.duration.Seconds(), "observed_at": formatTime(call.at)}
	}
	code := call.probe.Result.Code
	message := "guest agent is unavailable"
	if code == "guest_agent_timeout" {
		message = "guest agent ping timed out"
	}
	if code == "guest_agent_protocol_error" {
		message = "guest agent returned an invalid response"
	}
	return pingFailure(http.StatusServiceUnavailable, code, message, call.at)
}

func (s *Service) joinPing(requestContext context.Context) *pingCall {
	s.pingMu.Lock()
	if s.ping != nil {
		call := s.ping
		s.pingMu.Unlock()
		return call
	}
	call := &pingCall{done: make(chan struct{})}
	s.ping = call
	s.pingMu.Unlock()
	go func() {
		started := s.clock().UTC()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), pingTimeout)
		call.probe = s.instance.PingGuest(ctx)
		cancel()
		call.at = s.clock().UTC()
		call.duration = call.at.Sub(started)
		s.update(func(snapshot *Snapshot) {
			snapshot.Guest.ProbeAt, snapshot.Guest.ProbeDuration = call.at, call.duration
			snapshot.Collectors["guest_ping"] = collectorFromResult(call.probe.Result, started, call.at)
		})
		s.pingMu.Lock()
		s.ping = nil
		close(call.done)
		s.pingMu.Unlock()
	}()
	return call
}

func pingFailure(status int, code, message string, observedAt time.Time) (int, map[string]any) {
	return status, map[string]any{"api_version": 1, "ok": false, "code": code, "message": message, "observed_at": formatTime(observedAt)}
}
func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return formatTime(value)
}
func nonnegativeSeconds(duration time.Duration) float64 {
	if duration < 0 {
		return 0
	}
	return duration.Seconds()
}
func metricFloat(value float64) string { return strconv.FormatFloat(value, 'g', -1, 64) }
func metricUint(value uint64) string   { return strconv.FormatUint(value, 10) }
func metricLabel(value string) string  { return strconv.Quote(value) }
func metricSample(name string, labels map[string]string, value string) string {
	if len(labels) == 0 {
		return name + " " + value + "\n"
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	text := name + "{"
	for index, key := range keys {
		if index != 0 {
			text += ","
		}
		text += key + "=" + metricLabel(labels[key])
	}
	return text + "} " + value + "\n"
}
func metricHeader(name, help, kind string) string {
	return fmt.Sprintf("# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}
