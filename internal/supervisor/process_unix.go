//go:build darwin || linux

package supervisor

import (
	"errors"
	"os"
	"syscall"
)

func detachedProcessAttributes() (*syscall.SysProcAttr, error) {
	return &syscall.SysProcAttr{Setsid: true}, nil
}

func terminateSupervisor(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}

func cooperativeTerminationProcessDone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}
