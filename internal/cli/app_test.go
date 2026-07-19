package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/store"
)

func testApp(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	s, err := store.New(filepath.Join(root, "vms"), filepath.Join(root, "run"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	return &App{Store: s, Geteuid: func() int { return 501 }}
}

func testConfig(name string) *model.Config {
	return &model.Config{
		SchemaVersion: model.SchemaVersion, ID: "0123456789abcdef0123456789abcdef", Name: name,
		Backend: model.BackendQEMU, Architecture: "aarch64", UUID: "123e4567-e89b-42d3-a456-426614174000",
		CPUs: 2, MemoryMiB: 2048, RestartPolicy: model.RestartNever, ShutdownTimeoutSeconds: 180,
		Firmware:  model.FirmwareConfig{Code: "code.fd", Variables: "vars.fd"},
		Disks:     []model.DiskConfig{{Path: "disk.qcow2", Format: "qcow2", Serial: "disk0", BootIndex: 0}},
		Network:   model.NetworkConfig{Mode: model.NetworkUser, MAC: "02:00:00:00:00:01", Forwards: []model.PortForward{}},
		QEMU:      model.QEMUConfig{Binary: "/usr/bin/false", ImageTool: "/usr/bin/false", Machine: "virt", ExtraArgs: []string{}},
		Autostart: model.AutostartConfig{Scope: model.AutostartNone},
	}
}

func saveTestConfig(t *testing.T, a *App, cfg *model.Config) {
	t.Helper()
	if err := a.Store.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
}

func runCLI(a *App, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := a.Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunRefusesRootBeforeInitializationOrMutation(t *testing.T) {
	a := testApp(t)
	a.Geteuid = func() int { return 0 }
	a.initializationError = os.ErrPermission
	code, out, errOut := runCLI(a, "create", "vm")
	if code != 1 || out != "" || !strings.Contains(errOut, "must not run as root") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	entries, err := os.ReadDir(a.Store.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("root invocation mutated data root: %v", entries)
	}
}

func TestUsageAndNameBeforeFlags(t *testing.T) {
	a := testApp(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{}, "Usage:"},
		{[]string{"wat"}, "unknown command"},
		{[]string{"create", "--cpus", "2", "vm"}, "missing NAME"},
		{[]string{"set", "--cpus", "2", "vm"}, "missing NAME"},
		{[]string{"start", "--foreground", "vm"}, "missing NAME"},
		{[]string{"stop", "--force", "vm"}, "missing NAME"},
		{[]string{"console", "--bad"}, "missing NAME"},
		{[]string{"vnc", "--bad"}, "missing NAME"},
		{[]string{"delete", "--force", "vm"}, "usage:"},
	} {
		code, _, stderr := runCLI(a, tc.args...)
		if code != 2 || !strings.Contains(stderr, tc.want) {
			t.Errorf("args=%v code=%d stderr=%q", tc.args, code, stderr)
		}
	}
}

func TestCreateDispatchPersistsDefaultsWithoutRealQEMU(t *testing.T) {
	a := testApp(t)
	root := t.TempDir()
	codeFD, varsFD, qemu, img := filepath.Join(root, "code"), filepath.Join(root, "vars"), filepath.Join(root, "qemu"), filepath.Join(root, "qemu-img")
	for _, p := range []string{codeFD, varsFD, qemu, img} {
		if err := os.WriteFile(p, []byte("x"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	a.RunExternal = func(_ context.Context, _ string, args []string) error {
		return os.WriteFile(args[3], []byte("disk"), 0600)
	}
	code, _, stderr := runCLI(a, "create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemu, "--qemu-img", img, "--disk-size", "1GiB")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	cfg, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "vm" || cfg.CPUs != 2 || cfg.MemoryMiB != 2048 || cfg.Network.Mode != model.NetworkUser {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}
