//go:build darwin

package cli

import (
	"context"
	"io"
	"os/exec"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

func openVNC(ctx context.Context, endpoint backend.VNCEndpoint, password string) error {
	return openVNCWithRunner(ctx, endpoint, password, func(ctx context.Context, path string, args []string, stdin io.Reader) error {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdin = stdin
		return cmd.Run()
	})
}
