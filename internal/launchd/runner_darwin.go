//go:build darwin

package launchd

import (
	"context"
	"os/exec"
)

type platformRunner struct{}

func newPlatformRunner() Runner { return platformRunner{} }

func (platformRunner) Run(ctx context.Context, privileged bool, path string, args ...string) ([]byte, error) {
	if privileged {
		sudoArgs := make([]string, 0, len(args)+1)
		sudoArgs = append(sudoArgs, path)
		sudoArgs = append(sudoArgs, args...)
		return exec.CommandContext(ctx, "/usr/bin/sudo", sudoArgs...).CombinedOutput()
	}
	return exec.CommandContext(ctx, path, args...).CombinedOutput()
}
