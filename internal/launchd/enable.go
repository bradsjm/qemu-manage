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

	"github.com/bradsjm/qemu-manage/internal/model"
)

// EnableResult reports what Enable observed and did.
type EnableResult struct {
	// Scope is the autostart scope now configured for the VM. It is set
	// whether Enable installed a new job or returned early because the
	// scope was already configured.
	Scope model.AutostartScope
	// AlreadyEnabled is true when the VM already had the requested scope
	// configured and Enable made no changes.
	AlreadyEnabled bool
	// Loaded is true when a launchd job for the VM was already loaded in
	// either domain when Enable was called. Enable never loads a job; this
	// flag lets the caller advise the user that the VM is already managed
	// by launchd (for example because a previous version bootstrapped it).
	Loaded bool
}

// Enable installs a launchd autostart job for name without loading it, so the
// VM is not started by this operation. The name mutation lock is held for the
// entire transaction, including rollback. The caller is responsible for any
// start or stop; Enable records the configured scope and writes the plist only.
//
// Enable does not require the VM to be stopped, because it never bootstraps the
// job. A job that is already loaded (for example from a prior version that
// bootstrapped on enable, or from a prior boot/login activation) is left
// loaded; only its on-disk plist is reconciled. The VM's current power state is
// never changed.
func (m *Manager) Enable(ctx context.Context, name string, scope model.AutostartScope, doctor func(context.Context, *model.Config) error) (EnableResult, error) {
	if m == nil || m.Store == nil {
		return EnableResult{}, errors.New("launchd: manager has no store")
	}
	if scope != model.AutostartBoot && scope != model.AutostartLogin {
		return EnableResult{}, fmt.Errorf("launchd: invalid autostart scope %q", scope)
	}
	if doctor == nil {
		return EnableResult{}, errors.New("launchd: runtime doctor is required")
	}

	nameLock, err := m.Store.LockName(name)
	if err != nil {
		return EnableResult{}, err
	}
	defer nameLock.Close()

	cfg, err := nameLock.Load()
	if err != nil {
		return EnableResult{}, err
	}
	if err := cfg.Validate(); err != nil {
		return EnableResult{}, err
	}

	login, system, err := m.inspectBoth(cfg.ID)
	if err != nil {
		return EnableResult{}, err
	}
	loginLoaded, err := m.printLoaded(ctx, domainLogin, cfg.ID)
	if err != nil {
		return EnableResult{}, err
	}
	systemLoaded, err := m.printLoaded(ctx, domainSystem, cfg.ID)
	if err != nil {
		return EnableResult{}, err
	}
	loaded := loginLoaded || systemLoaded

	// Idempotent: enabling the already-configured scope is a successful no-op.
	if cfg.Autostart.Scope == scope {
		return EnableResult{Scope: scope, AlreadyEnabled: true, Loaded: loaded}, nil
	}
	if cfg.Autostart.Scope != model.AutostartNone {
		return EnableResult{}, fmt.Errorf("launchd: VM %q already has autostart scope %q; disable it before enabling a different scope", name, cfg.Autostart.Scope)
	}

	if err := doctor(ctx, cfg); err != nil {
		return EnableResult{}, fmt.Errorf("launchd: runtime doctor: %w", err)
	}
	executable, err := stableExecutable(m.Executable)
	if err != nil {
		return EnableResult{}, err
	}

	configured := *cfg
	configured.Autostart.Scope = scope
	paths := m.Store.Paths(cfg)
	rendered, err := Render(&configured, executable, paths.VMDir, paths.SupervisorStdout, paths.SupervisorStderr, m.Username, m.Home, m.Store.DataRoot, m.Store.RuntimeRoot, m.Store.LogRoot)
	if err != nil {
		return EnableResult{}, err
	}

	// A matching file with a none scope is an orphan. A loaded orphan is not
	// bootout-ed here: doing so would terminate a running VM whose job is
	// still loaded. Remove only the stale on-disk path; loading is left to
	// boot/login or to an explicit `qemu-manage start`.
	for _, item := range []struct {
		d domain
		p pathInspection
	}{{domainLogin, login}, {domainSystem, system}} {
		if item.p.Present {
			// print/inspect are external calls. Reopen with O_NOFOLLOW and
			// require the exact inspected orphan before deleting its path.
			if err := m.verifyInstalled(item.d, item.p.Path, item.p.Bytes); err != nil {
				return EnableResult{}, fmt.Errorf("launchd: orphan changed before removal: %w", err)
			}
			if err := m.removePlist(ctx, item.d, item.p.Path); err != nil {
				return EnableResult{}, err
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
		return EnableResult{}, err
	}
	defer os.Remove(candidate)
	if err := m.lint(ctx, candidate); err != nil {
		return EnableResult{}, err
	}
	if err := m.installCandidate(ctx, d, candidate, destination); err != nil {
		return EnableResult{}, errors.Join(err, m.removeInstalledIfExact(ctx, d, cfg.ID, rendered))
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
		return EnableResult{}, errors.Join(err, m.removePlist(ctx, d, destination))
	}

	// Commit the durable scope. Enable never loads the job, so there is no
	// bootstrap step and no stopped-state precondition. If Save fails after
	// the file was written, roll back the file so the durable state and the
	// on-disk job stay consistent.
	if err := nameLock.Save(&configured); err != nil {
		// Atomic save can report a directory-sync/close error after rename.
		return EnableResult{}, errors.Join(err, cleanupInstalled(), nameLock.Save(cfg))
	}

	return EnableResult{Scope: scope, Loaded: loaded}, nil
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
