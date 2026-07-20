package backend

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/bradsjm/qemu-manage/internal/model"
)

// RuntimePaths contains the durable VM directory and the ephemeral or log paths
// owned by a backend process.
type RuntimePaths struct {
	VMDir         string
	QMP           string
	QMPCommand    string
	QGA           string
	Console       string
	Monitor       string
	QEMULog       string
	SerialLogPipe string
	VNCSecret     string
}

// ResolvePath resolves a configured path relative to the VM directory. Absolute
// paths are returned after filepath cleaning.
func ResolvePath(vmDir, configuredPath string) string {
	if filepath.IsAbs(configuredPath) {
		return filepath.Clean(configuredPath)
	}
	return filepath.Join(vmDir, configuredPath)
}

// Command is an executable and its argument vector. Args excludes Path.
type Command struct {
	Path string
	Args []string
}

type RenderOptions struct {
	BootMenu bool
}

// VNCEndpoint is the live VNC listener selected by the backend.
type VNCEndpoint struct {
	Host string `json:"host"`
	Port uint16 `json:"port"`
}

// Exit is the single terminal result published by an Instance. Implementations
// must send exactly one Exit on a channel buffered to one element and then close
// that channel.
type Exit struct {
	Code int
	Err  error
}

// Instance is a started backend process. ForceStop is the sole termination
// operation: it must not return until the exact child process has been reaped.
type Instance interface {
	PID() int
	Status(context.Context) (model.RunState, error)
	VNCEndpoint() (VNCEndpoint, bool)
	RequestShutdown(context.Context) error
	ForceStop(context.Context) error
	Wait() <-chan Exit
}

// Backend renders and starts one backend implementation. Start executes the
// supplied rendered command rather than rendering a second time.
type Backend interface {
	Render(*model.Config, RuntimePaths, RenderOptions) (Command, error)
	Start(context.Context, *model.Config, RuntimePaths, Command) (Instance, error)
}

// Factory constructs a backend. It permits registration without importing a
// concrete backend package and therefore keeps the registry free of cycles.
type Factory func() (Backend, error)

// Registry maps durable backend names to constructors.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register adds a named factory. Empty names, nil factories, and duplicate
// names are rejected deterministically.
func (r *Registry) Register(name string, factory Factory) error {
	if name == "" {
		return fmt.Errorf("backend name is empty")
	}
	if factory == nil {
		return fmt.Errorf("backend %q has a nil factory", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]Factory)
	}
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("backend %q is already registered", name)
	}
	r.factories[name] = factory
	return nil
}

// RegisterInstance registers an already-created backend.
func (r *Registry) RegisterInstance(name string, instance Backend) error {
	if instance == nil {
		return fmt.Errorf("backend %q has a nil instance", name)
	}
	return r.Register(name, func() (Backend, error) { return instance, nil })
}

// Lookup constructs the named backend.
func (r *Registry) Lookup(name string) (Backend, error) {
	r.mu.RLock()
	factory, exists := r.factories[name]
	r.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("backend %q is unavailable in this build", name)
	}

	instance, err := factory()
	if err != nil {
		return nil, fmt.Errorf("backend %q: %w", name, err)
	}
	if instance == nil {
		return nil, fmt.Errorf("backend %q factory returned nil", name)
	}
	return instance, nil
}
