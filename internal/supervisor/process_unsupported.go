//go:build !darwin && !linux

package supervisor

import (
	"errors"
	"os"
	"syscall"
)

func detachedProcessAttributes() (*syscall.SysProcAttr, error) {
	return nil, errors.New("supervisor: detached processes are unsupported on this platform")
}

func terminateSupervisor(_ *os.Process) error {
	return errors.New("supervisor: cooperative process termination is unsupported on this platform")
}

func cooperativeTerminationProcessDone(err error) bool {
	return errors.Is(err, os.ErrProcessDone)
}
