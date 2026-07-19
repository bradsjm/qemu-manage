//go:build !darwin

package cli

import (
	"context"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

func TestOpenVNCUnsupportedPlatform(t *testing.T) {
	err := openVNC(context.Background(), backend.VNCEndpoint{Host: "127.0.0.1", Port: 5901}, "secret")
	if err == nil || err.Error() != "vnc: Screen Sharing is supported only on macOS" {
		t.Fatalf("error = %v, want exact unsupported-platform message", err)
	}
}
