package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/qemu"
)

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestWriteTablePreservesCellValuesAndStripsANSIWhenNoninteractive(t *testing.T) {
	var output bytes.Buffer
	headers := []string{"NAME", "STATE", "RESTART REQUIRED", "ERROR"}
	rows := [][]string{
		{"vm", "running", "\x1b[93mtrue\x1b[0m", ""},
		{"broken", "failed", "false", "runtime unavailable"},
	}

	if err := writeTable(&output, false, headers, rows); err != nil {
		t.Fatalf("writeTable: %v", err)
	}

	got := output.String()
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("noninteractive table contains ANSI: %q", got)
	}
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Fatalf("table should end with exactly one newline: %q", got)
	}
	for _, want := range []string{"NAME", "STATE", "RESTART REQUIRED", "ERROR", "vm", "running", "true", "broken", "failed", "false", "runtime unavailable"} {
		if !strings.Contains(got, want) {
			t.Fatalf("table output missing %q: %q", want, got)
		}
	}
}

func TestWriteTablePropagatesWriterError(t *testing.T) {
	sentinel := errors.New("write failed")
	err := writeTable(failingWriter{err: sentinel}, false, []string{"FIELD", "VALUE"}, [][]string{{"version", "devel"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v want %v", err, sentinel)
	}
}

func TestCreateSharePersistsAbsoluteDirAndPrintsGuidance(t *testing.T) {
	a := testApp(t)
	a.RunExternal = stubPrimaryDiskCreate()
	a.RequireSMBD = func() error { return nil }

	prereqs := t.TempDir()
	codeFD, varsFD, qemuBin, qemuImg := writeCreatePrereqs(t, prereqs)

	work := t.TempDir()
	withWorkingDir(t, work)
	shareDir := filepath.Join(work, "shared")
	if err := os.Mkdir(shareDir, 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(
		a,
		"create", "vm",
		"--firmware-code", codeFD,
		"--firmware-vars", varsFD,
		"--qemu", qemuBin,
		"--qemu-img", qemuImg,
		"--disk-size", "1GiB",
		"--share", "shared",
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}

	cfg, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.SMBFolder != shareDir {
		t.Fatalf("smb_folder=%q want %q", cfg.Network.SMBFolder, shareDir)
	}
}

func TestCreateShareFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, a *App) []string
		code    int
		stderr  string
	}{
		{
			name: "empty share",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemuBin, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemuBin, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--share", ""}
			},
			code:   2,
			stderr: "share path is empty",
		},
		{
			name: "missing directory",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemuBin, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemuBin, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--share", filepath.Join(root, "missing")}
			},
			code:   1,
			stderr: "create: --share",
		},
		{
			name: "regular file rejected",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemuBin, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				regular := filepath.Join(root, "file")
				if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemuBin, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--share", regular}
			},
			code:   1,
			stderr: "is not a directory",
		},
		{
			name: "duplicate share rejected",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemuBin, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				dir := filepath.Join(root, "share")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemuBin, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--share", dir, "--share", dir}
			},
			code:   2,
			stderr: "only one --share may be specified",
		},
		{
			name: "socket_vmnet incompatible",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemuBin, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				dir := filepath.Join(root, "share")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemuBin, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--network", "socket_vmnet", "--share", dir}
			},
			code:   2,
			stderr: "create: --share is incompatible with socket_vmnet",
		},
		{
			name: "smbd missing surfaces install hint",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemuBin, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				a.RequireSMBD = func() error { return requireSMBDDefault() }
				dir := filepath.Join(root, "share")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemuBin, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--share", dir}
			},
			code:   1,
			stderr: "brew install samba",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			args := tc.prepare(t, a)
			code, stdout, stderr := runCLI(a, args...)
			if code != tc.code || stdout != "" || !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if _, err := a.Store.Load("vm"); err == nil {
				t.Fatal("VM was created on failure")
			}
		})
	}
}

func TestCreateShareFailureEmitsNoStdoutGuidance(t *testing.T) {
	a := testApp(t)
	a.RunExternal = stubPrimaryDiskCreate()
	a.RequireSMBD = func() error { return nil }

	root := t.TempDir()
	codeFD, varsFD, qemuBin, qemuImg := writeCreatePrereqs(t, root)
	shareDir := filepath.Join(root, "share")
	if err := os.Mkdir(shareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Force a post-validation failure by reusing a name with a bad restart policy.
	code, stdout, stderr := runCLI(
		a,
		"create", "vm",
		"--firmware-code", codeFD,
		"--firmware-vars", varsFD,
		"--qemu", qemuBin,
		"--qemu-img", qemuImg,
		"--disk-size", "1GiB",
		"--share", shareDir,
		"--restart-policy", "always",
	)
	if code != 2 {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", stdout)
	}
	if _, err := a.Store.Load("vm"); err == nil {
		t.Fatal("VM was created on failure")
	}
}

func TestStatusNamedEmitsSMBMountGuidance(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	cfg.Network.SMBFolder = "/srv/vm-share"
	saveTestConfig(t, a, cfg)
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateStopped, Backend: "qemu"}}

	code, stdout, stderr := runCLI(a, "status", "vm")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Fatalf("redirected status stdout contains ANSI: %q", stdout)
	}
	for _, want := range []string{
		"NAME", "STATE", "RESTART REQUIRED", "ERROR",
		"vm", "stopped", "false",
		"SMB host folder: /srv/vm-share",
		"Linux guest mount: sudo mount -t cifs //10.0.2.4/qemu /mnt/share -o username=guest",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status stdout missing %q: %q", want, stdout)
		}
	}
}

func TestStatusNamedOmitsSMBMountGuidanceWhenUnset(t *testing.T) {
	a := testApp(t)
	saveTestConfig(t, a, testConfig("vm"))
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateStopped, Backend: "qemu"}}

	code, stdout, stderr := runCLI(a, "status", "vm")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "SMB host folder") || strings.Contains(stdout, "//10.0.2.4/qemu") {
		t.Fatalf("status stdout unexpectedly contains SMB guidance: %q", stdout)
	}
}

func TestStatusNamedJSONExcludesSMBMountGuidance(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	cfg.Network.SMBFolder = "/srv/vm-share"
	saveTestConfig(t, a, cfg)
	a.Runtime = &fakeRuntime{row: StatusRow{State: model.RunStateStopped, Backend: "qemu", VNC: &backend.VNCEndpoint{}}}

	code, stdout, stderr := runCLI(a, "status", "vm", "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	var row StatusRow
	if err := json.Unmarshal([]byte(stdout), &row); err != nil {
		t.Fatalf("decode status json: %v\nstdout=%q", err, stdout)
	}
	if strings.Contains(stdout, "SMB host folder") || strings.Contains(stdout, "//10.0.2.4/qemu") {
		t.Fatalf("status --json unexpectedly contains SMB guidance: %q", stdout)
	}
}

func TestDoctorSambaSMBDCheckOnlyForUserNetworkShare(t *testing.T) {
	cfg := model.Config{Network: model.NetworkConfig{Mode: model.NetworkUser}}
	// No SMBFolder set: the samba_smbd check is absent.
	for _, check := range qemu.Doctor(nil, cfg, backend.RuntimePaths{}) {
		if check.Name == "samba_smbd" {
			t.Fatalf("unexpected samba_smbd check without SMBFolder: %+v", check)
		}
	}

	// SMBFolder set on user network: exactly one samba_smbd check appears.
	cfg.Network.SMBFolder = "/srv/share"
	checks := qemu.Doctor(nil, cfg, backend.RuntimePaths{})
	var smb []qemu.Check
	for _, check := range checks {
		if check.Name == "samba_smbd" {
			smb = append(smb, check)
		}
	}
	if len(smb) != 1 {
		t.Fatalf("expected exactly one samba_smbd check, got %d: %+v", len(smb), smb)
	}
	// On non-Apple-Silicon test hosts the candidate paths will not exist, so
	// the check reports a fail with the install hint. Either way the evidence
	// must be non-empty and reference the install instruction when missing.
	if smb[0].Status == qemu.CheckFail && !strings.Contains(smb[0].Evidence, "brew install samba") {
		t.Fatalf("fail evidence missing install hint: %q", smb[0].Evidence)
	}

	// socket_vmnet must never surface the samba_smbd check, even with SMBFolder.
	bridge := cfg
	bridge.Network.Mode = model.NetworkSocketVMNet
	bridge.Network.SocketVMNet = &model.SocketVMNetConfig{ClientPath: "/x", SocketPath: "/y", Interface: "shared"}
	for _, check := range qemu.Doctor(nil, bridge, backend.RuntimePaths{}) {
		if check.Name == "samba_smbd" {
			t.Fatalf("socket_vmnet must not surface samba_smbd: %+v", check)
		}
	}
}
