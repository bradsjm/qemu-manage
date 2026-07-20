//go:build darwin || linux

package supervisor

import (
	"errors"
	"os"
	"syscall"
)

// detachedProcessAttributes starts the supervisor in a new session so it owns
// its own process group after re-exec.
func detachedProcessAttributes() (*syscall.SysProcAttr, error) {
	return &syscall.SysProcAttr{Setsid: true}, nil
}

// terminateSupervisor requests cooperative shutdown of a detached supervisor
func terminateSupervisor(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}

// cooperativeTerminationProcessDone treats already-exited processes as success
func cooperativeTerminationProcessDone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}
