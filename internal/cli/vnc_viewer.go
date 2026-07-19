package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

type vncCommandRunner func(context.Context, string, []string, io.Reader) error

func openVNCWithRunner(ctx context.Context, endpoint backend.VNCEndpoint, password string, run vncCommandRunner) error {
	if run == nil {
		return errors.New("vnc: viewer is unavailable")
	}
	if err := run(ctx, "/usr/bin/pbcopy", nil, strings.NewReader(password)); err != nil {
		return err
	}
	url := "vnc://" + net.JoinHostPort(endpoint.Host, strconv.Itoa(int(endpoint.Port)))
	return run(ctx, "/usr/bin/open", []string{url}, nil)
}
