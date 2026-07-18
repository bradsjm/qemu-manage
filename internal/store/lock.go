package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"qemu-manage/internal/model"
)

var ErrLocked = errors.New("lock is held")

type Lock struct {
	file *os.File
	path string
}

func (l *Lock) Path() string { return l.path }

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unlockFile(file)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

type NameLock struct {
	*Lock
	store *Store
	name  string
}

func (s *Store) LockName(name string) (*NameLock, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := s.ensureRoots(); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(s.DataRoot, locksDirectory)
	if err := ensureOwnedDirectory(lockDir); err != nil {
		return nil, fmt.Errorf("store: lock directory: %w", err)
	}
	lock, _, err := openLock(filepath.Join(lockDir, name+".lock"), false)
	if err != nil {
		return nil, fmt.Errorf("store: name lock %q: %w", name, err)
	}
	return &NameLock{Lock: lock, store: s, name: name}, nil
}

func (l *NameLock) Load() (*model.Config, error) {
	if l == nil || l.Lock == nil || l.file == nil {
		return nil, errors.New("store: name lock is not held")
	}
	return l.store.loadUnlocked(l.name)
}

func (l *NameLock) Save(config *model.Config) error {
	if l == nil || l.Lock == nil || l.file == nil {
		return errors.New("store: name lock is not held")
	}
	if config == nil || config.Name != l.name {
		return errors.New("store: config does not match name lock")
	}
	current, err := l.store.loadUnlocked(l.name)
	if err != nil {
		return err
	}
	if current.ID != config.ID {
		return fmt.Errorf("config: immutable id mismatch for %q", config.Name)
	}
	return writeConfigAtomic(l.store.Paths(config).Config, config)
}

func (l *NameLock) LockLifetime(config *model.Config) (*Lock, error) {
	if l == nil || l.Lock == nil || l.file == nil {
		return nil, errors.New("store: name lock is not held")
	}
	if config == nil || config.Name != l.name {
		return nil, errors.New("store: lifetime lock config does not match name lock")
	}
	return l.store.lockLifetime(config.ID, false)
}

func (l *NameLock) TryLockLifetime(config *model.Config) (*Lock, bool, error) {
	if l == nil || l.Lock == nil || l.file == nil {
		return nil, false, errors.New("store: name lock is not held")
	}
	if config == nil || config.Name != l.name {
		return nil, false, errors.New("store: lifetime lock config does not match name lock")
	}
	lock, acquired, err := l.store.tryLifetime(config.ID)
	return lock, acquired, err
}

func (s *Store) TryLifetime(id string) (*Lock, bool, error) {
	return s.tryLifetime(id)
}

func (s *Store) tryLifetime(id string) (*Lock, bool, error) {
	if err := validateIDForPath(id); err != nil {
		return nil, false, err
	}
	if err := s.ensureRoots(); err != nil {
		return nil, false, err
	}
	dir := s.RuntimeDir(id)
	if err := ensureOwnedDirectory(dir); err != nil {
		return nil, false, fmt.Errorf("store: runtime directory: %w", err)
	}
	return openLock(filepath.Join(dir, "lifetime.lock"), true)
}

func (s *Store) lockLifetime(id string, nonblocking bool) (*Lock, error) {
	if err := validateIDForPath(id); err != nil {
		return nil, err
	}
	if err := s.ensureRoots(); err != nil {
		return nil, err
	}
	dir := s.RuntimeDir(id)
	if err := ensureOwnedDirectory(dir); err != nil {
		return nil, fmt.Errorf("store: runtime directory: %w", err)
	}
	lock, acquired, err := openLock(filepath.Join(dir, "lifetime.lock"), nonblocking)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, ErrLocked
	}
	return lock, nil
}
