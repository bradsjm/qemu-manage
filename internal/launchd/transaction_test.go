package launchd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
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

func TestEnableReconcilesDriftedLoginPlist(t *testing.T) {
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
	if err = os.MkdirAll(m.LoginDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Install a stale login plist whose ProgramArguments reference a different
	// executable, simulating drift after a Homebrew upgrade moved the binary.
	paths := m.Store.Paths(cfg)
	stale, err := Render(cfg, m.Executable+".stale", paths.VMDir, paths.SupervisorStdout, paths.SupervisorStderr, m.Username, m.Home, m.Store.DataRoot, m.Store.RuntimeRoot, m.Store.LogRoot)
	if err != nil {
		t.Fatal(err)
	}
	plistPath := m.plistPath(domainLogin, cfg.ID)
	if err = os.WriteFile(plistPath, stale, 0600); err != nil {
		t.Fatal(err)
	}
	r.hook = func(c runnerCall) ([]byte, error) { return missingPrint(c, cfg.ID) }
	result, err := m.Enable(context.Background(), cfg.Name, model.AutostartLogin, func(context.Context, *model.Config) error { return nil })
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !result.Reconciled || result.AlreadyEnabled {
		t.Fatalf("expected reconcile, got %+v", result)
	}
	expected, err := m.renderExpected(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatalf("plist not reconciled to current render")
	}
}

func TestDisableStoppedBootoutsLoadedJob(t *testing.T) {
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
	plistPath := m.plistPath(domainLogin, cfg.ID)
	if err = os.WriteFile(plistPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	m.Stopped = func(context.Context, *model.Config) error { return nil }
	r.hook = func(c runnerCall) ([]byte, error) {
		if len(c.args) > 0 && c.args[0] == "print" {
			if len(c.args) > 1 && strings.HasPrefix(c.args[1], "system/") {
				return missingPrint(c, cfg.ID)
			}
			return []byte("loaded"), nil
		}
		return nil, nil
	}
	if err := m.Disable(context.Background(), cfg.Name); err != nil {
		t.Fatalf("disable stopped loaded VM: %v", err)
	}
	if _, statErr := os.Stat(plistPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("login plist remains: %v", statErr)
	}
	var sawBootout bool
	for _, c := range r.calls {
		if len(c.args) > 0 && c.args[0] == "bootout" {
			sawBootout = true
			if c.privileged {
				t.Fatalf("login bootout should be unprivileged: %#v", c)
			}
		}
	}
	if !sawBootout {
		t.Fatal("disable did not bootout the loaded login job")
	}
	got, err := m.Store.Load(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Autostart.Scope != model.AutostartNone {
		t.Fatalf("scope=%q want none", got.Autostart.Scope)
	}
}

func TestDisableRefusesRunningViaStopped(t *testing.T) {
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
	plistPath := m.plistPath(domainLogin, cfg.ID)
	if err = os.WriteFile(plistPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	m.Stopped = func(context.Context, *model.Config) error { return errors.New("VM is running") }
	r.hook = func(c runnerCall) ([]byte, error) { return missingPrint(c, cfg.ID) }
	err = m.Disable(context.Background(), cfg.Name)
	if !errors.Is(err, ErrVMRunning) {
		t.Fatalf("expected ErrVMRunning, got %v", err)
	}
	if _, statErr := os.Stat(plistPath); statErr != nil {
		t.Fatalf("running refusal deleted plist: %v", statErr)
	}
	for _, c := range r.calls {
		if len(c.args) > 0 && (c.args[0] == "bootout" || c.args[0] == "bootstrap") {
			t.Fatalf("disable mutated loaded state while running: %#v", c)
		}
	}
}

func sawLaunchctl(r *fakeRunner, sub string) bool {
	for _, c := range r.calls {
		if c.path == launchctlPath && len(c.args) > 0 && c.args[0] == sub {
			return true
		}
	}
	return false
}

func TestStartRejectsScopeNone(t *testing.T) {
	m, _, cfg := launchdTestManager(t)
	if err := m.Start(context.Background(), cfg.Name); err == nil {
		t.Fatal("expected error for scope none")
	}
}

func TestStartRefusesRunningVM(t *testing.T) {
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
	m.Stopped = func(context.Context, *model.Config) error { return errors.New("VM is running") }
	if err := m.Start(context.Background(), cfg.Name); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected already-running refusal, got %v", err)
	}
	if sawLaunchctl(r, "bootstrap") || sawLaunchctl(r, "bootout") {
		t.Fatalf("Start mutated launchd while running: %#v", r.calls)
	}
}

func TestStartBootstrapsWhenNotLoaded(t *testing.T) {
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
	if err = os.WriteFile(m.plistPath(domainLogin, cfg.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	r.hook = func(c runnerCall) ([]byte, error) { return missingPrint(c, cfg.ID) }
	if err := m.Start(context.Background(), cfg.Name); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !sawLaunchctl(r, "bootstrap") {
		t.Fatalf("Start did not bootstrap: %#v", r.calls)
	}
	if sawLaunchctl(r, "bootout") {
		t.Fatalf("Start bootout-ed a non-loaded job: %#v", r.calls)
	}
}

func TestStartReloadsLoadedJob(t *testing.T) {
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
	if err = os.WriteFile(m.plistPath(domainLogin, cfg.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	r.hook = func(c runnerCall) ([]byte, error) {
		if len(c.args) > 0 && c.args[0] == "print" {
			return []byte("loaded"), nil
		}
		return nil, nil
	}
	if err := m.Start(context.Background(), cfg.Name); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !sawLaunchctl(r, "bootout") {
		t.Fatalf("Start did not bootout loaded job: %#v", r.calls)
	}
	if !sawLaunchctl(r, "bootstrap") {
		t.Fatalf("Start did not bootstrap after bootout: %#v", r.calls)
	}
	// bootout must precede bootstrap.
	var bootoutIdx, bootstrapIdx = -1, -1
	for i, c := range r.calls {
		if c.path == launchctlPath && len(c.args) > 0 {
			switch c.args[0] {
			case "bootout":
				bootoutIdx = i
			case "bootstrap":
				bootstrapIdx = i
			}
		}
	}
	if bootoutIdx < 0 || bootstrapIdx < 0 || bootoutIdx > bootstrapIdx {
		t.Fatalf("expected bootout before bootstrap: %#v", r.calls)
	}
}

func TestStartReconcilesDriftedPlist(t *testing.T) {
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
	if err = os.MkdirAll(m.LoginDir, 0700); err != nil {
		t.Fatal(err)
	}
	paths := m.Store.Paths(cfg)
	stale, err := Render(cfg, m.Executable+".stale", paths.VMDir, paths.SupervisorStdout, paths.SupervisorStderr, m.Username, m.Home, m.Store.DataRoot, m.Store.RuntimeRoot, m.Store.LogRoot)
	if err != nil {
		t.Fatal(err)
	}
	plistPath := m.plistPath(domainLogin, cfg.ID)
	if err = os.WriteFile(plistPath, stale, 0600); err != nil {
		t.Fatal(err)
	}
	r.hook = func(c runnerCall) ([]byte, error) { return missingPrint(c, cfg.ID) }
	if err := m.Start(context.Background(), cfg.Name); err != nil {
		t.Fatalf("Start: %v", err)
	}
	expected, err := m.renderExpected(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatal("Start did not reconcile the drifted plist before loading")
	}
	if !sawLaunchctl(r, "bootstrap") {
		t.Fatalf("Start did not bootstrap: %#v", r.calls)
	}
}
