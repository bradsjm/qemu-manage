package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/bradsjm/qemu-manage/internal/launchd"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/qemu"
)

func (a *App) runAutostart(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_ = stderr
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

	switch subcommand {
	case "enable":
		flags := quietFlagSet("autostart enable")
		scopeValue := flags.String("scope", string(model.AutostartBoot), "")
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
		err := a.Launchd.Enable(ctx, name, scope, func(ctx context.Context, config *model.Config) error {
			paths := a.Store.Paths(config)
			checks := qemu.Doctor(ctx, *config, backendPaths(paths))
			return qemu.RequiredPassed(checks)
		})
		return withLaunchdPrefix(err)
	case "disable":
		if len(remaining) != 0 {
			return usageErrorf("autostart disable: unexpected arguments")
		}
		if a.Launchd == nil {
			return fmt.Errorf("launchd: manager is unavailable")
		}
		return withLaunchdPrefix(a.Launchd.Disable(ctx, name))
	case "status":
		if len(remaining) != 0 {
			return usageErrorf("autostart status: unexpected arguments")
		}
		if a.Launchd == nil {
			return fmt.Errorf("launchd: manager is unavailable")
		}
		report, err := a.Launchd.Status(ctx, name)
		if err != nil {
			return withLaunchdPrefix(err)
		}
		return writeAutostartStatus(stdout, report)
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

func writeAutostartStatus(output io.Writer, report launchd.StatusReport) error {
	if _, err := fmt.Fprintf(output, "configured_scope: %s\n", report.ConfiguredScope); err != nil {
		return err
	}
	for _, domain := range []struct {
		name   string
		status launchd.DomainStatus
	}{
		{name: "login", status: report.Login},
		{name: "boot", status: report.Boot},
	} {
		if _, err := fmt.Fprintf(output, "%s:\n  file_present: %t\n  file_match: %t\n  loaded: %t\n  error: %q\n",
			domain.name, domain.status.FilePresent, domain.status.FileMatch, domain.status.Loaded, domain.status.Error); err != nil {
			return err
		}
	}
	return nil
}
