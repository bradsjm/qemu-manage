package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/model"
)

func assertBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("operation completed while lock held: %v", err)
	default:
	}
}

func TestNameLockExcludesAndPersists(t *testing.T) {
	s := testStore(t)
	first, err := s.LockName("alpha")
	if err != nil {
		t.Fatal(err)
	}
	path := first.Path()
	done := make(chan error, 1)
	go func() {
		second, err := s.LockName("alpha")
		if err == nil {
			err = second.Close()
		}
		done <- err
	}()
	assertBlocked(t, done)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %v", info.Mode())
	}
}

func TestNameLockMustPrecedeLifetimeLock(t *testing.T) {
	s := testStore(t)
	cfg := testConfig("alpha", "88888888888888888888888888888888")
	if err := s.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	nameLock, err := s.LockName(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	lifetime, err := nameLock.LockLifetime(cfg)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Delete(cfg.Name, func(*model.Config, Paths) error { return nil }) }()
	assertBlocked(t, done)
	if err := lifetime.Close(); err != nil {
		t.Fatal(err)
	}
	assertBlocked(t, done)
	if err := nameLock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSaveSerializesUnderNameLock(t *testing.T) {
	s := testStore(t)
	cfg := testConfig("alpha", "99999999999999999999999999999999")
	if err := s.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	held, err := s.LockName(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}

	updated := *cfg
	updated.CPUs = 9
	done := make(chan error, 1)
	go func() { done <- s.Save(&updated) }()
	assertBlocked(t, done)
	loaded, err := held.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CPUs != cfg.CPUs {
		t.Fatalf("blocked save became visible: CPUs = %d", loaded.CPUs)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	loaded, err = s.Load(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CPUs != updated.CPUs {
		t.Fatalf("serialized save CPUs = %d", loaded.CPUs)
	}
}

func TestDeleteRefusesHeldLifetimeWithoutInspection(t *testing.T) {
	s := testStore(t)
	cfg := testConfig("alpha", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err := s.Create(cfg, nil); err != nil {
		t.Fatal(err)
	}
	nameLock, err := s.LockName(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	lifetime, err := nameLock.LockLifetime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := nameLock.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	err = s.Delete(cfg.Name, func(*model.Config, Paths) error { called = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("Delete error = %v", err)
	}
	if called {
		t.Fatal("inspector called while lifetime lock held")
	}
	if _, err := os.Stat(s.Paths(cfg).Config); err != nil {
		t.Fatalf("running VM config removed: %v", err)
	}
	if err := lifetime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHeldLifetimeCannotDeleteRecreatedName(t *testing.T) {
	s := testStore(t)
	old := testConfig("alpha", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := s.Create(old, nil); err != nil {
		t.Fatal(err)
	}
	oldName, err := s.LockName(old.Name)
	if err != nil {
		t.Fatal(err)
	}
	oldLifetime, err := oldName.LockLifetime(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldName.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a prior, externally completed deletion while its old supervisor still
	// owns the immutable-ID lifetime lock, then reuse the stable name.
	oldPaths := s.Paths(old)
	if err := os.RemoveAll(oldPaths.VMDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(oldPaths.QEMULog)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(oldPaths.RuntimeDir); err != nil {
		t.Fatal(err)
	}
	fresh := testConfig(old.Name, "cccccccccccccccccccccccccccccccc")
	if err := s.Create(fresh, nil); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(fresh.Name, func(got *model.Config, paths Paths) error {
		if got.ID != fresh.ID || paths.RuntimeDir != s.RuntimeDir(fresh.ID) {
			t.Fatalf("delete targeted stale identity: %#v %#v", got, paths)
		}
		return errors.New("stop before removal")
	}); err == nil || !strings.Contains(err.Error(), "stop before removal") {
		t.Fatalf("Delete fresh error = %v", err)
	}
	if _, err := os.Stat(s.Paths(fresh).Config); err != nil {
		t.Fatalf("fresh VM damaged: %v", err)
	}
	if err := oldLifetime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLifetimeLockRejectsSymlinkAndWrongMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, path string)
	}{
		{name: "symlink", prepare: func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "lock")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong-mode", prepare: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			id := "dddddddddddddddddddddddddddddddd"
			dir := s.RuntimeDir(id)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, filepath.Join(dir, "lifetime.lock"))
			if _, _, err := s.TryLifetime(id); err == nil {
				t.Fatal("unsafe lifetime lock accepted")
			}
		})
	}
}
