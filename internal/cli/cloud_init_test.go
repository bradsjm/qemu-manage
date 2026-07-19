package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCreateCloudInitUserData(t *testing.T) {
	a := testApp(t)
	prereqs := t.TempDir()
	codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, prereqs)
	installer := filepath.Join(prereqs, "installer.iso")
	extraDrive := filepath.Join(prereqs, "extra.raw")
	userData := filepath.Join(prereqs, "user-data")
	if err := os.WriteFile(installer, []byte("installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extraDrive, []byte("extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantUserData := []byte("#cloud-config\nusers: []\n")
	if err := os.WriteFile(userData, wantUserData, 0o600); err != nil {
		t.Fatal(err)
	}

	var metadata []byte
	a.RunExternal = func(_ context.Context, executable string, args []string) error {
		switch executable {
		case qemuImg:
			if len(args) != 5 || args[0] != "create" {
				t.Fatalf("qemu-img args = %v", args)
			}
			return os.WriteFile(args[3], []byte("disk"), 0o600)
		case hdiutilPath:
			if len(args) != 8 {
				t.Fatalf("hdiutil args = %v", args)
			}
			wantArgs := []string{"makehybrid", "-o", args[2], args[3], "-iso", "-joliet", "-default-volume-name", "cidata"}
			if !reflect.DeepEqual(args, wantArgs) {
				t.Fatalf("hdiutil args = %v, want %v", args, wantArgs)
			}
			stagedUserData, err := os.ReadFile(filepath.Join(args[3], "user-data"))
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(stagedUserData, wantUserData) {
				t.Fatalf("staged user-data = %q, want %q", stagedUserData, wantUserData)
			}
			assertFileMode(t, filepath.Join(args[3], "user-data"), 0o600)
			metadata, err = os.ReadFile(filepath.Join(args[3], "meta-data"))
			if err != nil {
				return err
			}
			assertFileMode(t, filepath.Join(args[3], "meta-data"), 0o600)
			return os.WriteFile(args[2], []byte("iso"), 0o600)
		default:
			t.Fatalf("unexpected executable %q", executable)
			return nil
		}
	}

	exit, stdout, stderr := runCLI(a,
		"create", "vm",
		"--firmware-code", codeFD,
		"--firmware-vars", varsFD,
		"--qemu", qemu,
		"--qemu-img", qemuImg,
		"--iso", installer,
		"--drive", "file="+extraDrive+",format=raw",
		"--cloud-init-user-data", userData,
		"--disk-size", "1GiB",
	)
	if exit != 0 {
		t.Fatalf("create exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	cfg, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	wantMetadata := []byte(fmt.Sprintf("{\"instance-id\":%q}\n", cfg.UUID))
	if !reflect.DeepEqual(metadata, wantMetadata) {
		t.Fatalf("meta-data = %q, want %q", metadata, wantMetadata)
	}
	if cfg.Installer == nil || cfg.Installer.BootIndex != 0 {
		t.Fatalf("installer = %+v", cfg.Installer)
	}
	if len(cfg.Disks) != 3 {
		t.Fatalf("disks = %+v", cfg.Disks)
	}
	if cfg.Disks[0].BootIndex != 1 || cfg.Disks[1].BootIndex != 2 {
		t.Fatalf("disk boot order = %+v", cfg.Disks)
	}
	seed := cfg.Disks[2]
	if seed.Path != cloudInitSeedFilename || seed.Format != "raw" || seed.Serial != "cloud-init-"+cfg.ID[:16] || seed.BootIndex != 3 || !seed.ReadOnly {
		t.Fatalf("seed disk = %+v", seed)
	}
	seedPath := filepath.Join(a.Store.DataRoot, "vm", cloudInitSeedFilename)
	assertFileMode(t, seedPath, 0o400)
	entries, err := os.ReadDir(filepath.Join(a.Store.DataRoot, "vm"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".cloud-init-") {
			t.Fatalf("staging directory remains: %s", entry.Name())
		}
	}
	for _, progress := range []string{
		"Copying cloud-init user-data...\nCopying cloud-init user-data done\n",
		"Creating cloud-init seed...\nCreating cloud-init seed done\n",
	} {
		if !strings.Contains(stderr, progress) {
			t.Fatalf("stderr missing %q: %q", progress, stderr)
		}
	}
}

func TestCreateCloudInitEmptyUserData(t *testing.T) {
	a := testApp(t)
	prereqs := t.TempDir()
	codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, prereqs)
	userData := filepath.Join(prereqs, "empty-user-data")
	if err := os.WriteFile(userData, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	observed := false
	a.RunExternal = func(_ context.Context, executable string, args []string) error {
		switch executable {
		case qemuImg:
			return os.WriteFile(args[3], []byte("disk"), 0o600)
		case hdiutilPath:
			info, err := os.Stat(filepath.Join(args[3], "user-data"))
			if err != nil {
				return err
			}
			if info.Size() != 0 || info.Mode().Perm() != 0o600 {
				t.Fatalf("empty user-data size=%d mode=%#o", info.Size(), info.Mode().Perm())
			}
			observed = true
			return os.WriteFile(args[2], []byte("iso"), 0o600)
		default:
			return fmt.Errorf("unexpected executable %q", executable)
		}
	}
	exit, _, stderr := runCLI(a, "create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemu, "--qemu-img", qemuImg, "--cloud-init-user-data", userData, "--disk-size", "1GiB")
	if exit != 0 || !observed {
		t.Fatalf("exit=%d observed=%t stderr=%q", exit, observed, stderr)
	}
}

func TestCreateCloudInitInputFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		input      func(string) string
		exit       int
		wantStderr string
	}{
		{name: "explicit empty", input: func(string) string { return "" }, exit: 2, wantStderr: "create: --cloud-init-user-data must not be empty"},
		{name: "missing", input: func(root string) string { return filepath.Join(root, "missing") }, exit: 1, wantStderr: "create: --cloud-init-user-data"},
		{name: "directory", input: func(root string) string { return root }, exit: 1, wantStderr: "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			prereqs := t.TempDir()
			codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, prereqs)
			a.RunExternal = func(_ context.Context, executable string, _ []string) error {
				t.Fatalf("external executable ran: %s", executable)
				return nil
			}
			exit, _, stderr := runCLI(a, "create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemu, "--qemu-img", qemuImg, "--cloud-init-user-data", tc.input(prereqs))
			if exit != tc.exit || !strings.Contains(stderr, tc.wantStderr) {
				t.Fatalf("exit=%d stderr=%q", exit, stderr)
			}
			if tc.exit == 2 && !strings.Contains(stderr, "Usage:") {
				t.Fatalf("usage failure omitted create help: %q", stderr)
			}
			if _, err := os.Lstat(filepath.Join(a.Store.DataRoot, "vm")); !os.IsNotExist(err) {
				t.Fatalf("failed create left VM directory: %v", err)
			}
		})
	}
}

func TestCreateCloudInitSeedCleansStagingOnFailure(t *testing.T) {
	root := t.TempDir()
	sourceRoot := t.TempDir()
	userData := filepath.Join(sourceRoot, "user-data")
	if err := os.WriteFile(userData, []byte("#cloud-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("hdiutil sentinel")
	a := testApp(t)
	a.RunExternal = func(_ context.Context, executable string, _ []string) error {
		if executable != hdiutilPath {
			t.Fatalf("executable = %q", executable)
		}
		return sentinel
	}
	err := a.createCloudInitSeed(context.Background(), userData, filepath.Join(root, cloudInitSeedFilename), "instance", io.Discard, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".cloud-init-") {
			t.Fatalf("staging directory remains: %s", entry.Name())
		}
	}
}

func TestCreateCloudInitSeedFailureRollsBack(t *testing.T) {
	a := testApp(t)
	prereqs := t.TempDir()
	codeFD, varsFD, qemu, qemuImg := writeCreatePrereqs(t, prereqs)
	userData := filepath.Join(prereqs, "user-data")
	if err := os.WriteFile(userData, []byte("#cloud-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("hdiutil rollback sentinel")
	a.RunExternal = func(_ context.Context, executable string, args []string) error {
		if executable != hdiutilPath {
			t.Fatalf("external executable ran after seed failure: %s %v", executable, args)
		}
		if _, err := os.Stat(filepath.Join(args[3], "user-data")); err != nil {
			t.Fatalf("staged user-data: %v", err)
		}
		return sentinel
	}
	exit, _, stderr := runCLI(a, "create", "vm", "--firmware-code", codeFD, "--firmware-vars", varsFD, "--qemu", qemu, "--qemu-img", qemuImg, "--cloud-init-user-data", userData, "--disk-size", "1GiB")
	if exit != 1 || !strings.Contains(stderr, "Creating cloud-init seed failed: "+sentinel.Error()) || !strings.Contains(stderr, "cloud-init: create seed ISO: "+sentinel.Error()) {
		t.Fatalf("exit=%d stderr=%q", exit, stderr)
	}
	if _, err := os.Lstat(filepath.Join(a.Store.DataRoot, "vm")); !os.IsNotExist(err) {
		t.Fatalf("failed seed left VM directory: %v", err)
	}
	for _, root := range []string{a.Store.RuntimeRoot, a.Store.LogRoot} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("failed seed left entries in %s: %v", root, entries)
		}
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %#o, want %#o", path, info.Mode().Perm(), want)
	}
}
