package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"qemu-manage/internal/model"
)

func missingPrint(c runnerCall, id string) ([]byte, error) {
	if len(c.args) > 0 && c.args[0] == "print" {
		return []byte(`Could not find service "` + Label(id) + `" in domain for test`), errors.New("exit 113")
	}
	return nil, nil
}

func TestEnableLoginOrdersSaveBeforeLoadAndHoldsNameLock(t *testing.T) {
	m, r, cfg := launchdTestManager(t)
	m.Stopped = func(context.Context, *model.Config) error { return nil }
	var sawSaved, sawLock bool
	r.hook = func(c runnerCall) ([]byte, error) {
		if len(c.args) > 0 && c.args[0] == "enable" {
			loaded, err := m.Store.Load(cfg.Name)
			if err != nil {
				return nil, err
			}
			sawSaved = loaded.Autostart.Scope == model.AutostartLogin
			f, err := os.OpenFile(filepath.Join(m.Store.DataRoot, ".locks", cfg.Name+".lock"), os.O_RDWR, 0)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			sawLock = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
		}
		return missingPrint(c, cfg.ID)
	}
	if err := m.Enable(context.Background(), cfg.Name, model.AutostartLogin, func(context.Context, *model.Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !sawSaved || !sawLock {
		t.Fatalf("enable ordering/locking saved=%v lock=%v", sawSaved, sawLock)
	}
	got, err := m.Store.Load(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Autostart.Scope != model.AutostartLogin {
		t.Fatalf("scope=%q", got.Autostart.Scope)
	}
	info, err := os.Stat(m.plistPath(domainLogin, cfg.ID))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("login mode=%#o", info.Mode().Perm())
	}
	for _, c := range r.calls {
		if c.privileged {
			t.Fatalf("login enable invoked privileged runner: %#v", c)
		}
	}
}

func TestEnableRollbackAfterBootstrapFailure(t *testing.T) {
	m, r, cfg := launchdTestManager(t)
	m.Stopped = func(context.Context, *model.Config) error { return nil }
	r.hook = func(c runnerCall) ([]byte, error) {
		if len(c.args) > 0 && c.args[0] == "bootstrap" {
			return []byte("load failed"), errors.New("exit 5")
		}
		return missingPrint(c, cfg.ID)
	}
	err := m.Enable(context.Background(), cfg.Name, model.AutostartLogin, func(context.Context, *model.Config) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "load failed") {
		t.Fatalf("unexpected error %v", err)
	}
	got, loadErr := m.Store.Load(cfg.Name)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got.Autostart.Scope != model.AutostartNone {
		t.Fatalf("rollback scope=%q", got.Autostart.Scope)
	}
	if _, statErr := os.Stat(m.plistPath(domainLogin, cfg.ID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("installed plist survived rollback: %v", statErr)
	}
	var enabled, bootedOut bool
	for _, c := range r.calls {
		if len(c.args) > 0 && c.args[0] == "enable" {
			enabled = true
		}
		if len(c.args) > 0 && c.args[0] == "bootout" {
			bootedOut = true
		}
	}
	if !enabled || !bootedOut {
		t.Fatalf("rollback calls missing: %#v", r.calls)
	}
}

func TestEnableRefusesForeignOrphan(t *testing.T) {
	m, _, cfg := launchdTestManager(t)
	m.Stopped = func(context.Context, *model.Config) error { return nil }
	if err := os.MkdirAll(m.LoginDir, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>foreign.job</string></dict></plist>`)
	if err := os.WriteFile(m.plistPath(domainLogin, cfg.ID), foreign, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(context.Background(), cfg.Name, model.AutostartLogin, func(context.Context, *model.Config) error { return nil }); err == nil || !strings.Contains(err.Error(), "foreign plist") {
		t.Fatalf("foreign error=%v", err)
	}
	got, err := os.ReadFile(m.plistPath(domainLogin, cfg.ID))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(foreign) {
		t.Fatal("foreign plist was modified")
	}
}

func TestDisableRefusalAndSuccessfulRemoval(t *testing.T) {
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
	data, err := m.renderForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(m.LoginDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := m.plistPath(domainLogin, cfg.ID)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	r.hook = func(c runnerCall) ([]byte, error) { return missingPrint(c, cfg.ID) }
	m.Stop = func(context.Context, *model.Config) error { return errors.New("shutdown_timeout") }
	if err = m.Disable(context.Background(), cfg.Name); err == nil || !strings.Contains(err.Error(), "shutdown_timeout") {
		t.Fatalf("stop error=%v", err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("plist removed after stop refusal: %v", err)
	}
	m.Stop = func(context.Context, *model.Config) error { return nil }
	if err = m.Disable(context.Background(), cfg.Name); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist remains: %v", err)
	}
	got, err := m.Store.Load(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Autostart.Scope != model.AutostartNone {
		t.Fatalf("scope=%q", got.Autostart.Scope)
	}
}

func TestBootCandidateVerificationPrecedesBootstrap(t *testing.T) {
	m, r, cfg := launchdTestManager(t)
	m.Stopped = func(context.Context, *model.Config) error { return nil }
	if err := os.MkdirAll(m.SystemDir, 0700); err != nil {
		t.Fatal(err)
	}
	r.hook = func(c runnerCall) ([]byte, error) {
		if c.path == "/usr/bin/install" {
			src, dst := c.args[len(c.args)-2], c.args[len(c.args)-1]
			data, err := os.ReadFile(src)
			if err != nil {
				return nil, err
			}
			return nil, os.WriteFile(dst, append(data, []byte("tampered")...), 0644)
		}
		return missingPrint(c, cfg.ID)
	}
	err := m.Enable(context.Background(), cfg.Name, model.AutostartBoot, func(context.Context, *model.Config) error { return nil })
	if err == nil {
		t.Fatal("tampered/user-owned boot candidate accepted")
	}
	sawInstall := false
	for _, c := range r.calls {
		if c.path == "/usr/bin/install" {
			sawInstall = true
			if !c.privileged || len(c.args) < 6 || strings.Join(c.args[:6], " ") != "-o root -g wheel -m 0644" {
				t.Fatalf("unsafe boot install: %#v", c)
			}
		}
		if len(c.args) > 0 && c.args[0] == "bootstrap" {
			t.Fatalf("bootstrap occurred before verification: %#v", r.calls)
		}
	}
	if !sawInstall {
		t.Fatal("boot candidate was not installed")
	}
}
