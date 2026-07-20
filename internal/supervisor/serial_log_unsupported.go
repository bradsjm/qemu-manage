//go:build !unix

package supervisor

import (
	"errors"
	"io"
	"os"
)

var errSerialLogUnsupported = errors.New("serial log: FIFO transport is unsupported on this platform")

func openSerialLogPipe(string) (io.ReadCloser, io.Closer, error) {
	return nil, nil, errSerialLogUnsupported
}

func inspectSerialLogDirectory(string) error {
	return errSerialLogUnsupported
}

func inspectSerialLogFile(string) (serialLogFileInfo, error) {
	return serialLogFileInfo{}, errSerialLogUnsupported
}

func openSecureSerialLog(string) (*os.File, int64, error) {
	return nil, 0, errSerialLogUnsupported
}
