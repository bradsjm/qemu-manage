package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"qemu-manage/internal/model"
)

const (
	locksDirectory = ".locks"
	configFilename = "config.json"
)

type Store struct {
	DataRoot    string
	RuntimeRoot string
	LogRoot     string
}

type Paths struct {
	VMDir            string
	Config           string
	RuntimeDir       string
	ControlSocket    string
	LifetimeLock     string
	QMP              string
	QGA              string
	Console          string
	RuntimeMetadata  string
	LastExitMetadata string
	SupervisorStdout string
	SupervisorStderr string
	QEMULog          string
	SerialLog        string
}

type CreateArtifacts func(config *model.Config, paths Paths) error

type DeleteInspector func(config *model.Config, paths Paths) error

func Default() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("store: determine home directory: %w", err)
	}
	return New(
		filepath.Join(home, "Library", "Application Support", "qemu-manage", "vms"),
		filepath.Join(os.TempDir(), fmt.Sprintf("qemu-manage-%d", os.Getuid())),
		filepath.Join(home, "Library", "Logs", "qemu-manage"),
	)
}

func New(dataRoot, runtimeRoot, logRoot string) (*Store, error) {
	store := &Store{DataRoot: dataRoot, RuntimeRoot: runtimeRoot, LogRoot: logRoot}
	if err := store.ensureRoots(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Paths(config *model.Config) Paths {
	vmDir := filepath.Join(s.DataRoot, config.Name)
	runtimeDir := s.RuntimeDir(config.ID)
	logDir := filepath.Join(s.LogRoot, config.Name)
	return Paths{
		VMDir:            vmDir,
		Config:           filepath.Join(vmDir, configFilename),
		RuntimeDir:       runtimeDir,
		ControlSocket:    filepath.Join(runtimeDir, "control.sock"),
		LifetimeLock:     filepath.Join(runtimeDir, "lifetime.lock"),
		QMP:              filepath.Join(runtimeDir, "qmp.sock"),
		QGA:              filepath.Join(runtimeDir, "qga.sock"),
		Console:          filepath.Join(runtimeDir, "console.sock"),
		RuntimeMetadata:  filepath.Join(runtimeDir, "runtime.json"),
		LastExitMetadata: filepath.Join(runtimeDir, "last_exit.json"),
		SupervisorStdout: filepath.Join(logDir, "supervisor.stdout.log"),
		SupervisorStderr: filepath.Join(logDir, "supervisor.stderr.log"),
		QEMULog:          filepath.Join(logDir, "qemu.log"),
		SerialLog:        filepath.Join(logDir, "serial.log"),
	}
}

func (s *Store) RuntimeDir(id string) string {
	prefix := id
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return filepath.Join(s.RuntimeRoot, prefix)
}

func (s *Store) Create(config *model.Config, createArtifacts CreateArtifacts) error {
	if config == nil {
		return errors.New("store: nil config")
	}
	if err := config.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	nameLock, err := s.LockName(config.Name)
	if err != nil {
		return err
	}
	defer nameLock.Close()

	paths := s.Paths(config)
	if _, err := os.Lstat(paths.VMDir); err == nil {
		return fmt.Errorf("store: VM %q already exists", config.Name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: inspect VM %q: %w", config.Name, err)
	}

	candidates := []string{paths.VMDir, filepath.Dir(paths.QEMULog), paths.RuntimeDir}
	created := make([]string, 0, len(candidates))
	for _, dir := range candidates {
		if err := createOwnedDirectory(dir); err != nil {
			rollbackDirectories(created)
			return fmt.Errorf("store: create VM %q: %w", config.Name, err)
		}
		created = append(created, dir)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackDirectories(created)
		}
	}()
	if createArtifacts != nil {
		if err := createArtifacts(config, paths); err != nil {
			return fmt.Errorf("store: create artifacts: %w", err)
		}
	}
	if err := writeConfigAtomic(paths.Config, config); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) Load(name string) (*model.Config, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	return s.loadUnlocked(name)
}

func (s *Store) loadUnlocked(name string) (*model.Config, error) {
	vmDir := filepath.Join(s.DataRoot, name)
	if err := inspectOwnedDirectory(vmDir); err != nil {
		return nil, fmt.Errorf("config: VM directory %q: %w", name, err)
	}
	path := filepath.Join(vmDir, configFilename)
	file, err := openOwnedRegular(path)
	if err != nil {
		return nil, fmt.Errorf("config: load %q: %w", name, err)
	}
	defer file.Close()
	config, err := model.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("config: load %q: %w", name, err)
	}
	if config.Name != name {
		return nil, fmt.Errorf("config: name %q does not match directory %q", config.Name, name)
	}
	return config, nil
}

func (s *Store) Save(config *model.Config) error {
	if config == nil {
		return errors.New("store: nil config")
	}
	if err := config.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	nameLock, err := s.LockName(config.Name)
	if err != nil {
		return err
	}
	defer nameLock.Close()
	return nameLock.Save(config)
}

func (s *Store) List() ([]*model.Config, error) {
	entries, err := os.ReadDir(s.DataRoot)
	if err != nil {
		return nil, fmt.Errorf("store: list VMs: %w", err)
	}
	configs := make([]*model.Config, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == locksDirectory {
			continue
		}
		if !entry.IsDir() {
			return nil, fmt.Errorf("store: unexpected entry %q in data root", entry.Name())
		}
		config, err := s.Load(entry.Name())
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })
	return configs, nil
}

func (s *Store) Delete(name string, inspect DeleteInspector) error {
	if inspect == nil {
		return errors.New("store: delete requires autostart and running inspection")
	}
	nameLock, err := s.LockName(name)
	if err != nil {
		return err
	}
	defer nameLock.Close()
	config, err := s.loadUnlocked(name)
	if err != nil {
		return err
	}
	if config.Autostart.Scope != model.AutostartNone {
		return fmt.Errorf("store: VM %q has autostart scope %q", name, config.Autostart.Scope)
	}
	lifetimeLock, acquired, err := nameLock.TryLockLifetime(config)
	if err != nil {
		return fmt.Errorf("runtime: lifetime lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("runtime: VM %q is running", name)
	}
	defer lifetimeLock.Close()
	paths := s.Paths(config)
	if err := inspect(config, paths); err != nil {
		return err
	}
	if err := os.RemoveAll(paths.VMDir); err != nil {
		return fmt.Errorf("store: remove VM %q: %w", name, err)
	}
	if err := os.RemoveAll(filepath.Dir(paths.QEMULog)); err != nil {
		return fmt.Errorf("store: remove logs for %q: %w", name, err)
	}
	if err := os.RemoveAll(paths.RuntimeDir); err != nil {
		return fmt.Errorf("store: remove runtime for %q: %w", name, err)
	}
	return nil
}

func (s *Store) ensureRoots() error {
	roots := []struct {
		description string
		path        string
	}{
		{description: "data", path: s.DataRoot},
		{description: "runtime", path: s.RuntimeRoot},
		{description: "log", path: s.LogRoot},
	}
	for _, root := range roots {
		if root.path == "" || !filepath.IsAbs(root.path) {
			return fmt.Errorf("store: %s root must be absolute", root.description)
		}
		if err := ensureOwnedDirectory(root.path); err != nil {
			return fmt.Errorf("store: %s root: %w", root.description, err)
		}
	}
	return nil
}

func ensureOwnedDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return inspectOwnedDirectory(path)
}

func inspectOwnedDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("not a non-symlink directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine directory owner")
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("owned by uid %d, want %d", stat.Uid, os.Getuid())
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("mode is %04o, want 0700", info.Mode().Perm())
	}
	return nil
}
func createOwnedDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	return ensureOwnedDirectory(path)
}

func writeConfigAtomic(path string, config *model.Config) error {
	if err := config.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	data, err := model.CanonicalJSON(config)
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	dir := filepath.Dir(path)
	if err := ensureOwnedDirectory(dir); err != nil {
		return fmt.Errorf("config: directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("config: set temporary file mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("config: write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("config: sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("config: close temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("config: install: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("config: open directory for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("config: sync directory: %w", err)
	}
	return nil
}

func rollbackDirectories(paths []string) {
	for index := len(paths) - 1; index >= 0; index-- {
		_ = os.RemoveAll(paths[index])
	}
}

func validateName(name string) error {
	if name == "" || len(name) > 63 {
		return errors.New("store: invalid VM name")
	}
	for index, character := range name {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-'
		if !valid || index == 0 && (character == '.' || character == '_' || character == '-') {
			return errors.New("store: invalid VM name")
		}
	}
	return nil
}

func validateIDForPath(id string) error {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return errors.New("store: invalid VM id")
	}
	return nil
}
