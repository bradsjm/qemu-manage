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

func ownsLifecycleSignals(args []string) bool {
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
