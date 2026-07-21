// Package cli parses qemu-manage command lines and wires them to the store,
// lifecycle, supervisor, and backend collaborators that implement each
// subcommand.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/term"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/launchd"
	"github.com/bradsjm/qemu-manage/internal/lifecycle"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/qemu"
	"github.com/bradsjm/qemu-manage/internal/store"
	"github.com/bradsjm/qemu-manage/internal/supervisor"
	"github.com/pterm/pterm"
)

// PlatformUser captures the invoking platform account used for ownership-aware
// defaults and host service integration.
type PlatformUser struct {
	Name string
	Home string
	UID  int
}

// StatusRow summarizes the observed runtime state for one VM.
type StatusRow struct {
	Name                string               `json:"name"`
	State               model.RunState       `json:"state"`
	RestartRequired     bool                 `json:"restart_required"`
	PID                 *int                 `json:"pid,omitempty"`
	Backend             string               `json:"backend,omitempty"`
	CurrentConfigSHA256 string               `json:"current_config_sha256,omitempty"`
	RunningConfigSHA256 string               `json:"running_config_sha256,omitempty"`
	VNC                 *backend.VNCEndpoint `json:"vnc,omitempty"`
	Error               string               `json:"error,omitempty"`
	StartedAt           *time.Time           `json:"started_at,omitempty"`
	CPUs                int                  `json:"cpus,omitempty"`
	MemoryMiB           int                  `json:"memory_mib,omitempty"`
	Network             model.NetworkMode    `json:"network,omitempty"`
	Autostart           model.AutostartScope `json:"autostart,omitempty"`
}

// RuntimeService reads live VM state without mutating it.
type RuntimeService interface {
	Status(context.Context, *model.Config) (StatusRow, error)
	DeleteAllowed(context.Context, *model.Config) (bool, error)
}

// MonitorClient is one human-oriented QMP session used by interactive CLI
// commands.
type MonitorClient interface {
	HumanMonitorCommand(context.Context, string) (string, error)
	Close() error
}

// App serves one qemu-manage invocation.
//
// Its exported fields are pre-Run injection seams for platform wiring and
// tests; callers must finish configuring them before invoking Run. App is not
// safe for concurrent Run calls because each invocation mutates shared debug
// state that subcommands and delegated helpers read.
type App struct {
	Store                      *store.Store
	Backends                   *backend.Registry
	Lifecycle                  *lifecycle.Service
	Supervisor                 *supervisor.Service
	Launchd                    *launchd.Manager
	Geteuid                    func() int
	ExecutablePath             string
	User                       PlatformUser
	RunExternal                func(context.Context, string, []string) error
	HTTPClient                 *http.Client
	Runtime                    RuntimeService
	IsTerminalOutput           func(io.Writer) bool
	IsTerminal                 func(io.Reader) bool
	LookupEnv                  func(string) (string, bool)
	DiscoverFirmware           func() (string, string)
	DiscoverMachine            func(context.Context, string) (string, error)
	RequireSMBD                func() error
	DiscoverSocketVMNet        func() *model.SocketVMNetConfig
	ProvisionSocketVMNetBridge func(context.Context, string, string) (*model.SocketVMNetConfig, error)
	OpenVNC                    func(context.Context, backend.VNCEndpoint, string) error
	DialQMP                    func(context.Context, string) (MonitorClient, error)
	CallGuestAgent             func(context.Context, string, qemu.GuestAgentRequest) (json.RawMessage, error)
	Confirm                    func(string) (bool, error)

	initializationError error
	debug               bool
	debugWriter         io.Writer
	debugLogger         *pterm.Logger
}

// NewApp constructs an App wired to the host's default collaborators.
func NewApp() *App {
	if os.Geteuid() == 0 {
		return &App{
			Geteuid:          os.Geteuid,
			IsTerminalOutput: terminalWriter,
			LookupEnv:        os.LookupEnv,
			DiscoverMachine:  qemu.DiscoverVersionedMachine,
			Confirm: func(prompt string) (bool, error) {
				return pterm.DefaultInteractiveConfirm.WithDefaultValue(false).Show(prompt)
			},
		}
	}

	a := &App{
		IsTerminalOutput:    terminalWriter,
		Backends:            backend.NewRegistry(),
		Geteuid:             os.Geteuid,
		IsTerminal:          terminalReader,
		LookupEnv:           os.LookupEnv,
		DiscoverFirmware:    qemu.DiscoverFirmware,
		DiscoverMachine:     qemu.DiscoverVersionedMachine,
		DiscoverSocketVMNet: qemu.DiscoverSocketVMNet,
		RequireSMBD:         requireSMBDDefault,
		HTTPClient:          newImageHTTPClient(),
		RunExternal: func(ctx context.Context, path string, args []string) error {
			return exec.CommandContext(ctx, path, args...).Run()
		},
		OpenVNC: openVNC,
		DialQMP: func(ctx context.Context, path string) (MonitorClient, error) {
			return qemu.NewQMPClientContext(ctx, path)
		},
		CallGuestAgent: qemu.GuestAgentCommand,
		Confirm: func(prompt string) (bool, error) {
			return pterm.DefaultInteractiveConfirm.WithDefaultValue(false).Show(prompt)
		},
	}

	var storeErr error
	a.Store, storeErr = store.Default()

	path, executableErr := os.Executable()
	if executableErr == nil {
		a.ExecutablePath = path
	}

	var uidErr error
	current, currentUserErr := user.Current()
	if currentUserErr == nil {
		a.User.Name = current.Username
		a.User.Home = current.HomeDir
		a.User.UID, uidErr = strconv.Atoi(current.Uid)
	}

	registerErr := a.Backends.RegisterInstance(string(model.BackendQEMU), qemu.NewBackend())
	a.initializationError = errors.Join(storeErr, executableErr, currentUserErr, uidErr, registerErr)

	a.Lifecycle = lifecycle.NewService(a.Store)
	a.Supervisor = supervisor.NewService(a.Store, a.Backends)
	a.Supervisor.BuildVersion = currentBuildInfo().version
	a.Supervisor.Preflight = func(ctx context.Context, config *model.Config, paths store.Paths) error {
		if _, err := a.Backends.Lookup(string(config.Backend)); err != nil {
			return err
		}
		return qemu.RequiredPassed(qemu.Doctor(ctx, *config, backendPaths(paths)))
	}
	a.Launchd = launchd.NewManager(a.Store, a.ExecutablePath, a.User.Name, a.User.Home, a.User.UID)
	a.Launchd.Stopped = func(ctx context.Context, cfg *model.Config) error {
		// Treat the VM as running only when the authenticated lifecycle state
		// proves it. A boot-scope LaunchDaemon stays loaded for the whole boot
		// session even while the VM is stopped, so launchd registration alone
		// is not a safe signal for autostart disable.
		result, err := a.Lifecycle.Status(ctx, cfg)
		if err != nil {
			return fmt.Errorf("determine run state: %w", err)
		}
		switch result.State {
		case model.RunStateStarting, model.RunStateRunning, model.RunStatePaused, model.RunStateStopping:
			return fmt.Errorf("VM is %s", result.State)
		}
		return nil
	}
	a.ProvisionSocketVMNetBridge = a.Launchd.ProvisionSocketVMNetBridge
	a.Runtime = newRuntimeAdapter(a.Lifecycle)
	return a
}

type usageError struct {
	message string
}
type silentError struct {
	err error
}

func (e *silentError) Error() string { return e.err.Error() }
func (e *silentError) Unwrap() error { return e.err }

func (e *usageError) Error() string { return e.message }

func usageErrorf(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

// Run executes one CLI invocation and returns its process exit code.
func (a *App) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a.debug = false
	a.debugWriter = nil
	a.debugLogger = nil
	debugEnabled, args := parseLeadingDebugFlags(args)
	stderrInteractive := a.progressInteractive(stderr)
	stderr = newPresentationWriter(stderr, stderrInteractive)
	a.debug = debugEnabled
	a.debugWriter = ptermWriter(stderr, stderrInteractive)
	level := pterm.LogLevelDisabled
	if debugEnabled {
		level = pterm.LogLevelDebug
	}
	a.debugLogger = pterm.DefaultLogger.WithWriter(a.debugWriter).WithTime(false).WithLevel(level)
	if len(args) > 0 && args[0] == "--version" {
		if len(args) != 1 {
			writeUsageFailure(stderr, usageErrorf("--version does not accept arguments"), args, a.LookupEnv)
			return 2
		}
		if err := writeVersion(stdout, a.progressInteractive(stdout)); err != nil {
			fmt.Fprintf(stderr, "write version: %v\n", err)
			return 1
		}
		return 0
	}
	if len(args) == 0 {
		if err := writeHelp(stderr, "", a.LookupEnv); err != nil {
			fmt.Fprintf(stderr, "write help: %v\n", err)
			return 1
		}
		return 2
	}
	if topic, requested, err := requestedHelp(args); requested {
		if err != nil {
			writeUsageFailure(stderr, err, args, a.LookupEnv)
			return 2
		}
		if err := writeHelp(stdout, topic, a.LookupEnv); err != nil {
			fmt.Fprintf(stderr, "write help: %v\n", err)
			return 1
		}
		return 0
	}
	if a.Geteuid == nil || a.Geteuid() == 0 {
		fmt.Fprintln(stderr, "runtime: qemu-manage must not run as root")
		return 1
	}
	if a.initializationError != nil {
		fmt.Fprintf(stderr, "config: initialize store: %v\n", a.initializationError)
		return 1
	}
	a.debugf("command=%q data_root=%q runtime_root=%q log_root=%q", args[0], a.Store.DataRoot, a.Store.RuntimeRoot, a.Store.LogRoot)

	var err error
	switch args[0] {
	case "create":
		name, rest, parseErr := nameBeforeFlags("create", args[1:])
		if parseErr != nil {
			err = parseErr
		} else {
			err = a.runCreate(ctx, name, rest, stdin, stdout, stderr)
		}
	case "set":
		name, rest, parseErr := nameBeforeFlags("set", args[1:])
		if parseErr != nil {
			err = parseErr
		} else {
			err = a.runSet(ctx, name, rest, stdin, stdout, stderr)
		}
	case "config":
		err = a.dispatchConfig(ctx, args[1:], stdin, stdout, stderr)
	case "showcmd", "log", "status", "list", "info", "delete":
		err = a.runInfoCommand(ctx, args[0], args[1:], stdin, stdout, stderr)
	case "start", "stop", "restart", "console", "monitor", "guest-agent", "vnc", "doctor", "supervise":
		err = a.runRuntimeCommand(ctx, args[0], args[1:], stdin, stdout, stderr)
	case "autostart":
		err = a.runAutostart(ctx, args[1:], stdout, stderr)
	default:
		err = usageErrorf("unknown command %q", args[0])
	}
	if err == nil {
		return 0
	}
	var usage *usageError
	if errors.As(err, &usage) {
		writeUsageFailure(stderr, err, args, a.LookupEnv)
		return 2
	}
	var silent *silentError
	if errors.As(err, &silent) {
		return 1
	}
	fmt.Fprintln(stderr, err)
	return 1
}

func writeUsageFailure(stderr io.Writer, err error, args []string, lookupEnv func(string) (string, bool)) {
	fmt.Fprintf(stderr, "error: %v\n\n", err)
	if helpErr := writeHelp(stderr, inferHelpTopic(args), lookupEnv); helpErr != nil {
		fmt.Fprintf(stderr, "write help: %v\n", helpErr)
	}
}

func (a *App) dispatchConfig(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageErrorf("config: missing subcommand")
	}
	switch args[0] {
	case "show", "validate", "apply":
		return a.runConfig(ctx, args[0], args[1:], stdin, stdout, stderr)
	default:
		return usageErrorf("config: unknown subcommand %q", args[0])
	}
}

func nameBeforeFlags(command string, args []string) (string, []string, error) {
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return "", nil, usageErrorf("%s: missing NAME", command)
	}
	return args[0], args[1:], nil
}

func quietFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseNoPositionals(fs *flag.FlagSet, command string, args []string) error {
	if err := fs.Parse(args); err != nil {
		return usageErrorf("%s: %v", command, err)
	}
	if fs.NArg() != 0 {
		return usageErrorf("%s: unexpected arguments", command)
	}
	return nil
}

func parseLeadingDebugFlags(args []string) (bool, []string) {
	debug := false
	for len(args) > 0 {
		switch args[0] {
		case "-d", "--debug":
			debug = true
			args = args[1:]
		default:
			return debug, args
		}
	}
	return debug, args
}

func (a *App) debugf(format string, args ...any) {
	if a == nil || a.debugLogger == nil {
		return
	}
	a.debugLogger.Debug(fmt.Sprintf(format, args...))
}
func (a *App) progressInteractive(output io.Writer) bool {
	if carrier, ok := output.(terminalOutputCarrier); ok {
		return carrier.terminalOutputInteractive()
	}
	if a == nil || a.IsTerminalOutput == nil {
		return false
	}
	return a.IsTerminalOutput(output)
}

func terminalWriter(output io.Writer) bool {
	file, ok := output.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (a *App) runExternal(ctx context.Context, path string, args []string) error {
	a.debugf("external argv=%s", formatQuotedArgv(path, args))
	if a == nil || a.RunExternal == nil {
		return errors.New("external command runner is unavailable")
	}
	return a.RunExternal(ctx, path, args)
}

func formatQuotedArgv(path string, args []string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, strconv.Quote(path))
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}
