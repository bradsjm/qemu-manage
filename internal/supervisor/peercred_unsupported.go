//go:build !darwin

package supervisor

import (
	"errors"
	"net"
)

func peerUID(_ *net.UnixConn) (uint32, error) {
	return 0, errors.New("Unix socket peer authentication is supported only on macOS")
}
