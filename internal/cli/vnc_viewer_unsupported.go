//go:build !darwin

package cli

import (
	"context"
	"errors"

	"qemu-manage/internal/backend"
)

func openVNC(_ context.Context, _ backend.VNCEndpoint, _ string) error {
	return errors.New("vnc: Screen Sharing is supported only on macOS")
}
