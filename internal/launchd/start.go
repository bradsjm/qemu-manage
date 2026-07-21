package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/bradsjm/qemu-manage/internal/model"
)

// Start loads (or reloads) the configured autostart job so launchd runs the
// foreground supervisor, instead of qemu-manage spawning a detached process.
// It is the launchd-owned equivalent of `qemu-manage start` for a VM whose
// autostart scope is boot or login, hiding the launchctl bootstrap from the
// user.
//
// Start reconciles the installed plist to the current executable, bootouts any
// already-loaded job so launchd re-reads the reconciled plist instead of a
// stale in-memory copy (for example a job crash-looping on a binary removed by
// a Homebrew upgrade), and bootstraps the job. The plist sets RunAtLoad, so
// bootstrapping starts the supervisor immediately; Start returns once the job
// is loaded and the caller observes VM readiness separately.
//
// Start refuses while the VM is running (via Stopped when wired), since
// booting out a loaded, running job would terminate it.
func (m *Manager) Start(ctx context.Context, name string) error {
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
	if cfg.Autostart.Scope != model.AutostartBoot && cfg.Autostart.Scope != model.AutostartLogin {
		return fmt.Errorf("launchd: VM %q has no autostart scope; start it directly without launchd", name)
	}
	if m.Stopped != nil {
		if err := m.Stopped(ctx, cfg); err != nil {
			return fmt.Errorf("launchd: VM %q is already running: %w", name, err)
		}
	}

	d := domainLogin
	if cfg.Autostart.Scope == model.AutostartBoot {
		d = domainSystem
	}

	// Ensure launchd runs the current executable, not a stale or missing plist.
	expected, err := m.renderExpected(cfg)
	if err != nil {
		return err
	}
	installed, err := m.inspectPath(d, cfg.ID)
	if err != nil {
		return err
	}
	if !installed.Present || !bytes.Equal(installed.Bytes, expected) {
		if installed.Present {
			if err := m.verifyInstalled(d, installed.Path, installed.Bytes); err != nil {
				return fmt.Errorf("launchd: drifted plist changed before reload: %w", err)
			}
			if err := m.removePlist(ctx, d, installed.Path); err != nil {
				return err
			}
		}
		if _, err := m.installPlist(ctx, d, cfg.ID, expected); err != nil {
			return err
		}
	}

	// Reload any loaded job so it picks up the reconciled plist. A loaded but
	// non-running job (idle after a stop, or crash-looping on a deleted binary)
	// is safe to bootout; the running case was refused above.
	loaded, err := m.printLoaded(ctx, d, cfg.ID)
	if err != nil {
		return err
	}
	if loaded {
		if err := m.bootout(ctx, d, cfg.ID); err != nil {
			return err
		}
	}
	return m.bootstrap(ctx, d, m.plistPath(d, cfg.ID))
}
