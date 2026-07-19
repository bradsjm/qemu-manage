package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qemu-manage/internal/backend"
	"qemu-manage/internal/launchd"
	"qemu-manage/internal/lifecycle"
	"qemu-manage/internal/model"
	"qemu-manage/internal/store"
	"qemu-manage/internal/supervisor"
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
	if code != 0 {
		t.Fatalf("status failed: %s", stderr)
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
	if code != 0 {
		t.Fatalf("list failed: %s", stderr)
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

func TestVZRuntimeCommandsReturnUnavailableError(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	cfg.Backend = model.BackendVZ
	saveTestConfig(t, a, cfg)
	for _, args := range [][]string{{"start", "vm"}, {"doctor", "vm", "--json"}} {
		code, _, stderr := runCLI(a, args...)
		if code != 1 || !strings.Contains(stderr, `backend "vz" is unavailable`) {
			t.Errorf("args=%v code=%d stderr=%q", args, code, stderr)
		}
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

func TestAbsoluteStorePathsIncludesVNCSecret(t *testing.T) {
	paths := store.Paths{
		VMDir: "vm", Config: "config.json", RuntimeDir: "runtime", ControlSocket: "control.sock",
		LifetimeLock: "lifetime.lock", QMP: "qmp.sock", QGA: "qga.sock", Console: "console.sock",
		VNCSecret: "vnc-password", RuntimeMetadata: "runtime.json", LastExitMetadata: "last_exit.json",
		SupervisorStdout: "supervisor.stdout.log", SupervisorStderr: "supervisor.stderr.log",
		QEMULog: "qemu.log", SerialLog: "serial.log",
	}
	got, err := absoluteStorePaths(paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		got.VMDir, got.Config, got.RuntimeDir, got.ControlSocket, got.LifetimeLock, got.QMP, got.QGA,
		got.Console, got.VNCSecret, got.RuntimeMetadata, got.LastExitMetadata, got.SupervisorStdout,
		got.SupervisorStderr, got.QEMULog, got.SerialLog,
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
