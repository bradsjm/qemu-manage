package launchd

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/model"
)

func missingPrint(c runnerCall, id string) ([]byte, error) {
	if len(c.args) > 0 && c.args[0] == "print" {
		return []byte(`Could not find service "` + Label(id) + `" in domain for test`), errors.New("exit 113")
	}
	return nil, nil
}

func TestEnableLoginIsNonStartingAndWritesPlist(t *testing.T) {
	m, r, cfg := launchdTestManager(t)
	sawLoadedCheck := 0
	r.hook = func(c runnerCall) ([]byte, error) {
		if len(c.args) > 0 && c.args[0] == "print" {
			sawLoadedCheck++
			return []byte(`Could not find service "` + Label(cfg.ID) + `" in domain for test`), errors.New("exit 113")
		}
		return nil, nil
	}
	result, err := m.Enable(context.Background(), cfg.Name, model.AutostartLogin, func(context.Context, *model.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.AlreadyEnabled || result.Loaded || result.Scope != model.AutostartLogin {
		t.Fatalf("unexpected result: %+v", result)
	}
	if sawLoadedCheck != 2 {
		t.Fatalf("expected two print checks, got %d", sawLoadedCheck)
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
		if len(c.args) > 0 && (c.args[0] == "bootstrap" || c.args[0] == "kickstart" || c.args[0] == "bootout") {
			t.Fatalf("enable unexpectedly loaded or unloaded a job: %#v", c)
		}
	}
}

func TestEnableAlreadyConfiguredIsNoOp(t *testing.T) {
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
	result, err := m.Enable(context.Background(), cfg.Name, model.AutostartLogin, func(context.Context, *model.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyEnabled || result.Scope != model.AutostartLogin {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(r.calls) != 2 || r.calls[0].args[0] != "print" || r.calls[1].args[0] != "print" {
		t.Fatalf("enable should only inspect loaded state, got: %#v", r.calls)
	}
}

func TestDisableRefusesLoadedVMWithoutMutation(t *testing.T) {
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
	r.hook = func(c runnerCall) ([]byte, error) { return []byte("loaded"), nil }
	err = m.Disable(context.Background(), cfg.Name)
	if err == nil || !errors.Is(err, ErrVMRunning) {
		t.Fatalf("running refusal error=%v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("running refusal deleted plist: %v", statErr)
	}
	got, err := m.Store.Load(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Autostart.Scope != model.AutostartLogin {
		t.Fatalf("scope=%q", got.Autostart.Scope)
	}
	for _, c := range r.calls {
		if len(c.args) > 0 && (c.args[0] == "bootout" || c.args[0] == "enable") {
			t.Fatalf("disable mutated loaded job: %#v", c)
		}
	}
}

func TestDisableSuccessfulRemovalWhenStopped(t *testing.T) {
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
	for _, c := range r.calls {
		if c.privileged {
			t.Fatalf("disable should not use privileged runner: %#v", c)
		}
		if len(c.args) > 0 && c.args[0] == "bootout" {
			t.Fatalf("disable should not bootout stopped VM: %#v", c)
		}
	}
}
