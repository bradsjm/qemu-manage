//go:build unix

package supervisor

import (
	"fmt"
	"os"
	"syscall"
)

// setUmaskPrivate forces supervisor-created files to default to owner-only access
func setUmaskPrivate() func() {
	old := syscall.Umask(0o077)
	return func() { syscall.Umask(old) }
}

// closeOnExecFile is the *os.File helper for closeOnExec
func closeOnExecFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return closeOnExec(file)
}

// closeOnExec explicitly protects every supervisor-owned descriptor that is
// alive when the backend is started. Go normally creates sockets with
// close-on-exec already set; doing this at the ownership boundary makes that
// requirement independent of the constructor and prevents fd 3 from being
// occupied in a socket_vmnet child.
func closeOnExec(connection syscall.Conn) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return err
	}
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		if fd <= 2 {
			return
		}
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, syscall.FD_CLOEXEC)
		if errno != 0 {
			controlErr = errno
		}
	}); err != nil {
		return err
	}
	if controlErr != nil {
		return fmt.Errorf("set close-on-exec: %w", controlErr)
	}
	return nil
}
