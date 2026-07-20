//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openLock opens or creates a lock file, validates that the opened descriptor
// still refers to an owned 0600 regular file, and only then acquires the flock
func openLock(path string, nonblocking bool) (*Lock, bool, error) {
	// Open or create the path without following symlinks so later validation and locking use one descriptor.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	created := err == nil
	if errors.Is(err, syscall.EEXIST) {
		fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, false, err
	}
	if created {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			_ = unix.Close(fd)
			return nil, false, err
		}
	}
	file := os.NewFile(uintptr(fd), path)
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()

	// Validate the opened descriptor after the open step so the locked inode is an owned 0600 regular file.
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, false, errors.New("not a regular file")
	}
	if stat.Uid != uint32(os.Getuid()) {
		return nil, false, fmt.Errorf("owned by uid %d, want %d", stat.Uid, os.Getuid())
	}
	if stat.Mode&0o777 != 0o600 {
		return nil, false, fmt.Errorf("mode is %04o, want 0600", stat.Mode&0o777)
	}

	// Acquire the advisory flock only after the descriptor has passed validation.
	operation := unix.LOCK_EX
	if nonblocking {
		operation |= unix.LOCK_NB
	}
	if err := unix.Flock(fd, operation); err != nil {
		if nonblocking && errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	ok = true
	return &Lock{file: file, path: path}, true, nil
}

// openOwnedRegular opens an existing file without following symlinks and
// validates that the descriptor refers to an owned 0600 regular file
func openOwnedRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	// Validate the opened descriptor before decoding or trusting file contents.
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		return nil, errors.New("not a regular file")
	}
	if stat.Uid != uint32(os.Getuid()) {
		file.Close()
		return nil, fmt.Errorf("owned by uid %d, want %d", stat.Uid, os.Getuid())
	}
	if stat.Mode&0o777 != 0o600 {
		file.Close()
		return nil, fmt.Errorf("mode is %04o, want 0600", stat.Mode&0o777)
	}
	return file, nil
}

// unlockFile releases the advisory flock on an opened lock file
func unlockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
