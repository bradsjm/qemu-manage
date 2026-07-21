package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/store"
)

type runnerCall struct {
	privileged bool
	path       string
	args       []string
}
type fakeRunner struct {
	calls []runnerCall
	hook  func(runnerCall) ([]byte, error)
}

func (r *fakeRunner) Run(_ context.Context, privileged bool, path string, args ...string) ([]byte, error) {
	c := runnerCall{privileged, path, append([]string(nil), args...)}
	r.calls = append(r.calls, c)
	if r.hook != nil {
		return r.hook(c)
	}
	return nil, nil
}

func launchdTestConfig() *model.Config {
	return &model.Config{SchemaVersion: model.SchemaVersion, ID: "0123456789abcdef0123456789abcdef", Name: "vm", Backend: model.BackendQEMU, Architecture: "aarch64", UUID: "123e4567-e89b-42d3-a456-426614174000", CPUs: 2, MemoryMiB: 2048, RestartPolicy: model.RestartNever, ShutdownTimeoutSeconds: 180, Firmware: model.FirmwareConfig{Code: "code.fd", Variables: "vars.fd"}, Disks: []model.DiskConfig{{Path: "disk.qcow2", Format: "qcow2", Serial: "disk0", BootIndex: 0}}, Network: model.NetworkConfig{Mode: model.NetworkUser, MAC: "02:00:00:00:00:01", Forwards: []model.PortForward{}}, QEMU: model.QEMUConfig{Binary: "/bin/true", ImageTool: "/bin/true", Machine: "virt", ExtraArgs: []string{}}, Autostart: model.AutostartConfig{Scope: model.AutostartNone}}
}

func launchdTestManager(t *testing.T) (*Manager, *fakeRunner, *model.Config) {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(filepath.Join(root, "data"), filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := launchdTestConfig()
	if err := st.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "qemu-manage")
	if err := os.WriteFile(exe, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{}
	m := NewManager(st, exe, "alice", root, os.Getuid())
	m.Runner = r
	m.LoginDir = filepath.Join(root, "agents")
	m.SystemDir = filepath.Join(root, "daemons")
	return m, r, cfg
}

func TestPrintLoadedIsAlwaysUnprivilegedAndDistinguishesErrors(t *testing.T) {
	m, r, cfg := launchdTestManager(t)
	r.hook = func(c runnerCall) ([]byte, error) {
		return []byte(`Could not find service "` + Label(cfg.ID) + `" in domain for user`), errors.New("exit 113")
	}
	loaded, err := m.printLoaded(context.Background(), domainSystem, cfg.ID)
	if err != nil || loaded {
		t.Fatalf("missing service: loaded=%v err=%v", loaded, err)
	}
	if len(r.calls) != 1 || r.calls[0].privileged || !reflect.DeepEqual(r.calls[0].args, []string{"print", "system/" + Label(cfg.ID)}) {
		t.Fatalf("unexpected print call: %#v", r.calls)
	}
	r.hook = func(c runnerCall) ([]byte, error) { return []byte("permission denied"), errors.New("exit 1") }
	if _, err = m.printLoaded(context.Background(), domainSystem, cfg.ID); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("execution error suppressed: %v", err)
	}
}

func TestMutationPrivilegeBoundary(t *testing.T) {
	m, r, cfg := launchdTestManager(t)
	ctx := context.Background()
	if err := m.bootstrap(ctx, domainLogin, "/tmp/a"); err != nil {
		t.Fatal(err)
	}
	if err := m.enableJob(ctx, domainLogin, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.bootout(ctx, domainLogin, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.bootstrap(ctx, domainSystem, "/tmp/b"); err != nil {
		t.Fatal(err)
	}
	if err := m.enableJob(ctx, domainSystem, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.bootout(ctx, domainSystem, cfg.ID); err != nil {
		t.Fatal(err)
	}
	for i, c := range r.calls {
		wantPriv := i >= 3
		if c.privileged != wantPriv {
			t.Errorf("call %d privilege=%v want %v: %#v", i, c.privileged, wantPriv, c)
		}
	}
}

func TestStatusReportsBytesAndUsesStableExecutableTarget(t *testing.T) {
	m, r, cfg := launchdTestManager(t)
	cfg.Autostart.Scope = model.AutostartLogin
	lock, err := m.Store.LockName(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err = lock.Save(cfg); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	target := m.Executable
	link := target + "-link"
	if err = os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	m.Executable = link
	pinned, err := stableExecutable(m.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if pinned != link {
		t.Fatalf("stableExecutable resolved the symlink: got %q want %q", pinned, link)
	}
	paths := m.Store.Paths(cfg)
	data, err := Render(cfg, pinned, paths.VMDir, paths.SupervisorStdout, paths.SupervisorStderr, m.Username, m.Home, m.Store.DataRoot, m.Store.RuntimeRoot, m.Store.LogRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<string>"+link+"</string>") {
		t.Fatalf("plist did not pin invoked executable path %q: %s", link, data)
	}
	if err = os.MkdirAll(m.LoginDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(m.plistPath(domainLogin, cfg.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	r.hook = func(c runnerCall) ([]byte, error) { return nil, errors.New("not found") }
	report, err := m.Status(context.Background(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Login.FilePresent || !report.Login.FileMatch {
		t.Fatalf("matching plist not reported: %+v", report)
	}
	if err = os.WriteFile(m.plistPath(domainLogin, cfg.ID), append(data, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = m.Status(context.Background(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if report.Login.FileMatch {
		t.Fatal("changed bytes reported matching")
	}
	for _, c := range r.calls {
		if c.privileged {
			t.Fatalf("status used privilege: %#v", c)
		}
	}
}

func TestPlistProgramArguments(t *testing.T) {
	m, _, cfg := launchdTestManager(t)
	cfg.Autostart.Scope = model.AutostartLogin
	paths := m.Store.Paths(cfg)
	const exe = "/custom/path/qemu-manage"
	data, err := Render(cfg, exe, paths.VMDir, paths.SupervisorStdout, paths.SupervisorStderr, m.Username, m.Home, m.Store.DataRoot, m.Store.RuntimeRoot, m.Store.LogRoot)
	if err != nil {
		t.Fatal(err)
	}
	args, err := plistProgramArguments(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{exe, "start", cfg.Name, "--foreground"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("ProgramArguments = %#v, want %#v", args, want)
	}
}
