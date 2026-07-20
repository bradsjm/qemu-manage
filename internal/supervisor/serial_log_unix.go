//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func openSerialLogPipe(path string) (io.ReadCloser, io.Closer, error) {
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, nil, fmt.Errorf("serial log: create FIFO: %w", err)
	}
	readFD, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("serial log: open FIFO reader: %w", err)
	}
	reader := os.NewFile(uintptr(readFD), path)
	dummyFD, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = reader.Close()
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("serial log: open FIFO writer: %w", err)
	}
	dummy := os.NewFile(uintptr(dummyFD), path)
	if err := unix.SetNonblock(readFD, false); err != nil {
		_ = dummy.Close()
		_ = reader.Close()
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("serial log: configure FIFO reader: %w", err)
	}
	return reader, dummy, nil
}

func inspectSerialLogDirectory(path string) error {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("serial log: inspect directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("serial log: parent is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("serial log: directory is not owned by current user")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("serial log: directory mode is %04o, want 0700", info.Mode().Perm())
	}
	return nil
}

func inspectSerialLogFile(path string) (serialLogFileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return serialLogFileInfo{}, nil
	}
	if err != nil {
		return serialLogFileInfo{}, fmt.Errorf("serial log: inspect %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return serialLogFileInfo{}, fmt.Errorf("serial log: %q is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return serialLogFileInfo{}, fmt.Errorf("serial log: %q is not owned by current user", path)
	}
	if info.Mode().Perm() != 0o600 {
		return serialLogFileInfo{}, fmt.Errorf("serial log: %q mode is %04o, want 0600", path, info.Mode().Perm())
	}
	return serialLogFileInfo{exists: true, size: info.Size()}, nil
}

func openSecureSerialLog(path string) (*os.File, int64, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("serial log: open active log: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		return nil, 0, errors.Join(fmt.Errorf("serial log: stat active log: %w", err), file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.Join(errors.New("serial log: active log is not a regular file"), file.Close())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, 0, errors.Join(errors.New("serial log: active log is not owned by current user"), file.Close())
	}
	if info.Mode().Perm() != 0o600 {
		return nil, 0, errors.Join(fmt.Errorf("serial log: active log mode is %04o, want 0600", info.Mode().Perm()), file.Close())
	}
	return file, info.Size(), nil
}
