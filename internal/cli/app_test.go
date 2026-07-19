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
	return &App{
		Store:           s,
		Geteuid:         func() int { return 501 },
		LookupEnv:       func(string) (string, bool) { return "", false },
		DiscoverMachine: func(context.Context, string) (string, error) { return "virt-11.0", nil },
	}
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

func withWorkingDir(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatal(err)
		}
	})
}

func writeExecutableFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeCreatePrereqs(t *testing.T, root string) (string, string, string, string) {
	t.Helper()
	codeFD := filepath.Join(root, "code.fd")
	varsFD := filepath.Join(root, "vars.fd")
	qemu := filepath.Join(root, "qemu-system-aarch64")
	qemuImg := filepath.Join(root, "qemu-img")
	for _, path := range []string{codeFD, varsFD, qemu, qemuImg} {
		writeExecutableFile(t, path)
	}
	return codeFD, varsFD, qemu, qemuImg
}

func stubPrimaryDiskCreate() func(context.Context, string, []string) error {
	return func(_ context.Context, _ string, args []string) error {
		return os.WriteFile(args[3], []byte("disk"), 0o600)
	}
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
		{[]string{"monitor", "--bad"}, "missing NAME"},
		{[]string{"guest-agent", "--bad"}, "missing NAME"},
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

func TestUSBValuesSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    model.USBDeviceConfig
		wantErr string
	}{
		{
			name: "vendor product normalized",
			raw:  "vendor=00Aa,product=0FfF",
			want: model.USBDeviceConfig{VendorID: "00aa", ProductID: "0fff"},
		},
		{
			name: "bus address order independent",
			raw:  "address=127,bus=255",
			want: model.USBDeviceConfig{HostBus: 255, HostAddress: 127},
		},
		{name: "vendor zero", raw: "vendor=0000,product=0001", wantErr: "vendor: must be between 0001 and ffff"},
		{name: "address too high", raw: "bus=1,address=128", wantErr: "address: must be between 1 and 127"},
		{name: "mixed selectors", raw: "vendor=0001,product=0002,bus=1,address=2", wantErr: "vendor/product cannot be mixed with bus/address"},
		{name: "unknown key", raw: "path=1,address=2", wantErr: "unknown key"},
		{name: "duplicate key", raw: "bus=1,bus=2,address=3", wantErr: "duplicate key"},
		{name: "missing pair", raw: "bus=1", wantErr: "bus and address must be provided together"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var values usbValues
			err := values.Set(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set() error: %v", err)
			}
			if len(values) != 1 || values[0] != tc.want {
				t.Fatalf("values=%#v want %#v", values, tc.want)
			}
		})
	}
}

func TestDriveValuesSetAndDetectDriveFormat(t *testing.T) {
	root := t.TempDir()
	withWorkingDir(t, root)

	var drives driveValues
	if err := drives.Set("aio=native,file=extras/data,,one.img,readonly=on,cache=none,if=virtio"); err != nil {
		t.Fatalf("Set() error: %v", err)
	}
	if len(drives) != 1 {
		t.Fatalf("len(drives)=%d", len(drives))
	}
	wantPath := filepath.Join(root, "extras", "data,one.img")
	if drives[0].Source != wantPath || drives[0].Format != "" || drives[0].Cache != "none" || drives[0].AIO != "native" || !drives[0].ReadOnly {
		t.Fatalf("drive=%+v wantPath=%q", drives[0], wantPath)
	}

	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "missing file", raw: "cache=none", want: "file is required"},
		{name: "duplicate key", raw: "file=disk.img,file=other.img", want: "duplicate key"},
		{name: "unknown key", raw: "file=disk.img,bus=virtio", want: "unknown key"},
		{name: "invalid if", raw: "file=disk.img,if=scsi", want: "valid values: virtio"},
		{name: "invalid format", raw: "file=disk.img,format=vmdk", want: "valid values: raw, qcow2"},
		{name: "invalid cache", raw: "file=disk.img,cache=passthrough", want: "valid values: none, writeback, writethrough, directsync, unsafe"},
		{name: "invalid aio", raw: "file=disk.img,aio=io_uring", want: "valid values: threads, native"},
		{name: "invalid readonly", raw: "file=disk.img,readonly=true", want: "valid values: on, off"},
		{name: "malformed item", raw: "file=disk.img,cache", want: "key=value"},
		{name: "empty item", raw: "file=disk.img,", want: "empty item"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var values driveValues
			err := values.Set(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want substring %q", err, tc.want)
			}
		})
	}

	rawPath := filepath.Join(root, "raw.img")
	if err := os.WriteFile(rawPath, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	qcowPath := filepath.Join(root, "disk.qcow2")
	if err := os.WriteFile(qcowPath, []byte("QFI\xfbrest"), 0o600); err != nil {
		t.Fatal(err)
	}
	if format, err := detectDriveFormat(rawPath); err != nil || format != "raw" {
		t.Fatalf("detectDriveFormat(raw)=%q err=%v", format, err)
	}
	if format, err := detectDriveFormat(qcowPath); err != nil || format != "qcow2" {
		t.Fatalf("detectDriveFormat(qcow2)=%q err=%v", format, err)
	}
	if _, err := detectDriveFormat(root); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("detectDriveFormat(dir) error=%v", err)
	}
}

func TestCreatePersistsUSBAndDetectedExtraDrives(t *testing.T) {
	a := testApp(t)
	a.RunExternal = stubPrimaryDiskCreate()

	prereqs := t.TempDir()
	codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, prereqs)

	work := t.TempDir()
	withWorkingDir(t, work)
	extrasDir := filepath.Join(work, "extras")
	if err := os.MkdirAll(extrasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rawSource := filepath.Join(extrasDir, "alpha,one.raw")
	rawBytes := []byte("raw-extra")
	if err := os.WriteFile(rawSource, rawBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	qcowSource := filepath.Join(extrasDir, "beta.qcow2")
	qcowBytes := []byte("QFI\xfbpayload")
	if err := os.WriteFile(qcowSource, qcowBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runCLI(
		a,
		"create", "vm",
		"--firmware-code", codeFD,
		"--firmware-vars", varsFD,
		"--qemu", qemu,
		"--qemu-img", qemuImg,
		"--disk-size", "1GiB",
		"--usb", "vendor=00Aa,product=0FfF",
		"--usb", "address=127,bus=255",
		"--drive", "file=extras/alpha,,one.raw,cache=none,aio=native",
		"--drive", "readonly=on,file=extras/beta.qcow2",
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}

	cfg, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.USB) != 2 {
		t.Fatalf("len(cfg.USB)=%d", len(cfg.USB))
	}
	if cfg.USB[0] != (model.USBDeviceConfig{VendorID: "00aa", ProductID: "0fff"}) {
		t.Fatalf("usb[0]=%+v", cfg.USB[0])
	}
	if cfg.USB[1] != (model.USBDeviceConfig{HostBus: 255, HostAddress: 127}) {
		t.Fatalf("usb[1]=%+v", cfg.USB[1])
	}
	if len(cfg.Disks) != 3 {
		t.Fatalf("len(cfg.Disks)=%d", len(cfg.Disks))
	}
	if cfg.Disks[0].Path != "disk.qcow2" || cfg.Disks[0].BootIndex != 0 || cfg.Disks[0].Serial != "disk-"+cfg.ID[:16] {
		t.Fatalf("primary disk=%+v id=%q", cfg.Disks[0], cfg.ID)
	}
	if cfg.Disks[1].Path != rawSource || cfg.Disks[1].Format != "raw" || cfg.Disks[1].Cache != "none" || cfg.Disks[1].AIO != "native" || cfg.Disks[1].ReadOnly || cfg.Disks[1].BootIndex != 1 || cfg.Disks[1].Serial != "disk-"+cfg.ID[:12]+"-1" {
		t.Fatalf("disk[1]=%+v id=%q", cfg.Disks[1], cfg.ID)
	}
	if cfg.Disks[2].Path != qcowSource || cfg.Disks[2].Format != "qcow2" || cfg.Disks[2].Cache != "" || cfg.Disks[2].AIO != "" || !cfg.Disks[2].ReadOnly || cfg.Disks[2].BootIndex != 2 || cfg.Disks[2].Serial != "disk-"+cfg.ID[:12]+"-2" {
		t.Fatalf("disk[2]=%+v id=%q", cfg.Disks[2], cfg.ID)
	}
	if data, err := os.ReadFile(rawSource); err != nil || string(data) != string(rawBytes) {
		t.Fatalf("raw source changed data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(qcowSource); err != nil || string(data) != string(qcowBytes) {
		t.Fatalf("qcow source changed data=%q err=%v", data, err)
	}
}

func TestCreatePersistsInstallerBootIndexAndExplicitDriveFormat(t *testing.T) {
	a := testApp(t)
	a.RunExternal = stubPrimaryDiskCreate()

	prereqs := t.TempDir()
	codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, prereqs)

	work := t.TempDir()
	withWorkingDir(t, work)
	isoPath := filepath.Join(work, "installer.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	extraPath := filepath.Join(work, "extra.qcow2")
	if err := os.WriteFile(extraPath, []byte("QFI\xfbpayload"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runCLI(
		a,
		"create", "vm",
		"--firmware-code", codeFD,
		"--firmware-vars", varsFD,
		"--qemu", qemu,
		"--qemu-img", qemuImg,
		"--disk-size", "1GiB",
		"--iso", isoPath,
		"--drive", "file=extra.qcow2,format=raw,readonly=off",
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	cfg, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Installer == nil || cfg.Installer.BootIndex != 0 {
		t.Fatalf("installer=%+v", cfg.Installer)
	}
	if cfg.Disks[0].BootIndex != 1 {
		t.Fatalf("primary boot index=%d", cfg.Disks[0].BootIndex)
	}
	if len(cfg.Disks) != 2 || cfg.Disks[1].Path != extraPath || cfg.Disks[1].Format != "raw" || cfg.Disks[1].ReadOnly || cfg.Disks[1].BootIndex != 2 || cfg.Disks[1].Serial != "disk-"+cfg.ID[:12]+"-1" {
		t.Fatalf("extra disk=%+v id=%q", cfg.Disks[1], cfg.ID)
	}
}

func TestCreateUSBAndDriveFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, a *App) []string
		code    int
		stderr  string
	}{
		{
			name: "usb parse failure",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemu, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--usb", "vendor=0000,product=0001"}
			},
			code:   2,
			stderr: "--usb",
		},
		{
			name: "drive parse failure",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemu, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--drive", "cache=none"}
			},
			code:   2,
			stderr: "--drive",
		},
		{
			name: "usb capacity without vnc",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				return []string{
					"create", "vm",
					"--firmware-code", codeFD,
					"--firmware-vars", varsFD,
					"--qemu", qemu,
					"--qemu-img", qemuImg,
					"--disk-size", "1GiB",
					"--usb", "vendor=0001,product=0001",
					"--usb", "vendor=0001,product=0002",
					"--usb", "bus=1,address=1",
					"--usb", "bus=1,address=2",
					"--usb", "bus=1,address=3",
				}
			},
			code:   2,
			stderr: "at most 4",
		},
		{
			name: "usb capacity with vnc",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				return []string{
					"create", "vm",
					"--firmware-code", codeFD,
					"--firmware-vars", varsFD,
					"--qemu", qemu,
					"--qemu-img", qemuImg,
					"--disk-size", "1GiB",
					"--vnc",
					"--vnc-password", "secret",
					"--usb", "vendor=0001,product=0001",
					"--usb", "bus=1,address=1",
					"--usb", "bus=1,address=2",
				}
			},
			code:   2,
			stderr: "at most 2",
		},
		{
			name: "drive source not regular",
			prepare: func(t *testing.T, a *App) []string {
				root := t.TempDir()
				codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, root)
				a.RunExternal = stubPrimaryDiskCreate()
				dir := filepath.Join(root, "diskdir")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return []string{"create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemu, "--qemu-img", qemuImg, "--disk-size", "1GiB", "--drive", "file=" + dir + ",format=raw"}
			},
			code:   1,
			stderr: "create: --drive file",
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
