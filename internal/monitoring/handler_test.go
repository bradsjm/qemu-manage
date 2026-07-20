package monitoring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

type handlerMonitoringInstance struct {
	mu          sync.Mutex
	pingCalls   int
	pingStarted chan struct{}
	pingRelease chan struct{}
	probe       backend.GuestProbe
}

func (*handlerMonitoringInstance) CollectQEMU(context.Context) backend.QEMUObservation {
	return backend.QEMUObservation{}
}
func (*handlerMonitoringInstance) CollectGuest(context.Context) backend.GuestObservation {
	return backend.GuestObservation{}
}
func (f *handlerMonitoringInstance) PingGuest(context.Context) backend.GuestProbe {
	f.mu.Lock()
	f.pingCalls++
	started, release := f.pingStarted, f.pingRelease
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return f.probe
}

func testHTTPService(t *testing.T) *Service {
	t.Helper()
	now := time.Date(2026, 7, 20, 12, 0, 0, 123, time.UTC)
	instance := &handlerMonitoringInstance{}
	service := New(Options{Instance: instance, Clock: func() time.Time { return now }})
	zero := uint64(0)
	service.store.store(&Snapshot{
		ObservedAt: now,
		VM:         VMIdentity{ID: "0123456789abcdef0123456789abcdef", Name: "vm", Backend: "qemu", Architecture: "aarch64", CPUs: 2, MemoryMiB: 2048, GuestAgent: true, StartedAt: now.Add(-time.Minute), BuildVersion: "test"},
		QMP:        QMPState{State: "running", Version: backend.QEMUVersion{Major: 11, Package: "test"}, Events: backend.QEMUEventCounters{Lifecycle: map[string]uint64{"shutdown": 0}}, Blocks: []backend.QEMUBlockDevice{{Device: "disk", ReadBytes: &zero}}},
		Process:    ProcessState{PID: 42},
		Guest:      GuestState{Observation: backend.GuestObservation{Info: backend.GuestInfo{Version: "9.1", Capabilities: map[string]bool{"guest-ping": true}}, Networks: []backend.GuestNetworkInterface{{Name: "eth0", AddressesPresent: true, Addresses: []backend.GuestIPAddress{{Address: "192.0.2.1", Family: "ipv4", Prefix: 24}}}}}},
		Collectors: map[string]CollectorState{"qmp": {Status: CollectorOK, ObservedAt: now, LastSuccess: now}, "block": {Status: CollectorOK, ObservedAt: now, LastSuccess: now}, "process": {Status: CollectorUnsupported}, "guest_info": {Status: CollectorOK, ObservedAt: now, LastSuccess: now}, "guest_network": {Status: CollectorOK, ObservedAt: now, LastSuccess: now}},
	})
	return service
}

func TestHTTPRoutesGETAndHEADContract(t *testing.T) {
	service := testHTTPService(t)
	for _, path := range []string{"/metrics", "/health", "/status", "/info"} {
		t.Run(path, func(t *testing.T) {
			get := httptest.NewRecorder()
			service.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, path, nil))
			head := httptest.NewRecorder()
			service.Handler().ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))
			if get.Code != head.Code || head.Body.Len() != 0 || get.Header().Get("Content-Type") != head.Header().Get("Content-Type") || get.Header().Get("Content-Length") != head.Header().Get("Content-Length") || get.Header().Get("Cache-Control") != "no-store" || head.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("GET=%d %#v HEAD=%d %#v body=%q", get.Code, get.Header(), head.Code, head.Header(), head.Body.String())
			}
			if get.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
				t.Fatalf("Content-Length=%q body=%d", get.Header().Get("Content-Length"), get.Body.Len())
			}
			if path != "/metrics" && !strings.HasSuffix(get.Body.String(), "\n") {
				t.Fatalf("JSON lacks final newline: %q", get.Body.String())
			}
		})
	}
}

func TestHTTPExactPathsAndMethods(t *testing.T) {
	service := testHTTPService(t)
	unknown := httptest.NewRecorder()
	service.Handler().ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/status/", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown status=%d", unknown.Code)
	}
	method := httptest.NewRecorder()
	service.Handler().ServeHTTP(method, httptest.NewRequest(http.MethodPost, "/status", nil))
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("method response=%d %#v", method.Code, method.Header())
	}
}

func TestStatusAddressesAreCanonicalAndAbsentFromMetrics(t *testing.T) {
	service := testHTTPService(t)
	status := httptest.NewRecorder()
	service.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	var payload map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Body.String(), `"addresses":[{"address":"192.0.2.1","family":"ipv4","prefix":24}]`) {
		t.Fatalf("status address schema=%s", status.Body.String())
	}
	for _, forbidden := range []string{"hardware-address", "hostname", "os-release"} {
		if strings.Contains(status.Body.String(), forbidden) {
			t.Fatalf("status leaked %q", forbidden)
		}
	}
	metrics := httptest.NewRecorder()
	service.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(metrics.Body.String(), "192.0.2.1") {
		t.Fatal("metrics leaked guest IP")
	}
	first := metrics.Body.String()
	second := httptest.NewRecorder()
	service.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if first != second.Body.String() {
		t.Fatal("metrics rendering is nondeterministic")
	}
	for _, definition := range metricDefinitions {
		if !strings.Contains(first, "# HELP "+definition.name+" ") || !strings.Contains(first, "# TYPE "+definition.name+" "+definition.kind) {
			t.Errorf("missing metadata for %s", definition.name)
		}
	}
}

func TestHealthPolicyStateTable(t *testing.T) {
	ok := CollectorState{Status: CollectorOK}
	unsupported := CollectorState{Status: CollectorUnsupported}
	for _, tc := range []struct {
		state      string
		qmp        CollectorState
		wantHealth string
		wantCode   string
		wantStatus int
	}{
		{state: "running", qmp: ok, wantHealth: "healthy", wantStatus: http.StatusOK},
		{state: "paused", qmp: ok, wantHealth: "degraded", wantStatus: http.StatusOK},
		{state: "suspended", qmp: ok, wantHealth: "degraded", wantStatus: http.StatusOK},
		{state: "debug", qmp: ok, wantHealth: "degraded", wantStatus: http.StatusOK},
		{state: "inmigrate", qmp: ok, wantHealth: "unhealthy", wantCode: "state_inmigrate", wantStatus: http.StatusServiceUnavailable},
		{state: "internal-error", qmp: ok, wantHealth: "unhealthy", wantCode: "state_internal-error", wantStatus: http.StatusServiceUnavailable},
		{state: "io-error", qmp: ok, wantHealth: "unhealthy", wantCode: "state_io-error", wantStatus: http.StatusServiceUnavailable},
		{state: "postmigrate", qmp: ok, wantHealth: "unhealthy", wantCode: "state_postmigrate", wantStatus: http.StatusServiceUnavailable},
		{state: "prelaunch", qmp: ok, wantHealth: "unhealthy", wantCode: "state_prelaunch", wantStatus: http.StatusServiceUnavailable},
		{state: "finish-migrate", qmp: ok, wantHealth: "unhealthy", wantCode: "state_finish-migrate", wantStatus: http.StatusServiceUnavailable},
		{state: "restore-vm", qmp: ok, wantHealth: "unhealthy", wantCode: "state_restore-vm", wantStatus: http.StatusServiceUnavailable},
		{state: "save-vm", qmp: ok, wantHealth: "unhealthy", wantCode: "state_save-vm", wantStatus: http.StatusServiceUnavailable},
		{state: "shutdown", qmp: ok, wantHealth: "unhealthy", wantCode: "state_shutdown", wantStatus: http.StatusServiceUnavailable},
		{state: "watchdog", qmp: ok, wantHealth: "unhealthy", wantCode: "state_watchdog", wantStatus: http.StatusServiceUnavailable},
		{state: "guest-panicked", qmp: ok, wantHealth: "unhealthy", wantCode: "state_guest-panicked", wantStatus: http.StatusServiceUnavailable},
		{state: "colo", qmp: ok, wantHealth: "unhealthy", wantCode: "state_colo", wantStatus: http.StatusServiceUnavailable},
		{state: "future", qmp: ok, wantHealth: "unhealthy", wantCode: "unsupported_state", wantStatus: http.StatusServiceUnavailable},
		{state: "running", qmp: CollectorState{Status: CollectorFailed}, wantHealth: "unhealthy", wantCode: "qmp_unavailable", wantStatus: http.StatusServiceUnavailable},
		{state: "running", qmp: CollectorState{Status: CollectorStale}, wantHealth: "unhealthy", wantCode: "qmp_stale", wantStatus: http.StatusServiceUnavailable},
	} {
		health, code, status := healthPolicy(tc.state, tc.qmp, unsupported)
		if health != tc.wantHealth || code != tc.wantCode || status != tc.wantStatus {
			t.Errorf("state=%q qmp=%q: got (%q,%q,%d), want (%q,%q,%d)", tc.state, tc.qmp.Status, health, code, status, tc.wantHealth, tc.wantCode, tc.wantStatus)
		}
	}
}

func TestHealthProcessAvailabilityPolicy(t *testing.T) {
	health, code, status := healthPolicy("running", CollectorState{Status: CollectorOK}, CollectorState{Status: CollectorFailed})
	if processStatsSupported {
		if health != "degraded" || code != "process_stats_unavailable" || status != http.StatusOK {
			t.Fatalf("Darwin process failure policy = %q, %q, %d", health, code, status)
		}
	} else if health != "healthy" || code != "" || status != http.StatusOK {
		t.Fatalf("unsupported-platform process failure policy = %q, %q, %d", health, code, status)
	}
}

func TestPingConfigurationFailures(t *testing.T) {
	service := testHTTPService(t)
	snapshot := service.Snapshot()
	snapshot.VM.GuestAgent = false
	status, payload := service.renderPing(context.Background(), snapshot)
	if status != http.StatusConflict || payload["code"] != "guest_agent_not_configured" {
		t.Fatalf("unconfigured ping = %d %#v", status, payload)
	}
	snapshot.VM.GuestAgent = true
	snapshot.Guest.Observation.Info.Capabilities["guest-ping"] = false
	status, payload = service.renderPing(context.Background(), snapshot)
	if status != http.StatusConflict || payload["code"] != "guest_agent_command_disabled" {
		t.Fatalf("disabled ping = %d %#v", status, payload)
	}
}

func TestPingCoalescesOnlyInflightCalls(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	instance := &handlerMonitoringInstance{pingStarted: make(chan struct{}, 2), pingRelease: make(chan struct{})}
	service := New(Options{Instance: instance, Clock: func() time.Time { return now }})
	service.store.store(&Snapshot{ObservedAt: now, VM: VMIdentity{GuestAgent: true}, Guest: GuestState{Observation: backend.GuestObservation{Info: backend.GuestInfo{Capabilities: map[string]bool{"guest-ping": true}}}}, Collectors: map[string]CollectorState{"qmp": {Status: CollectorOK}, "process": {Status: CollectorUnsupported}}})

	first := service.joinPing(context.Background())
	<-instance.pingStarted
	overlapping := service.joinPing(context.Background())
	if first != overlapping {
		t.Fatal("overlapping ping did not share the in-flight call")
	}
	close(instance.pingRelease)
	<-first.done
	instance.mu.Lock()
	calls := instance.pingCalls
	instance.pingRelease = nil
	instance.mu.Unlock()
	if calls != 1 {
		t.Fatalf("overlapping ping calls=%d", calls)
	}

	later := service.joinPing(context.Background())
	<-later.done
	instance.mu.Lock()
	calls = instance.pingCalls
	instance.mu.Unlock()
	if calls != 2 {
		t.Fatalf("later ping reused result; calls=%d", calls)
	}
}
