package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"qemu-manage/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	s, err := New(filepath.Join(root, "data"), filepath.Join(root, "run"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testConfig(name, id string) *model.Config {
	return &model.Config{
		SchemaVersion:          model.SchemaVersion,
		ID:                     id,
		Name:                   name,
		Backend:                model.BackendQEMU,
		Architecture:           "aarch64",
		UUID:                   "123e4567-e89b-42d3-a456-426614174000",
		CPUs:                   2,
		MemoryMiB:              2048,
		RestartPolicy:          model.RestartNever,
		ShutdownTimeoutSeconds: 180,
		Firmware:               model.FirmwareConfig{Code: "efi-code.fd", Variables: "efi-vars.fd"},
		Disks:                  []model.DiskConfig{{Path: "disk.qcow2", Format: "qcow2", Serial: "disk0", BootIndex: 0}},
		Network:                model.NetworkConfig{Mode: model.NetworkUser, MAC: "02:00:00:00:00:01", Forwards: []model.PortForward{}},
		QEMU:                   model.QEMUConfig{Binary: "/usr/bin/qemu-system-aarch64", ImageTool: "/usr/bin/qemu-img", Machine: "virt", ExtraArgs: []string{}},
		Autostart:              model.AutostartConfig{Scope: model.AutostartNone},
	}
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func TestCreateOwnerModesAtomicSaveAndReload(t *testing.T) {
	oldMask := syscall.Umask(0o077)
	defer syscall.Umask(oldMask)
	s := testStore(t)
	cfg := testConfig("alpha", "11111111111111111111111111111111")
	if err := s.Create(cfg, func(_ *model.Config, paths Paths) error {
		return os.WriteFile(filepath.Join(paths.VMDir, "artifact"), []byte("ok"), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	paths := s.Paths(cfg)
	for _, dir := range []string{s.DataRoot, s.RuntimeRoot, s.LogRoot, paths.VMDir, paths.RuntimeDir, filepath.Dir(paths.QEMULog), filepath.Join(s.DataRoot, locksDirectory)} {
		requireMode(t, dir, 0o700)
	}
	requireMode(t, paths.Config, 0o600)

	cfg.CPUs = 7
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CPUs != 7 || loaded.ID != cfg.ID {
		t.Fatalf("reloaded config = %#v", loaded)
	}
	matches, err := filepath.Glob(filepath.Join(paths.VMDir, ".config-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary config files after save = %v, err %v", matches, err)
	}
}

func TestLoadRejectsSymlinksAndWrongModes(t *testing.T) {
	t.Run("vm directory symlink", func(t *testing.T) {
		s := testStore(t)
		target := filepath.Join(t.TempDir(), "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(s.DataRoot, "alpha")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load("alpha"); err == nil || !strings.Contains(err.Error(), "non-symlink") {
			t.Fatalf("Load error = %v", err)
		}
	})
	t.Run("vm directory mode", func(t *testing.T) {
		s := testStore(t)
		dir := filepath.Join(s.DataRoot, "alpha")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load("alpha"); err == nil || !strings.Contains(err.Error(), "want 0700") {
			t.Fatalf("Load error = %v", err)
		}
	})
	t.Run("config symlink and mode", func(t *testing.T) {
		for _, symlink := range []bool{false, true} {
			t.Run(map[bool]string{false: "mode", true: "symlink"}[symlink], func(t *testing.T) {
				s := testStore(t)
				cfg := testConfig("alpha", "22222222222222222222222222222222")
				if err := s.Create(cfg, nil); err != nil {
					t.Fatal(err)
				}
				path := s.Paths(cfg).Config
				if symlink {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					target := filepath.Join(t.TempDir(), "config")
					if err := os.WriteFile(target, data, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Load("alpha"); err == nil {
					t.Fatal("Load accepted unsafe config")
				}
			})
		}
	})
}

func TestCreateArtifactFailureRollsBackEveryNewDirectory(t *testing.T) {
	s := testStore(t)
	cfg := testConfig("alpha", "33333333333333333333333333333333")
	paths := s.Paths(cfg)
	wantErr := errors.New("artifact failed")
	err := s.Create(cfg, func(_ *model.Config, paths Paths) error {
		if err := os.WriteFile(filepath.Join(paths.VMDir, "partial"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Create error = %v", err)
	}
	for _, path := range []string{paths.VMDir, paths.RuntimeDir, filepath.Dir(paths.QEMULog)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback left %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.DataRoot, locksDirectory, cfg.Name+".lock")); err != nil {
		t.Fatalf("stable name lock missing: %v", err)
	}
}

func TestListSortedAndSkipsOnlyLocksDirectory(t *testing.T) {
	s := testStore(t)
	for i, name := range []string{"zeta", "alpha", "middle"} {
		id := strings.Repeat(string(rune('a'+i)), 32)
		if err := s.Create(testConfig(name, id), nil); err != nil {
			t.Fatal(err)
		}
	}
	configs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(configs))
	for i := range configs {
		got[i] = configs[i].Name
	}
	if want := []string{"alpha", "middle", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	if err := os.WriteFile(filepath.Join(s.DataRoot, "stray"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("List stray error = %v", err)
	}
}

func TestSaveRejectsImmutableIDChange(t *testing.T) {
	s := testStore(t)
	cfg := testConfig("alpha", "44444444444444444444444444444444")
	if err := s.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	cfg.ID = "55555555555555555555555555555555"
	if err := s.Save(cfg); err == nil || !strings.Contains(err.Error(), "immutable id") {
		t.Fatalf("Save error = %v", err)
	}
	loaded, err := s.Load("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != "44444444444444444444444444444444" {
		t.Fatalf("persisted ID = %s", loaded.ID)
	}
}

func TestDeleteSafetyOrderAndExternalArtifactPreservation(t *testing.T) {
	s := testStore(t)
	cfg := testConfig("alpha", "66666666666666666666666666666666")
	external := filepath.Join(t.TempDir(), "external.qcow2")
	if err := os.WriteFile(external, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Disks[0].Path = external
	if err := s.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	paths := s.Paths(cfg)
	for _, path := range []string{paths.QEMULog, filepath.Join(paths.RuntimeDir, "marker")} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	if err := s.Delete(cfg.Name, func(got *model.Config, gotPaths Paths) error {
		called = true
		if got.ID != cfg.ID || gotPaths != paths {
			t.Fatalf("inspector inputs mismatch")
		}
		for _, path := range []string{paths.Config, paths.QEMULog, filepath.Join(paths.RuntimeDir, "marker")} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s removed before inspection: %v", path, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("delete inspector not called")
	}
	for _, path := range []string{paths.VMDir, filepath.Dir(paths.QEMULog), paths.RuntimeDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("delete left %s: %v", path, err)
		}
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "disk" {
		t.Fatalf("external artifact = %q, %v", data, err)
	}
}

func TestDeleteRequiresInspectorAndRejectsAutostart(t *testing.T) {
	s := testStore(t)
	cfg := testConfig("alpha", "77777777777777777777777777777777")
	if err := s.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(cfg.Name, nil); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("Delete nil inspector = %v", err)
	}
	cfg.Autostart.Scope = model.AutostartBoot
	lock, err := s.LockName(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := s.Delete(cfg.Name, func(*model.Config, Paths) error { called = true; return nil }); err == nil || !strings.Contains(err.Error(), "autostart") {
		t.Fatalf("Delete autostart = %v", err)
	}
	if called {
		t.Fatal("inspector called for autostart VM")
	}
}
