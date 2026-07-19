package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/launchd"
	"github.com/bradsjm/qemu-manage/internal/lifecycle"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/qemu"
	"github.com/bradsjm/qemu-manage/internal/store"
	"github.com/bradsjm/qemu-manage/internal/supervisor"
)

type fakeRuntime struct {
	row           StatusRow
	err           error
	deleteAllowed bool
}

func (f *fakeRuntime) Status(context.Context, *model.Config) (StatusRow, error) { return f.row, f.err }
func (f *fakeRuntime) DeleteAllowed(context.Context, *model.Config) (bool, error) {
	return f.deleteAllowed, f.err
}

type fakeMonitorClient struct {
	output   string
	err      error
	closeErr error
	command  string
	closed   bool
}

func (f *fakeMonitorClient) HumanMonitorCommand(_ context.Context, command string) (string, error) {
	f.command = command
	return f.output, f.err
}

func (f *fakeMonitorClient) Close() error {
	f.closed = true
	return f.closeErr
}

type signalBuffer struct {
	bytes.Buffer
	written chan struct{}
	once    sync.Once
}

func (b *signalBuffer) Write(p []byte) (int, error) {
	n, err := b.Buffer.Write(p)
	if n > 0 {
		b.once.Do(func() {
			close(b.written)
		})
	}
	return n, err
}

type absentLaunchdRunner struct{}

func (absentLaunchdRunner) Run(_ context.Context, _ bool, _ string, args ...string) ([]byte, error) {
	target := args[len(args)-1]
	label := target[strings.LastIndex(target, "/")+1:]
	return []byte(`Could not find service "` + label + `" in domain for test`), errors.New("service not found")
}

func configureAbsentLaunchd(t *testing.T, a *App) {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "qemu-manage")
	if err := os.WriteFile(executable, []byte("test executable"), 0700); err != nil {
		t.Fatal(err)
	}
	manager := launchd.NewManager(a.Store, executable, "alice", root, os.Getuid())
	manager.Runner = absentLaunchdRunner{}
	manager.LoginDir = filepath.Join(root, "LaunchAgents")
	manager.SystemDir = filepath.Join(root, "LaunchDaemons")
	a.Launchd = manager
}

func serveUnixSocket(t *testing.T, socketPath string, serve func(net.Conn) error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen %s: %v", socketPath, err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			err = serve(conn)
			if conn != nil {
				_ = conn.Close()
			}
		}
		_ = listener.Close()
		done <- err
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-done; err != nil {
			t.Errorf("socket server %s: %v", socketPath, err)
		}
	})
}

func serveRuntimeStatus(t *testing.T, socketPath string, status supervisor.Status) <-chan error {
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

func TestStatusAndListJSONContracts(t *testing.T) {
	a := testApp(t)
	saveTestConfig(t, a, testConfig("zeta"))
	saveTestConfig(t, a, func() *model.Config { c := testConfig("alpha"); c.ID = "abcdef0123456789abcdef0123456789"; return c }())
	wantVNC := backend.VNCEndpoint{Host: "127.0.0.1", Port: 5907}
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning, RunningConfigSHA256: "different", Backend: "qemu", VNC: &wantVNC}}
	code, out, stderr := runCLI(a, "status", "zeta", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("status failed: stderr=%q", stderr)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "state", "restart_required", "vnc"} {
		if _, ok := row[key]; !ok {
			t.Errorf("status omitted required field %q: %s", key, out)
		}
	}
	if string(row["restart_required"]) != "true" {
		t.Fatalf("hash mismatch not reported: %s", out)
	}
	var gotVNC backend.VNCEndpoint
	if err := json.Unmarshal(row["vnc"], &gotVNC); err != nil {
		t.Fatalf("decode vnc: %v", err)
	}
	if gotVNC != wantVNC {
		t.Fatalf("vnc = %+v, want %+v", gotVNC, wantVNC)
	}

	invalidDir := filepath.Join(a.Store.DataRoot, "broken")
	if err := os.Mkdir(invalidDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidDir, "config.json"), []byte(`{"nope":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = runCLI(a, "list", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("list failed: stderr=%q", stderr)
	}
	var rows []StatusRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Name != "alpha" || rows[1].Name != "broken" || rows[1].State != model.RunStateFailed || rows[1].Error == "" || rows[2].Name != "zeta" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if rows[0].VNC == nil || *rows[0].VNC != wantVNC || rows[1].VNC != nil || rows[2].VNC == nil || *rows[2].VNC != wantVNC {
		t.Fatalf("unexpected vnc rows: %+v", rows)
	}
}

func TestRuntimeAdapterStatusCopiesLiveVNCEndpoint(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	status := supervisor.Status{
		State:               model.RunStatePaused,
		Backend:             model.BackendQEMU,
		SupervisorPID:       11,
		BackendPID:          22,
		StartedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		RunningConfigSHA256: strings.Repeat("b", 64),
		VNC:                 &backend.VNCEndpoint{Host: "127.0.0.1", Port: 5908},
	}
	errCh := serveRuntimeStatus(t, a.Store.Paths(cfg).ControlSocket, status)
	row, err := newRuntimeAdapter(lifecycle.NewService(a.Store)).Status(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != model.RunStatePaused || row.PID == nil || *row.PID != status.BackendPID || row.VNC == nil || *row.VNC != *status.VNC {
		t.Fatalf("row = %+v", row)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestNamedInvalidStatusJSONIsAFailedRow(t *testing.T) {
	a := testApp(t)
	code, out, stderr := runCLI(a, "status", "missing", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	var row StatusRow
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatal(err)
	}
	if row.Name != "missing" || row.State != model.RunStateFailed || row.RestartRequired || row.Error == "" {
		t.Fatalf("unexpected row: %+v", row)
	}
}

func TestDeleteRequiresForceAndRefusesAutostartOrRunning(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	code, _, stderr := runCLI(a, "delete", "vm")
	if code != 1 || !strings.Contains(stderr, "noninteractively requires --force") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := a.Store.Load("vm"); err != nil {
		t.Fatalf("refusal deleted VM: %v", err)
	}
	lock, err := a.Store.LockName("vm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = lock.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Autostart.Scope = model.AutostartLogin
	if err = lock.Save(cfg); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	code, _, stderr = runCLI(a, "delete", "vm", "--force")
	if code != 1 || !strings.Contains(stderr, "autostart disable") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}

	lock, err = a.Store.LockName("vm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ = lock.Load()
	cfg.Autostart.Scope = model.AutostartNone
	if err = lock.Save(cfg); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	configureAbsentLaunchd(t, a)
	nameLock, err := a.Store.LockName("vm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = nameLock.Load()
	if err != nil {
		t.Fatal(err)
	}
	lifetime, err := nameLock.LockLifetime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = nameLock.Close(); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runCLI(a, "delete", "vm", "--force")
	if code != 1 || !strings.Contains(stderr, `VM "vm" is running`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err = a.Store.Load("vm"); err != nil {
		t.Fatalf("running-lifetime refusal deleted VM: %v", err)
	}
	if err = lifetime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteForceRemovesStoppedVMWithNoLaunchdJobs(t *testing.T) {
	a := testApp(t)
	saveTestConfig(t, a, testConfig("vm"))
	configureAbsentLaunchd(t, a)

	code, _, stderr := runCLI(a, "delete", "vm", "--force")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := a.Store.Load("vm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted VM remains loadable: %v", err)
	}
}

func TestConsoleAdmissionRejectsStoppedVMWithoutDialing(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	a.Lifecycle = lifecycle.NewService(a.Store)
	code, _, stderr := runCLI(a, "console", "vm")
	if code != 1 || !strings.Contains(stderr, "console requires running or paused") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestMonitorCommandRejectsStoppedVMWithoutDialing(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateStopped}}
	dialed := false
	a.DialQMP = func(context.Context, string) (MonitorClient, error) {
		dialed = true
		return &fakeMonitorClient{}, nil
	}
	code, stdout, stderr := runCLI(a, "monitor", "vm", "info status")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "monitor requires running or paused") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if dialed {
		t.Fatal("DialQMP ran for stopped VM")
	}
}

func TestMonitorInteractiveUsesMonitorSocket(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning}}
	paths := a.Store.Paths(cfg)
	received := make(chan string, 1)
	serveUnixSocket(t, paths.Monitor, func(conn net.Conn) error {
		buffer := make([]byte, len("info version\n"))
		if _, err := io.ReadFull(conn, buffer); err != nil {
			return err
		}
		received <- string(buffer)
		if _, err := conn.Write([]byte("QEMU monitor ready\n")); err != nil {
			return err
		}
		return nil
	})

	stdin, input := net.Pipe()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = input.Close()
	})
	stdout := &signalBuffer{written: make(chan struct{})}
	var stderr bytes.Buffer
	go func() {
		_, _ = input.Write([]byte("info version\n"))
		<-stdout.written
		_ = input.Close()
	}()
	code := a.Run(context.Background(), []string{"monitor", "vm"}, stdin, stdout, &stderr)
	if code != 0 || !strings.Contains(stderr.String(), "Connecting to QEMU monitor") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "QEMU monitor ready\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if got := <-received; got != "info version\n" {
		t.Fatalf("monitor input = %q, want %q", got, "info version\n")
	}
}

func TestMonitorCommandUsesQMPCommandSocketAndClosesClient(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning}}
	client := &fakeMonitorClient{output: "status: running"}
	dialPath := ""
	a.DialQMP = func(_ context.Context, path string) (MonitorClient, error) {
		dialPath = path
		return client, nil
	}
	code, stdout, stderr := runCLI(a, "monitor", "vm", "info status")
	if code != 0 || !strings.Contains(stderr, "Connecting to QEMU monitor") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "status: running\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if dialPath != a.Store.Paths(cfg).QMPCommand {
		t.Fatalf("dial path = %q, want %q", dialPath, a.Store.Paths(cfg).QMPCommand)
	}
	if client.command != "info status" {
		t.Fatalf("command = %q, want %q", client.command, "info status")
	}
	if !client.closed {
		t.Fatal("monitor client was not closed")
	}
}

func TestMonitorCommandValidatesArgumentsAndAvailability(t *testing.T) {
	t.Run("empty command", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		saveTestConfig(t, a, cfg)
		a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning}}
		dialed := false
		a.DialQMP = func(context.Context, string) (MonitorClient, error) {
			dialed = true
			return &fakeMonitorClient{}, nil
		}
		code, stdout, stderr := runCLI(a, "monitor", "vm", "   ")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "COMMAND must not be empty") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if dialed {
			t.Fatal("DialQMP ran for an empty command")
		}
	})

	t.Run("empty command precedes config lookup", func(t *testing.T) {
		a := testApp(t)
		a.Runtime = &fakeRuntime{err: errors.New("runtime status must not run")}
		code, stdout, stderr := runCLI(a, "monitor", "missing", "\t ")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "COMMAND must not be empty") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("too many commands", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		saveTestConfig(t, a, cfg)
		a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning}}
		code, stdout, stderr := runCLI(a, "monitor", "vm", "info", "status")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "expected at most one COMMAND") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("missing seam", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		saveTestConfig(t, a, cfg)
		a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning}}
		a.DialQMP = nil
		code, stdout, stderr := runCLI(a, "monitor", "vm", "info status")
		if code != 1 || stdout != "" || stderr != "runtime: monitor is unavailable\n" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})
}

func TestGuestAgentRejectsMalformedRequestAndAdmissionFailures(t *testing.T) {
	t.Run("malformed request", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		cfg.GuestAgent.Enabled = true
		saveTestConfig(t, a, cfg)
		called := false
		a.CallGuestAgent = func(context.Context, string, qemu.GuestAgentRequest) (json.RawMessage, error) {
			called = true
			return nil, nil
		}
		code, stdout, stderr := runCLI(a, "guest-agent", "vm", "not-json")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "guest-agent:") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if called {
			t.Fatal("CallGuestAgent ran for malformed request")
		}
	})

	t.Run("missing request", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		cfg.GuestAgent.Enabled = true
		saveTestConfig(t, a, cfg)
		called := false
		a.CallGuestAgent = func(context.Context, string, qemu.GuestAgentRequest) (json.RawMessage, error) {
			called = true
			return nil, nil
		}
		code, stdout, stderr := runCLI(a, "guest-agent", "vm")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "missing REQUEST") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if called {
			t.Fatal("CallGuestAgent ran without a request")
		}
	})

	t.Run("too many arguments", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		cfg.GuestAgent.Enabled = true
		saveTestConfig(t, a, cfg)
		called := false
		a.CallGuestAgent = func(context.Context, string, qemu.GuestAgentRequest) (json.RawMessage, error) {
			called = true
			return nil, nil
		}
		code, stdout, stderr := runCLI(a, "guest-agent", "vm", `{"execute":"guest-info"}`, "extra")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "unexpected arguments") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if called {
			t.Fatal("CallGuestAgent ran for extra arguments")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		saveTestConfig(t, a, cfg)
		called := false
		a.CallGuestAgent = func(context.Context, string, qemu.GuestAgentRequest) (json.RawMessage, error) {
			called = true
			return nil, nil
		}
		code, stdout, stderr := runCLI(a, "guest-agent", "vm", `{"execute":"guest-info"}`)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "does not have the guest agent enabled") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if called {
			t.Fatal("CallGuestAgent ran for disabled guest agent")
		}
	})

	t.Run("stopped", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		cfg.GuestAgent.Enabled = true
		saveTestConfig(t, a, cfg)
		a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateStopped}}
		called := false
		a.CallGuestAgent = func(context.Context, string, qemu.GuestAgentRequest) (json.RawMessage, error) {
			called = true
			return nil, nil
		}
		code, stdout, stderr := runCLI(a, "guest-agent", "vm", `{"execute":"guest-info"}`)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "guest-agent requires running or paused") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if called {
			t.Fatal("CallGuestAgent ran for stopped VM")
		}
	})

	t.Run("missing seam", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		cfg.GuestAgent.Enabled = true
		saveTestConfig(t, a, cfg)
		a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning}}
		a.CallGuestAgent = nil
		code, stdout, stderr := runCLI(a, "guest-agent", "vm", `{"execute":"guest-info"}`)
		if code != 1 || stdout != "" || stderr != "runtime: guest-agent is unavailable\n" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})
}

func TestGuestAgentCommandForwardsRequestAndPrintsCompactJSON(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	cfg.GuestAgent.Enabled = true
	saveTestConfig(t, a, cfg)
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning}}
	gotPath := ""
	var gotRequest qemu.GuestAgentRequest
	a.CallGuestAgent = func(_ context.Context, path string, request qemu.GuestAgentRequest) (json.RawMessage, error) {
		gotPath = path
		gotRequest = request
		return json.RawMessage(` { "ok": true, "count": 1 } `), nil
	}
	code, stdout, stderr := runCLI(a, "guest-agent", "vm", `{"execute":"guest-file-open","arguments":{"path":"/tmp/file","mode":"r"}}`)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "{\"ok\":true,\"count\":1}\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if gotPath != a.Store.Paths(cfg).QGA {
		t.Fatalf("QGA path = %q, want %q", gotPath, a.Store.Paths(cfg).QGA)
	}
	if gotRequest.Execute != "guest-file-open" {
		t.Fatalf("execute = %q", gotRequest.Execute)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, gotRequest.Arguments); err != nil {
		t.Fatalf("compact arguments: %v", err)
	}
	if compact.String() != `{"path":"/tmp/file","mode":"r"}` {
		t.Fatalf("arguments = %s", compact.String())
	}
}

func TestVNCAdmissionRejectsDisabledStoppedMissingEndpointAndStaleConfig(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		saveTestConfig(t, a, cfg)
		opened := false
		a.OpenVNC = func(context.Context, backend.VNCEndpoint, string) error {
			opened = true
			return nil
		}
		code, stdout, stderr := runCLI(a, "vnc", "vm")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "does not have VNC enabled") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if opened {
			t.Fatal("OpenVNC ran for disabled VNC")
		}
	})

	t.Run("stopped", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		cfg.VNC = &model.VNCConfig{Bind: "127.0.0.1", Port: 5900, PortTo: 5900, Password: "secret"}
		saveTestConfig(t, a, cfg)
		hash, err := model.Hash(cfg)
		if err != nil {
			t.Fatal(err)
		}
		a.Runtime = &fakeRuntime{row: StatusRow{
			State:               model.RunStateStopped,
			RunningConfigSHA256: hash,
			VNC:                 &backend.VNCEndpoint{Host: "127.0.0.1", Port: 5900},
		}}
		opened := false
		a.OpenVNC = func(context.Context, backend.VNCEndpoint, string) error {
			opened = true
			return nil
		}
		code, stdout, stderr := runCLI(a, "vnc", "vm")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "VNC requires running or paused") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if opened {
			t.Fatal("OpenVNC ran for stopped VM")
		}
	})

	t.Run("missing endpoint", func(t *testing.T) {

		a := testApp(t)
		cfg := testConfig("vm")
		cfg.VNC = &model.VNCConfig{Bind: "127.0.0.1", Port: 5900, PortTo: 5900, Password: "secret"}
		saveTestConfig(t, a, cfg)
		hash, err := model.Hash(cfg)
		if err != nil {
			t.Fatal(err)
		}
		a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning, RunningConfigSHA256: hash}}
		opened := false
		a.OpenVNC = func(context.Context, backend.VNCEndpoint, string) error {
			opened = true
			return nil
		}
		code, stdout, stderr := runCLI(a, "vnc", "vm")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "has no live VNC endpoint") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if opened {
			t.Fatal("OpenVNC ran without a live VNC endpoint")
		}
	})

	t.Run("stale config", func(t *testing.T) {
		a := testApp(t)
		cfg := testConfig("vm")
		cfg.VNC = &model.VNCConfig{Bind: "127.0.0.1", Port: 5900, PortTo: 5900, Password: "secret"}
		saveTestConfig(t, a, cfg)
		a.Runtime = &fakeRuntime{row: StatusRow{
			State:               model.RunStateRunning,
			RunningConfigSHA256: strings.Repeat("a", 64),
			VNC:                 &backend.VNCEndpoint{Host: "127.0.0.1", Port: 5900},
		}}
		opened := false
		a.OpenVNC = func(context.Context, backend.VNCEndpoint, string) error {
			opened = true
			return nil
		}
		code, stdout, stderr := runCLI(a, "vnc", "vm")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "requires restart before VNC can use the current password") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if opened {
			t.Fatal("OpenVNC ran for restart-required VNC")
		}
	})
}

func TestVNCCommandMapsWildcardHostAndPrintsSafeOutput(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	cfg.VNC = &model.VNCConfig{Bind: "127.0.0.1", Port: 5900, PortTo: 5909, Password: "secret"}
	saveTestConfig(t, a, cfg)
	hash, err := model.Hash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Runtime = &fakeRuntime{row: StatusRow{
		State:               model.RunStateRunning,
		RunningConfigSHA256: hash,
		VNC:                 &backend.VNCEndpoint{Host: "0.0.0.0", Port: 5905},
	}}
	var gotEndpoint backend.VNCEndpoint
	gotPassword := ""
	a.OpenVNC = func(_ context.Context, endpoint backend.VNCEndpoint, password string) error {
		gotEndpoint = endpoint
		gotPassword = password
		return nil
	}
	code, stdout, stderr := runCLI(a, "vnc", "vm")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if gotEndpoint != (backend.VNCEndpoint{Host: "127.0.0.1", Port: 5905}) {
		t.Fatalf("endpoint = %+v", gotEndpoint)
	}
	if gotPassword != "secret" {
		t.Fatalf("password = %q, want secret", gotPassword)
	}
	wantOutput := "VNC password copied to clipboard; opening vnc://127.0.0.1:5905\n"
	if stdout != wantOutput {
		t.Fatalf("stdout = %q, want %q", stdout, wantOutput)
	}
	if strings.Contains(stdout, "secret") || strings.Contains(stderr, "secret") {
		t.Fatalf("password leaked to output: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestVNCCommandRejectsNilViewer(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	cfg.VNC = &model.VNCConfig{Bind: "127.0.0.1", Port: 5900, PortTo: 5900, Password: "secret"}
	saveTestConfig(t, a, cfg)
	hash, err := model.Hash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Runtime = &fakeRuntime{row: StatusRow{
		State:               model.RunStateRunning,
		RunningConfigSHA256: hash,
		VNC:                 &backend.VNCEndpoint{Host: "127.0.0.1", Port: 5900},
	}}
	a.OpenVNC = nil
	code, stdout, stderr := runCLI(a, "vnc", "vm")
	if code != 1 || stdout != "" || stderr != "vnc: viewer is unavailable\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestDoctorHumanUsesTableAndProgress(t *testing.T) {
	a := testApp(t)
	code, stdout, stderr := runCLI(a, "doctor")
	if code != 0 && code != 1 {
		t.Fatalf("doctor exit code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "CHECK") || !strings.Contains(stdout, "STATUS") || strings.Contains(stdout, "\t") {
		t.Fatalf("doctor table=%q", stdout)
	}
	if !strings.Contains(stderr, "Running prerequisite checks") {
		t.Fatalf("doctor progress=%q", stderr)
	}
}

func TestDoctorEmitsReportAndFailsWhenRequiredChecksFail(t *testing.T) {
	a := testApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	var errOut strings.Builder
	code := a.Run(ctx, []string{"doctor", "--json"}, strings.NewReader(""), &out, &errOut)
	if code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var checks []map[string]any
	if err := json.Unmarshal([]byte(out.String()), &checks); err != nil {
		t.Fatalf("doctor did not emit JSON: %v (%q)", err, out.String())
	}
	if code != 1 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if len(checks) == 0 {
		t.Fatal("doctor emitted no checks")
	}
}

func TestStopAndStartMissingServicesAreReported(t *testing.T) {
	a := testApp(t)
	saveTestConfig(t, a, testConfig("vm"))
	code, _, stderr := runCLI(a, "stop", "vm", "--timeout", "1s")
	if code != 1 || !strings.Contains(stderr, "lifecycle service is unavailable") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	a.ExecutablePath = "/bin/false"
	code, _, stderr = runCLI(a, "start", "vm", "--foreground")
	if code != 1 || !strings.Contains(stderr, "supervisor service is unavailable") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestStopReportsWaitingProgressUntilSupervisorResponds(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	a.Lifecycle = lifecycle.NewService(a.Store)
	release := make(chan struct{})
	serveUnixSocket(t, a.Store.Paths(cfg).ControlSocket, func(conn net.Conn) error {
		request, err := supervisor.DecodeRequest(conn)
		if err != nil {
			return err
		}
		<-release
		return supervisor.EncodeResponse(conn, &supervisor.Response{
			Version: supervisor.ProtocolVersion,
			ID:      request.ID,
			OK:      true,
		})
	})
	stderr := &signalBuffer{written: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- a.Run(context.Background(), []string{"stop", "vm"}, strings.NewReader(""), io.Discard, stderr)
	}()
	<-stderr.written
	if !strings.Contains(stderr.String(), "Stopping VM (waiting for shutdown response)") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	select {
	case code := <-done:
		t.Fatalf("stop returned before response with code %d", code)
	default:
	}
	close(release)
	if code := <-done; code != 0 {
		t.Fatalf("stop exit code=%d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Stopping VM (waiting for shutdown response) done") {
		t.Fatalf("stop completion missing from stderr=%q", stderr.String())
	}
}

func TestAbsoluteStorePathsIncludesMonitorSocketsAndVNCSecret(t *testing.T) {
	paths := store.Paths{
		VMDir: "vm", Config: "config.json", RuntimeDir: "runtime", ControlSocket: "control.sock",
		LifetimeLock: "lifetime.lock", QMP: "qmp.sock", QMPCommand: "qmp-command.sock",
		QGA: "qga.sock", Console: "console.sock", Monitor: "monitor.sock",
		VNCSecret: "vnc-password", RuntimeMetadata: "runtime.json", LastExitMetadata: "last_exit.json",
		SupervisorStdout: "supervisor.stdout.log", SupervisorStderr: "supervisor.stderr.log",
		QEMULog: "qemu.log", SerialLog: "serial.log",
	}
	got, err := absoluteStorePaths(paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		got.VMDir, got.Config, got.RuntimeDir, got.ControlSocket, got.LifetimeLock, got.QMP, got.QMPCommand,
		got.QGA, got.Console, got.Monitor, got.VNCSecret, got.RuntimeMetadata, got.LastExitMetadata,
		got.SupervisorStdout, got.SupervisorStderr, got.QEMULog, got.SerialLog,
	} {
		if !filepath.IsAbs(path) {
			t.Fatalf("path %q is not absolute", path)
		}
	}
	wantSecret, err := filepath.Abs(paths.VNCSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got.VNCSecret != wantSecret {
		t.Fatalf("VNC secret = %q, want %q", got.VNCSecret, wantSecret)
	}
}

func TestBackendPathsIncludePrivateMonitorSockets(t *testing.T) {
	paths := store.Paths{
		VMDir: "/vm", QMP: "/run/qmp.sock", QMPCommand: "/run/qmp-command.sock", QGA: "/run/qga.sock",
		Console: "/run/console.sock", Monitor: "/run/monitor.sock", QEMULog: "/logs/qemu.log", SerialLog: "/logs/serial.log",
	}
	got := backendPaths(paths)
	if got.VMDir != paths.VMDir || got.QMP != paths.QMP || got.QMPCommand != paths.QMPCommand ||
		got.QGA != paths.QGA || got.Console != paths.Console || got.Monitor != paths.Monitor ||
		got.QEMULog != paths.QEMULog || got.SerialLog != paths.SerialLog {
		t.Fatalf("backend paths = %#v", got)
	}
}
