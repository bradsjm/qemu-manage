package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
)

const (
	infoTestName = "test-vm"
	infoTestID   = "0123456789abcdef0123456789abcdef"
)

var infoTestStartedAt = time.Date(2026, 7, 21, 12, 34, 56, 789123456, time.UTC)

func infoTestApp(t *testing.T, state model.RunState, metrics bool) *App {
	t.Helper()
	a := testApp(t)
	config := testConfig(infoTestName)
	config.ID = infoTestID
	if metrics {
		config.Metrics = &model.MetricsConfig{Port: 12345}
	}
	saveTestConfig(t, a, config)
	pid := 4321
	startedAt := infoTestStartedAt
	a.Runtime = &fakeRuntime{row: StatusRow{
		State:     state,
		PID:       &pid,
		StartedAt: &startedAt,
	}}
	return a
}

func infoTestAppWithConfig(t *testing.T, config *model.Config, row StatusRow) *App {
	t.Helper()
	a := testApp(t)
	saveTestConfig(t, a, config)
	a.Runtime = &fakeRuntime{row: row}
	return a
}

func runInfoContext(a *App, ctx context.Context, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	err := a.runInfo(ctx, args, &stdout)
	if err != nil {
		return 1, stdout.String(), err.Error()
	}
	return 0, stdout.String(), stderr.String()
}

func decodeInfoJSON(t *testing.T, output string) map[string]json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("decode info JSON: %v; output=%q", err, output)
	}
	return value
}

func requireJSONKeys(t *testing.T, value map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(value) != len(want) {
		t.Fatalf("JSON keys=%v, want exactly %v", sortedJSONKeys(value), want)
	}
	for _, key := range want {
		if _, ok := value[key]; !ok {
			t.Errorf("JSON omitted key %q: %v", key, sortedJSONKeys(value))
		}
	}
}

func sortedJSONKeys(value map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	// Stable enough for failure diagnostics without importing another helper.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func TestInfoInactiveStatesNeverQueryHTTP(t *testing.T) {
	tests := []struct {
		name       string
		state      model.RunState
		wantHuman  string
		wantJSON   []string
		wantRunRaw string
	}{
		{
			name:       "stopped",
			state:      model.RunStateStopped,
			wantHuman:  `VM "test-vm" is not running.` + "\n",
			wantJSON:   []string{"name", "state", "running"},
			wantRunRaw: "false",
		},
		{
			name:      "failed",
			state:     model.RunStateFailed,
			wantHuman: `VM "test-vm" state is failed; live information is not available.` + "\n",
			wantJSON:  []string{"name", "state"},
		},
		{
			name:      "starting",
			state:     model.RunStateStarting,
			wantHuman: `VM "test-vm" is starting; live information is not available.` + "\n",
			wantJSON:  []string{"name", "state"},
		},
		{
			name:      "stopping",
			state:     model.RunStateStopping,
			wantHuman: `VM "test-vm" is stopping; live information is not available.` + "\n",
			wantJSON:  []string{"name", "state"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := infoTestApp(t, tc.state, true)
			calls := 0
			a.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("HTTP must not be queried")
			})}

			code, stdout, stderr := runCLI(a, "info", infoTestName)
			if code != 0 || stderr != "" || stdout != tc.wantHuman {
				t.Fatalf("human code=%d stderr=%q stdout=%q, want code 0, no stderr, %q", code, stderr, stdout, tc.wantHuman)
			}
			code, stdout, stderr = runCLI(a, "info", infoTestName, "--json")
			if code != 0 || stderr != "" {
				t.Fatalf("JSON code=%d stderr=%q stdout=%q", code, stderr, stdout)
			}
			value := decodeInfoJSON(t, stdout)
			requireJSONKeys(t, value, tc.wantJSON...)
			if tc.wantRunRaw != "" && string(value["running"]) != tc.wantRunRaw {
				t.Fatalf("running=%s, want %s", value["running"], tc.wantRunRaw)
			}
			if strings.Contains(stdout, "offline") || strings.Contains(stdout, "monitoring") {
				t.Fatalf("inactive output made an unsupported live claim: %q", stdout)
			}
			if calls != 0 {
				t.Fatalf("HTTP calls=%d, want zero", calls)
			}
		})
	}
}

func TestInfoRunningFallbacksAndDriftHints(t *testing.T) {
	tests := []struct {
		name           string
		metrics        bool
		drift          bool
		wantHuman      string
		wantReason     string
		wantPort       bool
		wantRestart    bool
		wantMonitoring string
	}{
		{
			name:        "metrics disabled",
			wantHuman:   "disabled. Enable it with: qemu-manage set test-vm --metrics-port PORT",
			wantReason:  "disabled",
			wantRestart: false,
		},
		{
			name:        "disabled with config drift",
			drift:       true,
			wantHuman:   "disabled. Enable it with: qemu-manage set test-vm --metrics-port PORT Restart the VM to apply current configuration changes.",
			wantReason:  "disabled",
			wantRestart: true,
		},
		{
			name:        "unavailable",
			metrics:     true,
			wantHuman:   "unavailable: GET /info: dial failed",
			wantReason:  "unavailable",
			wantPort:    true,
			wantRestart: false,
		},
		{
			name:        "unavailable with config drift",
			metrics:     true,
			drift:       true,
			wantHuman:   "unavailable: GET /info: dial failed Restart the VM to apply current configuration changes.",
			wantReason:  "unavailable",
			wantPort:    true,
			wantRestart: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := infoTestApp(t, model.RunStateRunning, tc.metrics)
			if tc.drift {
				a.Runtime.(*fakeRuntime).row.RunningConfigSHA256 = "not-the-current-hash"
			}
			if tc.metrics {
				a.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("dial failed")
				})}
			}
			code, stdout, stderr := runCLI(a, "info", infoTestName)
			if code != 0 || stderr != "" || !strings.Contains(stdout, tc.wantHuman) {
				t.Fatalf("human code=%d stderr=%q stdout=%q, want %q", code, stderr, stdout, tc.wantHuman)
			}
			code, stdout, stderr = runCLI(a, "info", infoTestName, "--json")
			if code != 0 || stderr != "" {
				t.Fatalf("JSON code=%d stderr=%q stdout=%q", code, stderr, stdout)
			}
			value := decodeInfoJSON(t, stdout)
			var monitoring map[string]json.RawMessage
			if err := json.Unmarshal(value["monitoring"], &monitoring); err != nil {
				t.Fatal(err)
			}
			if string(monitoring["available"]) != "false" || string(monitoring["reason"]) != strconv.Quote(tc.wantReason) {
				t.Fatalf("monitoring=%s, want unavailable reason %q", value["monitoring"], tc.wantReason)
			}
			if tc.wantPort != (monitoring["port"] != nil) {
				t.Fatalf("monitoring port presence=%v, want %v: %s", monitoring["port"] != nil, tc.wantPort, value["monitoring"])
			}
			if tc.wantRestart != (string(value["restart_required"]) == "true") {
				t.Fatalf("restart_required=%s, want %v", value["restart_required"], tc.wantRestart)
			}
			if tc.wantReason == "disabled" && monitoring["error"] != nil {
				t.Fatalf("disabled monitoring unexpectedly has error: %s", value["monitoring"])
			}
		})
	}
}

func TestInfoRunningAndPausedSuccessRetainRawEndpointObjects(t *testing.T) {
	for _, state := range []model.RunState{model.RunStateRunning, model.RunStatePaused} {
		t.Run(string(state), func(t *testing.T) {
			a := infoTestApp(t, state, true)
			infoBody, statusBody := infoFixture(t, infoTestName, infoTestID, infoTestStartedAt, 4321)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				if r.URL.Path == "/info" {
					_, _ = io.WriteString(w, infoBody)
					return
				}
				if r.URL.Path == "/status" {
					_, _ = io.WriteString(w, statusBody)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			_, portText, err := netSplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.ParseUint(portText, 10, 16)
			if err != nil {
				t.Fatal(err)
			}
			config, err := a.Store.Load(infoTestName)
			if err != nil {
				t.Fatal(err)
			}
			config.Metrics.Port = uint16(port)
			if err := a.Store.Save(config); err != nil {
				t.Fatal(err)
			}
			a.HTTPClient = server.Client()

			code, stdout, stderr := runCLI(a, "info", infoTestName, "--json")
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
			}
			value := decodeInfoJSON(t, stdout)
			requireJSONKeys(t, value, "name", "state", "running", "restart_required", "cpus", "memory_mib", "network", "autostart", "pid", "started_at", "monitoring")
			if string(value["running"]) != "true" || string(value["state"]) != strconv.Quote(string(state)) {
				t.Fatalf("active wrapper=%s", stdout)
			}
			var monitoring map[string]json.RawMessage
			if err := json.Unmarshal(value["monitoring"], &monitoring); err != nil {
				t.Fatal(err)
			}
			if string(monitoring["available"]) != "true" || string(monitoring["port"]) != strconv.FormatUint(port, 10) {
				t.Fatalf("monitoring=%s", value["monitoring"])
			}
			if string(monitoring["info"]) != infoBody || string(monitoring["status"]) != statusBody {
				t.Fatalf("endpoint raw objects changed: info=%s status=%s", monitoring["info"], monitoring["status"])
			}
			for _, key := range []string{"reason", "error"} {
				if _, ok := monitoring[key]; ok {
					t.Errorf("successful monitoring emitted %q", key)
				}
			}
		})
	}
}

func TestInfoHumanFullSummaryAndTables(t *testing.T) {
	a := infoTestApp(t, model.RunStateRunning, true)
	infoBody, statusBody := infoFixture(t, infoTestName, infoTestID, infoTestStartedAt, 4321)
	a.HTTPClient = clientForInfoBodies(infoBody, statusBody)
	code, stdout, stderr := runCLI(a, "info", infoTestName)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	for _, want := range []string{
		"FIELD", "VALUE", "name", "test-vm", "state", "running", "started at", "2026-07-21T12:34:56.789123456Z",
		"pid", "4321", "resources", "2 CPUs, 2GiB memory", "network", "user", "autostart", "disabled", "vnc", "-",
		"restart required", "false", "monitoring", "available on 127.0.0.1:12345", "uptime", "1m2s",
		"health", "healthy (ready)", "qemu", "8.2.1 [distro] (hvf)",
		"process", "1.5MiB resident, 3.5s CPU, 7 threads", "guest agent", "up, version 9.0, capabilities cpu, network",
		"guest load", "0.5 / 1.25 / 2", "collectors", "disk=stale (age), qmp=failed",
		"INTERFACE", "ADDRESS", "eth0", "192.0.2.3/24", "MOUNTPOINT", "TYPE", "USED", "SIZE", "/", "ext4", "1MiB", "4MiB",
		"BLOCK DEVICE", "STATUS", "READ", "WRITTEN", "disk0", "active", "2KiB", "3KiB",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Index(stdout, "disk=stale (age)") > strings.Index(stdout, "qmp=failed") {
		t.Fatal("collector rows are not sorted")
	}
	if strings.Index(stdout, "capabilities cpu, network") < 0 {
		t.Fatal("enabled capabilities are not sorted")
	}
	_ = statusBody
}

func TestInfoHumanPartialPlaceholdersAndAddressPresence(t *testing.T) {
	a := infoTestApp(t, model.RunStateRunning, true)
	infoBody := `{"api_version":1,"vm":{"id":"0123456789abcdef0123456789abcdef","name":"test-vm","cpus":2,"memory_mib":2048},"qemu":{"version":{"major":8,"minor":2,"micro":1,"package":""},"accelerator":"hvf"},"guest_agent":{"configured":true,"version":"","capabilities":{"zulu":false,"alpha":false}}}`
	statusBody := `{"api_version":1,"observed_at":"2026-07-21T12:35:00Z","health":{"status":"degraded"},"vm":{"id":"0123456789abcdef0123456789abcdef","name":"test-vm","state":"running","started_at":"2026-07-21T12:34:56.789123456Z","uptime_seconds":0.49},"process":{"stats_up":true,"pid":4321},"block_devices":[{"device":"disk0"}],"guest_agent":{"configured":true,"up":true,"version":""},"collectors":{"zulu":{"status":"stale"},"alpha":{"status":"failed"}},"guest":{"load":{"load1":0.5},"filesystems":[{"mountpoint":"/data","fstype":"xfs"}],"network_interfaces":[{"name":"absent"},{"name":"empty","addresses":[]},{"name":"addr","addresses":[{"address":"2001:db8::1","family":"ipv6","prefix":64}]}]}}`
	a.HTTPClient = clientForInfoBodies(infoBody, statusBody)
	code, stdout, stderr := runCLI(a, "info", infoTestName)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	for _, want := range []string{
		"qemu", "8.2.1 (hvf)", "process", "- resident, - CPU, - threads", "guest agent", "up",
		"guest load", "0.5 / - / -", "collectors", "alpha=failed, zulu=stale", "INTERFACE", "empty", "-", "addr", "2001:db8::1/64",
		"MOUNTPOINT", "/data", "xfs", "-", "BLOCK DEVICE", "disk0", "-",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("partial output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "absent") {
		t.Fatalf("interface with absent addresses rendered a row:\n%s", stdout)
	}
}

func TestInfoMonitoringSharedDeadlineAndSequentialEndpoints(t *testing.T) {
	a := infoTestApp(t, model.RunStateRunning, true)
	infoBody, statusBody := infoFixture(t, infoTestName, infoTestID, infoTestStartedAt, 4321)
	rt := &recordingRoundTripper{responses: map[string]roundTripResponse{
		"/info":   {body: io.NopCloser(strings.NewReader(infoBody)), contentType: "application/json"},
		"/status": {body: io.NopCloser(strings.NewReader(statusBody)), contentType: "application/json"},
	}}
	a.HTTPClient = &http.Client{Transport: rt}
	code, stdout, stderr := runCLI(a, "info", infoTestName, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if len(rt.calls) != 2 || rt.calls[0].path != "/info" || rt.calls[1].path != "/status" {
		t.Fatalf("calls=%v, want sequential /info then /status", rt.calls)
	}
	if !rt.calls[0].hasDeadline || !rt.calls[1].hasDeadline || !rt.calls[0].deadline.Equal(rt.calls[1].deadline) {
		t.Fatalf("request deadlines=%v, want one shared deadline", rt.calls)
	}
	_ = stdout
}

func TestInfoMonitoringCanceledContext(t *testing.T) {
	a := infoTestApp(t, model.RunStateRunning, true)
	a.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, stdout, stderr := runInfoContext(a, ctx, infoTestName)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "unavailable: GET /info: context canceled") {
		t.Fatalf("code=%d stderr=%q stdout=%q, want canceled fallback: %q", code, stderr, stdout, "unavailable: GET /info: context canceled")
	}
}

type infoHTTPFixture struct {
	status      int
	contentType string
	body        io.ReadCloser
	err         error
	header      http.Header
}

type roundTripResponse struct {
	status      int
	contentType string
	body        io.ReadCloser
	err         error
	header      http.Header
}

type roundTripCall struct {
	path        string
	hasDeadline bool
	deadline    time.Time
}

type recordingRoundTripper struct {
	mu        sync.Mutex
	responses map[string]roundTripResponse
	calls     []roundTripCall
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	deadline, hasDeadline := req.Context().Deadline()
	r.calls = append(r.calls, roundTripCall{path: req.URL.Path, hasDeadline: hasDeadline, deadline: deadline})
	response := r.responses[req.URL.Path]
	r.mu.Unlock()
	if response.err != nil {
		return nil, response.err
	}
	body := response.body
	if body == nil {
		body = io.NopCloser(strings.NewReader("{}"))
	}
	header := response.header
	if header == nil {
		header = make(http.Header)
	}
	if response.contentType != "" {
		header.Set("Content-Type", response.contentType)
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: header, Body: body, Request: req}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func clientForInfoBodies(infoBody, statusBody string) *http.Client {
	return &http.Client{Transport: &recordingRoundTripper{responses: map[string]roundTripResponse{
		"/info":   {body: io.NopCloser(strings.NewReader(infoBody)), contentType: "application/json"},
		"/status": {body: io.NopCloser(strings.NewReader(statusBody)), contentType: "application/json"},
	}}}
}

func TestInfoMonitoringHTTPBoundaryFailuresFallback(t *testing.T) {
	validInfo, validStatus := infoFixture(t, infoTestName, infoTestID, infoTestStartedAt, 4321)
	tests := []struct {
		name     string
		response roundTripResponse
		want     string
	}{
		{name: "transport failure unwraps URL error", response: roundTripResponse{err: errors.New("connection reset")}, want: "GET /info: connection reset"},
		{name: "non-200", response: roundTripResponse{status: http.StatusServiceUnavailable, body: io.NopCloser(strings.NewReader("{}")), contentType: "application/json"}, want: "GET /info: HTTP 503 Service Unavailable"},
		{name: "redirect", response: roundTripResponse{status: http.StatusFound, body: io.NopCloser(strings.NewReader("redirect")), contentType: "text/plain", header: http.Header{"Location": []string{"/other"}}}, want: "GET /info: redirects are not permitted"},
		{name: "wrong content type", response: roundTripResponse{body: io.NopCloser(strings.NewReader(validInfo)), contentType: "text/plain"}, want: `/info returned content type "text/plain", want application/json`},
		{name: "malformed content type", response: roundTripResponse{body: io.NopCloser(strings.NewReader(validInfo)), contentType: "application/json; charset=\""}, want: `/info returned content type "application/json; charset=\"", want application/json`},
		{name: "malformed JSON", response: roundTripResponse{body: io.NopCloser(strings.NewReader("{\"api_version\":")), contentType: "application/json"}, want: "/info returned invalid JSON"},
		{name: "trailing JSON", response: roundTripResponse{body: io.NopCloser(strings.NewReader(validInfo + " {}")), contentType: "application/json"}, want: "/info returned invalid JSON"},
		{name: "wrong field type", response: roundTripResponse{body: io.NopCloser(strings.NewReader(strings.Replace(validInfo, `"api_version":1`, `"api_version":"1"`, 1))), contentType: "application/json"}, want: "/info returned invalid JSON"},
		{name: "read failure", response: roundTripResponse{body: &infoErrorBody{data: []byte(validInfo), err: errors.New("short read")}, contentType: "application/json"}, want: "read /info response: short read"},
		{name: "close failure", response: roundTripResponse{body: &infoErrorBody{data: []byte(validInfo), closeErr: errors.New("close failed")}, contentType: "application/json"}, want: "close /info response: close failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := infoTestApp(t, model.RunStateRunning, true)
			rt := &recordingRoundTripper{responses: map[string]roundTripResponse{"/info": tc.response, "/status": {body: io.NopCloser(strings.NewReader(validStatus)), contentType: "application/json"}}}
			a.HTTPClient = &http.Client{Transport: rt}
			code, stdout, stderr := runCLI(a, "info", infoTestName)
			if code != 0 || stderr != "" || !strings.Contains(stdout, "unavailable: "+tc.want) {
				t.Fatalf("code=%d stderr=%q stdout=%q, want error %q", code, stderr, stdout, tc.want)
			}
		})
	}
}

func TestInfoMonitoringResponseLimit(t *testing.T) {
	a := infoTestApp(t, model.RunStateRunning, true)
	a.HTTPClient = &http.Client{Transport: &recordingRoundTripper{responses: map[string]roundTripResponse{
		"/info": {body: &repeatReader{remaining: maxMonitoringResponseBytes + 1}, contentType: "application/json"},
	}}}
	code, stdout, stderr := runCLI(a, "info", infoTestName)
	if code != 0 || stderr != "" || !strings.Contains(stdout, fmt.Sprintf("unavailable: /info response exceeds %d bytes", maxMonitoringResponseBytes)) {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
}

type infoErrorBody struct {
	data     []byte
	offset   int
	err      error
	closeErr error
	closed   bool
}

func (b *infoErrorBody) Read(p []byte) (int, error) {
	if b.offset < len(b.data) {
		n := copy(p, b.data[b.offset:])
		b.offset += n
		return n, nil
	}
	if b.err != nil {
		err := b.err
		b.err = nil
		return 0, err
	}
	return 0, io.EOF
}

func (b *infoErrorBody) Close() error {
	b.closed = true
	return b.closeErr
}

type repeatReader struct{ remaining int }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}
func (r *repeatReader) Close() error { return nil }

func TestInfoMonitoringRequiredFieldsAndValidation(t *testing.T) {
	validInfo, validStatus := infoFixture(t, infoTestName, infoTestID, infoTestStartedAt, 4321)
	tests := []struct {
		name   string
		path   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "info api version", path: "/info", mutate: deleteJSONPath("api_version"), want: "/info is missing required field api_version"},
		{name: "info vm object", path: "/info", mutate: deleteJSONPath("vm"), want: "/info is missing required field vm"},
		{name: "info vm id", path: "/info", mutate: deleteJSONPath("vm.id"), want: "/info is missing required field vm.id"},
		{name: "info vm name", path: "/info", mutate: deleteJSONPath("vm.name"), want: "/info is missing required field vm.name"},
		{name: "info vm cpus", path: "/info", mutate: deleteJSONPath("vm.cpus"), want: "/info is missing required field vm.cpus"},
		{name: "info vm memory", path: "/info", mutate: deleteJSONPath("vm.memory_mib"), want: "/info is missing required field vm.memory_mib"},
		{name: "info qemu object", path: "/info", mutate: deleteJSONPath("qemu"), want: "/info is missing required field qemu"},
		{name: "info qemu version", path: "/info", mutate: deleteJSONPath("qemu.version"), want: "/info is missing required field qemu.version"},
		{name: "info qemu major", path: "/info", mutate: deleteJSONPath("qemu.version.major"), want: "/info is missing required field qemu.version.major"},
		{name: "info qemu minor", path: "/info", mutate: deleteJSONPath("qemu.version.minor"), want: "/info is missing required field qemu.version.minor"},
		{name: "info qemu micro", path: "/info", mutate: deleteJSONPath("qemu.version.micro"), want: "/info is missing required field qemu.version.micro"},
		{name: "info qemu package", path: "/info", mutate: deleteJSONPath("qemu.version.package"), want: "/info is missing required field qemu.version.package"},
		{name: "info accelerator", path: "/info", mutate: deleteJSONPath("qemu.accelerator"), want: "/info is missing required field qemu.accelerator"},
		{name: "info guest agent", path: "/info", mutate: deleteJSONPath("guest_agent"), want: "/info is missing required field guest_agent"},
		{name: "info agent configured", path: "/info", mutate: deleteJSONPath("guest_agent.configured"), want: "/info is missing required field guest_agent.configured"},
		{name: "info agent version", path: "/info", mutate: deleteJSONPath("guest_agent.version"), want: "/info is missing required field guest_agent.version"},
		{name: "info agent capabilities", path: "/info", mutate: deleteJSONPath("guest_agent.capabilities"), want: "/info is missing required field guest_agent.capabilities"},
		{name: "status api version", path: "/status", mutate: deleteJSONPath("api_version"), want: "/status is missing required field api_version"},
		{name: "status observed at", path: "/status", mutate: deleteJSONPath("observed_at"), want: "/status is missing required field observed_at"},
		{name: "status health", path: "/status", mutate: deleteJSONPath("health"), want: "/status is missing required field health"},
		{name: "status health status", path: "/status", mutate: deleteJSONPath("health.status"), want: "/status is missing required field health.status"},
		{name: "status vm", path: "/status", mutate: deleteJSONPath("vm"), want: "/status is missing required field vm"},
		{name: "status vm id", path: "/status", mutate: deleteJSONPath("vm.id"), want: "/status is missing required field vm.id"},
		{name: "status vm name", path: "/status", mutate: deleteJSONPath("vm.name"), want: "/status is missing required field vm.name"},
		{name: "status vm state", path: "/status", mutate: deleteJSONPath("vm.state"), want: "/status is missing required field vm.state"},
		{name: "status started at", path: "/status", mutate: deleteJSONPath("vm.started_at"), want: "/status is missing required field vm.started_at"},
		{name: "status uptime", path: "/status", mutate: deleteJSONPath("vm.uptime_seconds"), want: "/status is missing required field vm.uptime_seconds"},
		{name: "status process", path: "/status", mutate: deleteJSONPath("process"), want: "/status is missing required field process"},
		{name: "status stats up", path: "/status", mutate: deleteJSONPath("process.stats_up"), want: "/status is missing required field process.stats_up"},
		{name: "status pid", path: "/status", mutate: deleteJSONPath("process.pid"), want: "/status is missing required field process.pid"},
		{name: "status block devices", path: "/status", mutate: deleteJSONPath("block_devices"), want: "/status is missing required field block_devices"},
		{name: "status guest agent", path: "/status", mutate: deleteJSONPath("guest_agent"), want: "/status is missing required field guest_agent"},
		{name: "status agent configured", path: "/status", mutate: deleteJSONPath("guest_agent.configured"), want: "/status is missing required field guest_agent.configured"},
		{name: "status agent up", path: "/status", mutate: deleteJSONPath("guest_agent.up"), want: "/status is missing required field guest_agent.up"},
		{name: "status collectors", path: "/status", mutate: deleteJSONPath("collectors"), want: "/status is missing required field collectors"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			infoBody, statusBody := validInfo, validStatus
			if tc.path == "/info" {
				infoBody = mutateJSON(t, validInfo, tc.mutate)
			} else {
				statusBody = mutateJSON(t, validStatus, tc.mutate)
			}
			a := infoTestApp(t, model.RunStateRunning, true)
			a.HTTPClient = clientForInfoBodies(infoBody, statusBody)
			code, stdout, stderr := runCLI(a, "info", infoTestName)
			if code != 0 || stderr != "" || !strings.Contains(stdout, "unavailable: "+tc.want) {
				t.Fatalf("code=%d stderr=%q stdout=%q, want %q", code, stderr, stdout, tc.want)
			}
		})
	}
}

func TestInfoMonitoringValueConstraintsAndIdentityBinding(t *testing.T) {
	validInfo, validStatus := infoFixture(t, infoTestName, infoTestID, infoTestStartedAt, 4321)
	tests := []struct {
		name   string
		path   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "negative uptime", path: "/status", mutate: setJSONPath("vm.uptime_seconds", -1), want: "/status has invalid required field vm.uptime_seconds"},
		{name: "unsupported info version", path: "/info", mutate: setJSONPath("api_version", 2), want: "/info uses unsupported API version 2"},
		{name: "unsupported status version", path: "/status", mutate: setJSONPath("api_version", 2), want: "/status uses unsupported API version 2"},
		{name: "info VM ID mismatch", path: "/info", mutate: setJSONPath("vm.id", "other-id"), want: "/info VM identity does not match config"},
		{name: "info VM name mismatch", path: "/info", mutate: setJSONPath("vm.name", "other-name"), want: "/info VM identity does not match config"},
		{name: "status VM ID mismatch", path: "/status", mutate: setJSONPath("vm.id", "other-id"), want: "/status VM identity does not match config"},
		{name: "status VM name mismatch", path: "/status", mutate: setJSONPath("vm.name", "other-name"), want: "/status VM identity does not match config"},
		{name: "authenticated PID mismatch", path: "/status", mutate: setJSONPath("process.pid", 9876), want: "/status does not match the authenticated VM run"},
		{name: "authenticated start mismatch", path: "/status", mutate: setJSONPath("vm.started_at", "2026-07-21T12:34:57Z"), want: "/status does not match the authenticated VM run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			infoBody, statusBody := validInfo, validStatus
			if tc.path == "/info" {
				infoBody = mutateJSON(t, validInfo, tc.mutate)
			} else {
				statusBody = mutateJSON(t, validStatus, tc.mutate)
			}
			a := infoTestApp(t, model.RunStateRunning, true)
			a.HTTPClient = clientForInfoBodies(infoBody, statusBody)
			code, stdout, stderr := runCLI(a, "info", infoTestName, "--json")
			if code != 0 || stderr != "" || !strings.Contains(stdout, `"reason":"unavailable"`) || !strings.Contains(stdout, tc.want) {
				t.Fatalf("code=%d stderr=%q stdout=%q, want fallback containing %q", code, stderr, stdout, tc.want)
			}
		})
	}
}

func TestInfoHumanWriterErrorPropagates(t *testing.T) {
	wantErr := errors.New("writer failed")
	a := infoTestApp(t, model.RunStateRunning, false)
	if err := a.runInfo(context.Background(), []string{infoTestName}, errorWriter{err: wantErr}); !errors.Is(err, wantErr) {
		t.Fatalf("human writer error=%v, want %v", err, wantErr)
	}
	if err := a.runInfo(context.Background(), []string{infoTestName, "--json"}, errorWriter{err: wantErr}); !errors.Is(err, wantErr) {
		t.Fatalf("JSON writer error=%v, want %v", err, wantErr)
	}
}

func infoFixture(t *testing.T, name, id string, startedAt time.Time, pid int) (string, string) {
	t.Helper()
	info := map[string]any{
		"api_version": 1,
		"vm":          map[string]any{"id": id, "name": name, "cpus": 2, "memory_mib": 2048},
		"qemu":        map[string]any{"version": map[string]any{"major": 8, "minor": 2, "micro": 1, "package": "distro"}, "accelerator": "hvf"},
		"guest_agent": map[string]any{"configured": true, "version": "9.0", "capabilities": map[string]bool{"network": true, "cpu": true, "disk": false}},
	}
	status := map[string]any{
		"api_version":   1,
		"observed_at":   "2026-07-21T12:35:00Z",
		"health":        map[string]any{"status": "healthy", "code": "ready"},
		"vm":            map[string]any{"id": id, "name": name, "state": "running", "started_at": startedAt.UTC().Format(time.RFC3339Nano), "uptime_seconds": 62.4},
		"process":       map[string]any{"stats_up": true, "pid": pid, "cpu_seconds": map[string]any{"user": 1.25, "system": 2.25}, "resident_memory_bytes": 1572864, "threads": 7},
		"block_devices": []any{map[string]any{"device": "disk0", "io_status": "active", "read_bytes": 2048, "write_bytes": 3072}},
		"guest_agent":   map[string]any{"configured": true, "up": true, "version": "9.0"},
		"collectors":    map[string]any{"qmp": map[string]any{"status": "failed"}, "disk": map[string]any{"status": "stale", "code": "age"}, "process": map[string]any{"status": "ok"}},
		"guest": map[string]any{
			"load":               map[string]any{"load1": 0.5, "load5": 1.25, "load15": 2.0},
			"filesystems":        []any{map[string]any{"mountpoint": "/", "fstype": "ext4", "used_bytes": 1048576, "size_bytes": 4194304}},
			"network_interfaces": []any{map[string]any{"name": "eth0", "addresses": []any{map[string]any{"address": "192.0.2.3", "family": "ipv4", "prefix": 24}}}},
			"disks":              []any{},
		},
	}
	infoRaw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	statusRaw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	return string(infoRaw), string(statusRaw)
}

func mutateJSON(t *testing.T, raw string, mutate func(map[string]any)) string {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	mutate(value)
	result, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(result)
}

func deleteJSONPath(path string) func(map[string]any) {
	return func(value map[string]any) {
		parts := strings.Split(path, ".")
		current := value
		for _, part := range parts[:len(parts)-1] {
			next, ok := current[part].(map[string]any)
			if !ok {
				return
			}
			current = next
		}
		delete(current, parts[len(parts)-1])
	}
}

func setJSONPath(path string, replacement any) func(map[string]any) {
	return func(value map[string]any) {
		parts := strings.Split(path, ".")
		current := value
		for _, part := range parts[:len(parts)-1] {
			next, ok := current[part].(map[string]any)
			if !ok {
				return
			}
			current = next
		}
		current[parts[len(parts)-1]] = replacement
	}
}

// netSplitHostPort keeps this test file independent of platform-specific listener
// address formatting while retaining a useful error from net.SplitHostPort.
func netSplitHostPort(address string) (string, string, error) {
	parsed, err := neturlParse(address)
	if err != nil {
		return "", "", err
	}
	return parsed.host, parsed.port, nil
}

type parsedHostPort struct{ host, port string }

func neturlParse(address string) (parsedHostPort, error) {
	if strings.HasPrefix(address, "[") {
		closeBracket := strings.LastIndex(address, "]")
		if closeBracket < 0 || closeBracket+2 > len(address) || address[closeBracket+1] != ':' {
			return parsedHostPort{}, fmt.Errorf("invalid listener address %q", address)
		}
		return parsedHostPort{host: address[1:closeBracket], port: address[closeBracket+2:]}, nil
	}
	separator := strings.LastIndex(address, ":")
	if separator < 0 {
		return parsedHostPort{}, fmt.Errorf("invalid listener address %q", address)
	}
	return parsedHostPort{host: address[:separator], port: address[separator+1:]}, nil
}

var _ = backend.VNCEndpoint{}
