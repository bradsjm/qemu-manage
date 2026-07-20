// Command qemu-manage is the user-facing entry point for VM lifecycle
// operations.
//
// It owns process-level signal handling for normal CLI invocations and leaves
// foreground supervisor commands to install their own lifecycle policy.
package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/bradsjm/qemu-manage/internal/cli"
)

func main() { os.Exit(run()) }

func run() int {
	ctx := context.Background()
	stop := func() {}
	if !ownsLifecycleSignals(os.Args[1:]) {
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	defer stop()

	app := cli.NewApp()
	return app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// ownsLifecycleSignals reports whether the selected invocation installs and manages
// its own signal policy, so the top-level CLI should not add the default
// interrupt and terminate handlers on top.
func ownsLifecycleSignals(args []string) bool {
	args = stripLeadingDebugFlags(args)
	if len(args) == 0 {
		return false
	}
	if args[0] == "supervise" {
		return true
	}
	if args[0] != "start" {
		return false
	}
	foreground := false
	for _, arg := range args[1:] {
		for _, name := range []string{"-foreground", "--foreground"} {
			if arg == name {
				foreground = true
				break
			}
			if value, ok := strings.CutPrefix(arg, name+"="); ok {
				enabled, err := strconv.ParseBool(value)
				if err != nil {
					return false
				}
				foreground = enabled
				break
			}
		}
	}
	return foreground
}

// stripLeadingDebugFlags removes global debug flags before helper detection looks
// at the real subcommand.
func stripLeadingDebugFlags(args []string) []string {
	for len(args) > 0 {
		switch args[0] {
		case "-d", "--debug":
			args = args[1:]
		default:
			return args
		}
	}
	return args
}
