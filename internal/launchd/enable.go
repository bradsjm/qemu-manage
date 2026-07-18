package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"qemu-manage/internal/model"
)

// Enable installs and loads the launchd job for name. The name mutation lock is
// held for the entire transaction, including rollback.
func (m *Manager) Enable(ctx context.Context, name string, scope model.AutostartScope, doctor func(context.Context, *model.Config) error) error {
	if m == nil || m.Store == nil {
		return errors.New("launchd: manager has no store")
	}
	if scope != model.AutostartBoot && scope != model.AutostartLogin {
		return fmt.Errorf("launchd: invalid autostart scope %q", scope)
	}
	if doctor == nil {
		return errors.New("launchd: runtime doctor is required")
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
	if cfg.Autostart.Scope != model.AutostartNone {
		return fmt.Errorf("launchd: VM %q already has autostart scope %q; disable it before enabling", name, cfg.Autostart.Scope)
	}
	if err := cfg.ValidateRuntime(); err != nil {
		return err
	}

	// Holding the lifetime lock proves that no supervisor can be starting,
	// running, paused, or stopping while the static launchd job is generated.
	if m.Stopped == nil {
		return errors.New("launchd: stopped-state check is required")
	}
	if err := m.Stopped(ctx, cfg); err != nil {
		return fmt.Errorf("launchd: VM %q must be stopped: %w", name, err)
	}

	lifetime, stopped, err := nameLock.TryLockLifetime(cfg)
	if err != nil {
		return fmt.Errorf("launchd: check VM state: %w", err)
	}
	if !stopped {
		return fmt.Errorf("launchd: VM %q must be stopped before enabling autostart", name)
	}
	defer lifetime.Close()

	if err := doctor(ctx, cfg); err != nil {
		return fmt.Errorf("launchd: runtime doctor: %w", err)
	}
	executable, err := stableExecutable(m.Executable)
	if err != nil {
		return err
	}

	configured := *cfg
	configured.Autostart.Scope = scope
	paths := m.Store.Paths(cfg)
	rendered, err := Render(&configured, executable, paths.VMDir, paths.SupervisorStdout, paths.SupervisorStderr, m.Username, m.Home)
	if err != nil {
		return err
	}

	login, system, err := m.inspectBoth(cfg.ID)
	if err != nil {
		return err
	}
	// A matching file with a none scope is an orphan. Remove its loaded job
	// first, then its immutable-ID path, in both domains.
	for _, item := range []struct {
		d domain
		p pathInspection
	}{{domainLogin, login}, {domainSystem, system}} {
		loaded, printErr := m.printLoaded(ctx, item.d, cfg.ID)
		if printErr != nil {
			return printErr
		}
		if loaded {
			if err := m.bootout(ctx, item.d, cfg.ID); err != nil {
				return err
			}
		}
		if item.p.Present {
			// print/bootout are external calls. Reopen with O_NOFOLLOW and
			// require the exact inspected orphan before deleting its path.
			if err := m.verifyInstalled(item.d, item.p.Path, item.p.Bytes); err != nil {
				return fmt.Errorf("launchd: orphan changed before removal: %w", err)
			}
			if err := m.removePlist(ctx, item.d, item.p.Path); err != nil {
				return err
			}
		}
	}

	d := domainLogin
	if scope == model.AutostartBoot {
		d = domainSystem
	}
	destination := m.plistPath(d, cfg.ID)
	candidate, err := writeCandidate(rendered)
	if err != nil {
		return err
	}
	defer os.Remove(candidate)
	if err := m.lint(ctx, candidate); err != nil {
		return err
	}
	if err := m.installCandidate(ctx, d, candidate, destination); err != nil {
		return errors.Join(err, m.removeInstalledIfExact(ctx, d, cfg.ID, rendered))
	}
	installed := true
	cleanupInstalled := func() error {
		if !installed {
			return nil
		}
		installed = false
		if err := m.verifyInstalled(d, destination, rendered); err != nil {
			return fmt.Errorf("launchd: refuse to remove changed installed plist: %w", err)
		}
		return m.removePlist(ctx, d, destination)
	}
	if err := m.verifyInstalled(d, destination, rendered); err != nil {
		// The just-installed candidate failed the mandatory ownership, mode,
		// or byte check. It has never been loaded and must be removed.
		installed = false
		return errors.Join(err, m.removePlist(ctx, d, destination))
	}

	configured.Autostart.Scope = scope
	if err := nameLock.Save(&configured); err != nil {
		// Atomic save can report a directory-sync/close error after rename.
		// Restore the original none-scope config even when Save returned an
		// error, rather than assuming no durable mutation occurred.
		return errors.Join(err, cleanupInstalled(), nameLock.Save(cfg))
	}
	saved := true
	loaded := false
	rollback := func(primary error) error {
		var cleanup []error
		if loaded {
			cleanup = append(cleanup, m.bootout(ctx, d, cfg.ID))
		}
		cleanup = append(cleanup, cleanupInstalled())
		if saved {
			restored := configured
			restored.Autostart.Scope = model.AutostartNone
			cleanup = append(cleanup, nameLock.Save(&restored))
		}
		return errors.Join(append([]error{primary}, cleanup...)...)
	}
	if err := m.enableJob(ctx, d, cfg.ID); err != nil {
		return rollback(err)
	}
	// RunAtLoad may start the supervisor before bootstrap returns. Release the
	// lifetime exclusion only after the durable scope and job enablement are
	// committed, but before bootstrap makes the job runnable.
	if err := lifetime.Close(); err != nil {
		return rollback(fmt.Errorf("launchd: release stopped-state lock: %w", err))
	}
	if err := m.verifyInstalled(d, destination, rendered); err != nil {
		return rollback(fmt.Errorf("launchd: installed plist changed before bootstrap: %w", err))
	}
	if err := m.bootstrap(ctx, d, destination); err != nil {
		// bootstrap may have loaded the job even when launchctl returned an error;
		// attempt bootout during rollback without relying on command output text.
		loaded = true
		return rollback(err)
	}
	loaded = true
	installed = false
	return nil
}

func stableExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("launchd: executable path must be absolute")
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("launchd: resolve executable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("launchd: inspect executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("launchd: executable must be an executable regular file")
	}
	return resolved, nil
}

func writeCandidate(data []byte) (string, error) {
	file, err := os.CreateTemp("", "qemu-manage-launchd-*.plist")
	if err != nil {
		return "", fmt.Errorf("launchd: create candidate: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("launchd: protect candidate: %w", err)
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("launchd: write candidate: %w", err)
	}

	return path, nil
}

func (m *Manager) removeInstalledIfExact(ctx context.Context, d domain, id string, expected []byte) error {
	inspection, err := m.inspectPath(d, id)
	if err != nil {
		return err
	}
	if !inspection.Present {
		return nil
	}
	if err := m.verifyInstalled(d, inspection.Path, expected); err != nil {
		return fmt.Errorf("launchd: refuse to remove unverified install artifact: %w", err)
	}
	return m.removePlist(ctx, d, inspection.Path)
}

func (m *Manager) verifyInstalled(d domain, path string, expected []byte) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("launchd: verify installed plist: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("launchd: verify installed plist: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("launchd: installed plist %s is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("launchd: cannot inspect ownership of %s", path)
	}
	wantUID, wantGID, wantMode := uint32(m.UID), uint32(m.UID), os.FileMode(0600)
	if d == domainSystem {
		wantUID, wantGID, wantMode = 0, 0, 0644
	}
	if stat.Uid != wantUID || (d == domainSystem && stat.Gid != wantGID) || info.Mode().Perm() != wantMode {
		return fmt.Errorf("launchd: installed plist %s has uid %d gid %d mode %04o; want uid %d gid %d mode %04o", path, stat.Uid, stat.Gid, info.Mode().Perm(), wantUID, wantGID, wantMode)
	}
	actual, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("launchd: verify installed plist bytes: %w", err)
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("launchd: installed plist %s does not match rendered bytes", path)
	}
	return nil
}
