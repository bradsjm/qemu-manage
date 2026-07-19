package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bradsjm/qemu-manage/internal/model"
)

type disabledDomain struct {
	domain     domain
	inspection pathInspection
	backup     pathInspection
	loaded     bool
	removed    bool
	bootedOut  bool
}

// Disable stops the VM and transactionally removes its jobs from both launchd
// domains before recording that autostart is disabled.
func (m *Manager) Disable(ctx context.Context, name string) error {
	if m == nil || m.Store == nil {
		return errors.New("launchd: manager has no store")
	}
	nameLock, err := m.Store.LockName(name)
	if err != nil {
		return err
	}
	defer nameLock.Close()

	cfg, err := nameLock.Load()
	if err != nil {
		return err
	}
	login, system, err := m.inspectBoth(cfg.ID)
	if err != nil {
		return err
	}
	states := []disabledDomain{
		{domain: domainLogin, inspection: login},
		{domain: domainSystem, inspection: system},
	}
	for i := range states {
		states[i].loaded, err = m.printLoaded(ctx, states[i].domain, cfg.ID)
		if err != nil {
			return err
		}
		states[i].backup = states[i].inspection
		if states[i].loaded && !states[i].backup.Present {
			backupCfg := *cfg
			if states[i].domain == domainSystem {
				backupCfg.Autostart.Scope = model.AutostartBoot
			} else {
				backupCfg.Autostart.Scope = model.AutostartLogin
			}
			states[i].backup.Bytes, err = m.renderForConfig(&backupCfg)
			if err != nil {
				return err
			}
			states[i].backup.Present = true
		}
	}

	if m.Stop == nil {
		return errors.New("launchd: authenticated stop callback is not configured")
	}
	if err := m.Stop(ctx, cfg); err != nil {
		return fmt.Errorf("launchd: stop %q: %w", name, err)
	}

	rollback := func(primary error, restoreConfig bool) error {
		var cleanup error
		if restoreConfig {
			cleanup = errors.Join(cleanup, nameLock.Save(cfg))
		}
		for i := len(states) - 1; i >= 0; i-- {
			state := &states[i]
			needsRestore := state.removed || (state.bootedOut && state.loaded && !state.inspection.Present)
			if needsRestore {
				if err := m.restorePlist(ctx, state.domain, cfg.ID, state.backup); err != nil {
					cleanup = errors.Join(cleanup, err)
					continue
				}
			}
			if state.bootedOut && state.loaded {
				current, err := m.inspectPath(state.domain, cfg.ID)
				if err != nil {
					cleanup = errors.Join(cleanup, err)
					continue
				}
				if !current.Present || !bytes.Equal(current.Bytes, state.backup.Bytes) {
					cleanup = errors.Join(cleanup, fmt.Errorf("launchd: refusing to bootstrap changed plist during rollback: %s", state.backup.Path))
					continue
				}
				if err := m.bootstrap(ctx, state.domain, state.backup.Path); err != nil {
					cleanup = errors.Join(cleanup, err)
				}
			}
		}
		return errors.Join(primary, cleanup)
	}

	for i := range states {
		state := &states[i]
		current, inspectErr := m.inspectPath(state.domain, cfg.ID)
		if inspectErr != nil {
			return rollback(inspectErr, false)
		}
		if current.Present != state.inspection.Present ||
			(current.Present && !bytes.Equal(current.Bytes, state.inspection.Bytes)) {
			return rollback(fmt.Errorf("launchd: plist changed during disable: %s", state.inspection.Path), false)
		}
		if state.loaded {
			if err := m.bootout(ctx, state.domain, cfg.ID); err != nil {
				return rollback(err, false)
			}
			state.bootedOut = true
		}
		if state.inspection.Present {
			current, inspectErr = m.inspectPath(state.domain, cfg.ID)
			if inspectErr != nil {
				return rollback(inspectErr, false)
			}
			if !current.Present || !bytes.Equal(current.Bytes, state.inspection.Bytes) {
				return rollback(fmt.Errorf("launchd: plist changed during disable: %s", state.inspection.Path), false)
			}
			if err := m.removePlist(ctx, state.domain, state.inspection.Path); err != nil {
				return rollback(err, false)
			}
			state.removed = true
		}
	}

	updated := *cfg
	updated.Autostart.Scope = model.AutostartNone
	if err := nameLock.Save(&updated); err != nil {
		return rollback(err, true)
	}
	return nil
}

// Status reports both launchd domains without mutating files or acquiring the
// VM name lock. launchctl print is deliberately always unprivileged.
func (m *Manager) Status(ctx context.Context, name string) (StatusReport, error) {
	if m == nil || m.Store == nil {
		return StatusReport{}, errors.New("launchd: manager has no store")
	}
	cfg, err := m.Store.Load(name)
	if err != nil {
		return StatusReport{}, err
	}
	executable, err := stableExecutable(m.Executable)
	if err != nil {
		return StatusReport{}, err
	}
	report := StatusReport{ConfiguredScope: cfg.Autostart.Scope}
	for _, item := range []struct {
		domain domain
		status *DomainStatus
		scope  model.AutostartScope
	}{
		{domainLogin, &report.Login, model.AutostartLogin},
		{domainSystem, &report.Boot, model.AutostartBoot},
	} {
		inspection, inspectErr := m.inspectPath(item.domain, cfg.ID)
		if inspectErr != nil {
			return StatusReport{}, inspectErr
		}
		item.status.FilePresent = inspection.Present
		if inspection.Present {
			expectedCfg := *cfg
			expectedCfg.Autostart.Scope = item.scope
			paths := m.Store.Paths(&expectedCfg)
			expected, renderErr := Render(&expectedCfg, executable, paths.VMDir, paths.SupervisorStdout, paths.SupervisorStderr, m.Username, m.Home, m.Store.DataRoot, m.Store.RuntimeRoot, m.Store.LogRoot)
			if renderErr != nil {
				return StatusReport{}, renderErr
			}
			item.status.FileMatch = bytes.Equal(inspection.Bytes, expected)
		}
		item.status.Loaded, err = m.printLoaded(ctx, item.domain, cfg.ID)
		if err != nil {
			item.status.Error = err.Error()
		}
	}
	return report, nil
}

func (m *Manager) renderForConfig(cfg *model.Config) ([]byte, error) {
	paths := m.Store.Paths(cfg)
	return Render(cfg, m.Executable, paths.VMDir, paths.SupervisorStdout, paths.SupervisorStderr, m.Username, m.Home, m.Store.DataRoot, m.Store.RuntimeRoot, m.Store.LogRoot)
}

func (m *Manager) restorePlist(ctx context.Context, d domain, id string, inspection pathInspection) error {
	if !inspection.Present {
		return nil
	}
	candidate, err := os.CreateTemp("", "qemu-manage-launchd-restore-*.plist")
	if err != nil {
		return fmt.Errorf("launchd: create rollback candidate: %w", err)
	}
	candidatePath := candidate.Name()
	defer os.Remove(candidatePath)
	if err := candidate.Chmod(0600); err != nil {
		candidate.Close()
		return fmt.Errorf("launchd: protect rollback candidate: %w", err)
	}
	if _, err := candidate.Write(inspection.Bytes); err != nil {
		candidate.Close()
		return fmt.Errorf("launchd: write rollback candidate: %w", err)
	}
	if err := candidate.Close(); err != nil {
		return fmt.Errorf("launchd: close rollback candidate: %w", err)
	}
	if err := m.lint(ctx, candidatePath); err != nil {
		return err
	}
	if err := m.installCandidate(ctx, d, candidatePath, inspection.Path); err != nil {
		return errors.Join(err, m.removeInstalledIfExact(ctx, d, id, inspection.Bytes))
	}
	if err := m.verifyInstalled(d, inspection.Path, inspection.Bytes); err != nil {
		return errors.Join(err, m.removePlist(ctx, d, inspection.Path))
	}
	return nil
}
