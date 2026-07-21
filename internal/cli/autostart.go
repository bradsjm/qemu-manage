package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bradsjm/qemu-manage/internal/launchd"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/qemu"
)

func (a *App) runAutostart(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageErrorf("autostart: missing subcommand")
	}

	subcommand := args[0]
	if subcommand != "enable" && subcommand != "disable" && subcommand != "status" {
		return usageErrorf("autostart: unknown subcommand %q", subcommand)
	}
	name, remaining, err := nameBeforeFlags("autostart "+subcommand, args[1:])
	if err != nil {
		return err
	}
	stdoutInteractive := a.progressInteractive(stdout)

	switch subcommand {
	case "enable":
		flags := quietFlagSet("autostart enable")
		scopeValue := flags.String("scope", string(model.AutostartBoot), "")
		startFlag := flags.Bool("start", false, "")
		if err := parseNoPositionals(flags, "autostart enable", remaining); err != nil {
			return err
		}
		scope := model.AutostartScope(*scopeValue)
		if scope != model.AutostartBoot && scope != model.AutostartLogin {
			return usageErrorf(
				"autostart enable: --scope %q is invalid; valid values: boot, login",
				*scopeValue,
			)
		}
		if a.Launchd == nil {
			return fmt.Errorf("launchd: manager is unavailable")
		}
		var result launchd.EnableResult
		err := withWaitingProgress(stderr, true, a.liveProgressInteractive(stderr), fmt.Sprintf("Updating %s autostart for %s VM", scope, name), fmt.Sprintf("Completed %s autostart update for %s VM", scope, name), func() error {
			var err error
			result, err = a.Launchd.Enable(ctx, name, scope, func(ctx context.Context, config *model.Config) error {
				paths := a.Store.Paths(config)
				checks := qemu.Doctor(ctx, *config, backendPaths(paths))
				return qemu.RequiredPassed(checks)
			})
			return err
		})
		if err != nil {
			return withLaunchdPrefix(err)
		}
		if err := writeAutostartEnableStatus(stdout, stdoutInteractive, name, result); err != nil {
			return err
		}
		if *startFlag {
			// `enable --start` mirrors `systemctl enable --now`: install the job
			// and start the VM now, through launchd (so it is launchd-owned).
			config, err := a.loadQEMUConfig(name)
			if err != nil {
				return err
			}
			if err := a.startVM(ctx, config, false, false, stderr); err != nil {
				return err
			}
		}
		return nil
	case "disable":
		flags := quietFlagSet("autostart disable")
		scopeValue := flags.String("scope", "", "")
		if err := parseNoPositionals(flags, "autostart disable", remaining); err != nil {
			return err
		}
		// --scope is accepted for symmetry with enable but is informational
		// only: a VM has one autostart scope at a time, so disable always
		// removes the configured autostart regardless of the value passed.
		if value := *scopeValue; value != "" {
			scope := model.AutostartScope(value)
			if scope != model.AutostartBoot && scope != model.AutostartLogin {
				return usageErrorf("autostart disable: --scope %q is invalid; valid values: boot, login", value)
			}
		}
		if a.Launchd == nil {
			return fmt.Errorf("launchd: manager is unavailable")
		}
		err := withWaitingProgress(stderr, true, a.liveProgressInteractive(stderr), fmt.Sprintf("Disabling autostart for %s VM", name), fmt.Sprintf("Disabled autostart for %s VM", name), func() error {
			return a.Launchd.Disable(ctx, name)
		})
		if err != nil {
			if errors.Is(err, launchd.ErrVMRunning) {
				return writeAutostartDisableRefusal(stdout, stdoutInteractive, name, err)
			}
			return withLaunchdPrefix(err)
		}
		return writeAutostartDisableStatus(stdout, stdoutInteractive, name)
	case "status":
		if len(remaining) != 0 {
			return usageErrorf("autostart status: unexpected arguments")
		}
		if a.Launchd == nil {
			return fmt.Errorf("launchd: manager is unavailable")
		}
		var report launchd.StatusReport
		err := withWaitingProgress(stderr, true, a.liveProgressInteractive(stderr), fmt.Sprintf("Checking autostart for %s VM", name), fmt.Sprintf("Loaded autostart status for %s VM", name), func() error {
			var err error
			report, err = a.Launchd.Status(ctx, name)
			return err
		})
		if err != nil {
			return withLaunchdPrefix(err)
		}
		return writeAutostartStatus(stdout, stdoutInteractive, report)
	default:
		panic("unreachable")
	}
}

func withLaunchdPrefix(err error) error {
	if err == nil || strings.HasPrefix(err.Error(), "launchd:") {
		return err
	}
	return fmt.Errorf("launchd: %w", err)
}

func writeAutostartEnableStatus(output io.Writer, interactive bool, name string, result launchd.EnableResult) error {
	switch {
	case result.Reconciled:
		if err := writeMessage(output, interactive, messageSuccess, fmt.Sprintf("Reconciled autostart plist for %q at %s.", name, result.Scope)); err != nil {
			return err
		}
		if err := writeMessage(output, interactive, messageInfo, "The installed launchd job was rewritten to match the current executable."); err != nil {
			return err
		}
	case result.AlreadyEnabled:
		if err := writeMessage(output, interactive, messageInfo, fmt.Sprintf("Autostart already enabled for %q at %s.", name, result.Scope)); err != nil {
			return err
		}
	default:
		if err := writeMessage(output, interactive, messageSuccess, fmt.Sprintf("Autostart enabled for %q at %s.", name, result.Scope)); err != nil {
			return err
		}
	}
	if result.Loaded {
		if err := writeMessage(output, interactive, messageInfo, "VM is already loaded; its current state was not changed."); err != nil {
			return err
		}
		if result.Reconciled {
			if err := writeMessage(output, interactive, messageInfo, "The loaded job picks up the new plist at next boot, or after stop and start."); err != nil {
				return err
			}
		}
	} else if err := writeMessage(output, interactive, messageInfo, "VM state unchanged."); err != nil {
		return err
	}
	if !result.Reconciled {
		if err := writeMessage(output, interactive, messageInfo, fmt.Sprintf("Start when ready: qemu-manage start %s", name)); err != nil {
			return err
		}
	}
	return nil
}

func writeAutostartDisableStatus(output io.Writer, interactive bool, name string) error {
	if err := writeMessage(output, interactive, messageSuccess, fmt.Sprintf("Autostart disabled for %q.", name)); err != nil {
		return err
	}
	if err := writeMessage(output, interactive, messageInfo, "VM state unchanged."); err != nil {
		return err
	}
	return nil
}

func writeAutostartDisableRefusal(output io.Writer, interactive bool, name string, err error) error {
	if writeErr := writeMessage(output, interactive, messageWarning, fmt.Sprintf("Autostart was not changed for %q because the VM is running.", name)); writeErr != nil {
		return writeErr
	}
	if writeErr := writeMessage(output, interactive, messageInfo, fmt.Sprintf("Stop when ready: qemu-manage stop %s", name)); writeErr != nil {
		return writeErr
	}
	return err
}

func writeAutostartStatus(output io.Writer, interactive bool, report launchd.StatusReport) error {
	if err := writeTable(output, interactive, []string{"SETTING", "VALUE"}, [][]string{{"configured_scope", string(report.ConfiguredScope)}}); err != nil {
		return err
	}
	rows := make([][]string, 0, 2)
	for _, domain := range []struct {
		name   string
		status launchd.DomainStatus
	}{
		{name: "login", status: report.Login},
		{name: "boot", status: report.Boot},
	} {
		rows = append(rows, []string{
			domain.name,
			fmt.Sprintf("%t", domain.status.FilePresent),
			fmt.Sprintf("%t", domain.status.FileMatch),
			fmt.Sprintf("%t", domain.status.Loaded),
			fmt.Sprintf("%q", domain.status.Error),
		})
	}
	return writeTable(output, interactive, []string{"DOMAIN", "FILE PRESENT", "FILE MATCH", "LOADED", "ERROR"}, rows)
}
