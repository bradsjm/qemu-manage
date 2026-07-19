package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bradsjm/qemu-manage/internal/store"
)

const (
	metadataVersion = 1
	maxMetadataSize = 64 << 10
)

type RuntimeMetadata struct {
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	SupervisorPID int       `json:"supervisor_pid"`
	StartedAt     time.Time `json:"started_at"`
}

type LastExitMetadata struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	ExitCode  int       `json:"exit_code"`
	Error     string    `json:"error,omitempty"`
}

func WriteRuntimeMetadata(path string, metadata RuntimeMetadata) error {
	if err := metadata.validate(); err != nil {
		return fmt.Errorf("runtime: invalid runtime metadata: %w", err)
	}
	return writeMetadata(path, metadata)
}

func ReadRuntimeMetadata(path string) (RuntimeMetadata, error) {
	var metadata RuntimeMetadata
	if err := readMetadata(path, &metadata); err != nil {
		return metadata, fmt.Errorf("runtime: read runtime metadata: %w", err)
	}
	if err := metadata.validate(); err != nil {
		return metadata, fmt.Errorf("runtime: invalid runtime metadata: %w", err)
	}
	return metadata, nil
}

func WriteLastExit(path string, metadata LastExitMetadata) error {
	if err := metadata.validate(); err != nil {
		return fmt.Errorf("runtime: invalid last-exit metadata: %w", err)
	}
	return writeMetadata(path, metadata)
}

func ReadLastExit(path string) (LastExitMetadata, error) {
	var metadata LastExitMetadata
	if err := readMetadata(path, &metadata); err != nil {
		return metadata, fmt.Errorf("runtime: read last-exit metadata: %w", err)
	}
	if err := metadata.validate(); err != nil {
		return metadata, fmt.Errorf("runtime: invalid last-exit metadata: %w", err)
	}
	return metadata, nil
}

// ClearFailedLastExit removes stale failure evidence only after successful backend readiness.
func ClearFailedLastExit(path string) error {
	metadata, err := ReadLastExit(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if metadata.ExitCode == 0 {
		return nil
	}
	if err := inspectRuntimeDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("runtime: last-exit directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("runtime: remove stale last-exit metadata: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

// CleanupRuntime removes only ephemeral entries. It deliberately preserves last_exit.json
// and lifetime.lock as durable failure evidence and the lifetime serialization point.
func CleanupRuntime(paths store.Paths) error {
	if err := inspectRuntimeDirectory(paths.RuntimeDir); err != nil {
		return fmt.Errorf("runtime: cleanup directory: %w", err)
	}
	var cleanupErr error
	for _, path := range []string{
		paths.ControlSocket, paths.QMP, paths.QMPCommand, paths.QGA, paths.Console, paths.Monitor, paths.VNCSecret, paths.RuntimeMetadata,
	} {
		if filepath.Dir(path) != paths.RuntimeDir {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("runtime: refusing cleanup outside runtime directory: %q", path))
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("runtime: remove %q: %w", filepath.Base(path), err))
		}
	}
	if err := syncDirectory(paths.RuntimeDir); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func (metadata RuntimeMetadata) validate() error {
	if metadata.Version != metadataVersion {
		return fmt.Errorf("version is %d, want %d", metadata.Version, metadataVersion)
	}
	if err := validateMetadataID(metadata.ID); err != nil {
		return err
	}
	if err := validateMetadataName(metadata.Name); err != nil {
		return err
	}
	if metadata.SupervisorPID <= 0 {
		return errors.New("supervisor_pid must be positive")
	}
	return validateUTCTimestamp("started_at", metadata.StartedAt)
}

func (metadata LastExitMetadata) validate() error {
	if metadata.Version != metadataVersion {
		return fmt.Errorf("version is %d, want %d", metadata.Version, metadataVersion)
	}
	if err := validateMetadataID(metadata.ID); err != nil {
		return err
	}
	if err := validateUTCTimestamp("timestamp", metadata.Timestamp); err != nil {
		return err
	}
	if metadata.ExitCode < 0 || metadata.ExitCode > 255 {
		return errors.New("exit_code must be between 0 and 255")
	}
	if metadata.ExitCode == 0 && metadata.Error != "" {
		return errors.New("error must be empty for a zero exit_code")
	}
	if metadata.ExitCode != 0 && metadata.Error == "" {
		return errors.New("error is required for a nonzero exit_code")
	}
	return nil
}

func validateMetadataID(id string) error {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return errors.New("id must be exactly 32 lowercase hexadecimal characters")
	}
	return nil
}

func validateMetadataName(name string) error {
	if name == "" || len(name) > 63 {
		return errors.New("invalid name")
	}
	for index, character := range name {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-'
		if !valid || index == 0 && (character == '.' || character == '_' || character == '-') {
			return errors.New("invalid name")
		}
	}
	return nil
}

func validateUTCTimestamp(field string, timestamp time.Time) error {
	if timestamp.IsZero() {
		return fmt.Errorf("%s must be nonzero", field)
	}
	if timestamp.Location() != time.UTC {
		return fmt.Errorf("%s must use UTC", field)
	}
	return nil
}

func readMetadata(path string, destination interface{}) error {
	if err := inspectRuntimeDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd)
		return errors.New("create metadata file handle")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := inspectOwnedMetadataFile(info); err != nil {
		return err
	}
	if info.Size() > maxMetadataSize {
		return fmt.Errorf("metadata exceeds %d bytes", maxMetadataSize)
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxMetadataSize+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON data")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}

func writeMetadata(path string, value interface{}) error {
	directoryPath := filepath.Dir(path)
	if err := inspectRuntimeDirectory(directoryPath); err != nil {
		return fmt.Errorf("runtime: metadata directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("runtime: metadata destination is a symlink")
		}
		if err := inspectOwnedMetadataFile(info); err != nil {
			return fmt.Errorf("runtime: metadata destination: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("runtime: inspect metadata destination: %w", err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("runtime: encode metadata: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxMetadataSize {
		return fmt.Errorf("runtime: metadata exceeds %d bytes", maxMetadataSize)
	}
	temporary, err := os.CreateTemp(directoryPath, ".metadata-*.tmp")
	if err != nil {
		return fmt.Errorf("runtime: create metadata temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("runtime: set metadata temporary mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("runtime: write metadata temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("runtime: sync metadata temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("runtime: close metadata temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("runtime: install metadata: %w", err)
	}
	return syncDirectory(directoryPath)
}

func inspectRuntimeDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("not a non-symlink directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine runtime directory owner")
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("runtime directory owned by uid %d, want %d", stat.Uid, os.Getuid())
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("runtime directory mode is %04o, want 0700", info.Mode().Perm())
	}
	return nil
}

func inspectOwnedMetadataFile(info fs.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("metadata is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine metadata owner")
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("metadata owned by uid %d, want %d", stat.Uid, os.Getuid())
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("metadata mode is %04o, want 0600", info.Mode().Perm())
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("runtime: open directory for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("runtime: sync directory: %w", err)
	}
	return nil
}
