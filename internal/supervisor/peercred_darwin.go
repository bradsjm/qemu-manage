//go:build darwin

package supervisor

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(conn *net.UnixConn) (uint32, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}

	var credentials *unix.Xucred
	var syscallErr error
	if err := rawConn.Control(func(fd uintptr) {
		credentials, syscallErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if syscallErr != nil {
		return 0, syscallErr
	}
	return credentials.Uid, nil
}
