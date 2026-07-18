package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qemu-manage/internal/launchd"
	"qemu-manage/internal/lifecycle"
	"qemu-manage/internal/model"
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

func TestStatusAndListJSONContracts(t *testing.T) {
	a := testApp(t)
	saveTestConfig(t, a, testConfig("zeta"))
	saveTestConfig(t, a, func() *model.Config { c := testConfig("alpha"); c.ID = "abcdef0123456789abcdef0123456789"; return c }())
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateRunning, RunningConfigSHA256: "different", Backend: "qemu"}}
	code, out, stderr := runCLI(a, "status", "zeta", "--json")
	if code != 0 {
		t.Fatalf("status failed: %s", stderr)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "state", "restart_required"} {
		if _, ok := row[key]; !ok {
			t.Errorf("status omitted required field %q: %s", key, out)
		}
	}
	if string(row["restart_required"]) != "true" {
		t.Fatalf("hash mismatch not reported: %s", out)
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
