package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/launchd"
	"github.com/bradsjm/qemu-manage/internal/lifecycle"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/qemu"
	"github.com/bradsjm/qemu-manage/internal/store"
	"github.com/bradsjm/qemu-manage/internal/supervisor"
)

type PlatformUser struct {
	Name string
	Home string
	UID  int
}

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
}

type RuntimeService interface {
	Status(context.Context, *model.Config) (StatusRow, error)
	DeleteAllowed(context.Context, *model.Config) (bool, error)
}
type MonitorClient interface {
	HumanMonitorCommand(context.Context, string) (string, error)
	Close() error
}

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

	initializationError error
	debug               bool
	debugWriter         io.Writer
}

func NewApp() *App {
	if os.Geteuid() == 0 {
		return &App{
			Geteuid:         os.Geteuid,
			LookupEnv:       os.LookupEnv,
			DiscoverMachine: qemu.DiscoverVersionedMachine,
		}
	}

	a := &App{
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
	a.Supervisor.Preflight = func(ctx context.Context, config *model.Config, paths store.Paths) error {
		if _, err := a.Backends.Lookup(string(config.Backend)); err != nil {
			return err
		}
		return qemu.RequiredPassed(qemu.Doctor(ctx, *config, backendPaths(paths)))
	}
	a.Launchd = launchd.NewManager(a.Store, a.ExecutablePath, a.User.Name, a.User.Home, a.User.UID)
	a.ProvisionSocketVMNetBridge = a.Launchd.ProvisionSocketVMNetBridge
	a.Runtime = newRuntimeAdapter(a.Lifecycle)
	return a
}

type usageError struct {
	message string
}

func (e *usageError) Error() string { return e.message }

func usageErrorf(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

func (a *App) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a.debug = false
	a.debugWriter = nil
	debugEnabled, args := parseLeadingDebugFlags(args)
	a.debug = debugEnabled
	a.debugWriter = stderr
	if len(args) > 0 && args[0] == "--version" {
		if len(args) != 1 {
			writeUsageFailure(stderr, usageErrorf("--version does not accept arguments"), args, a.LookupEnv)
			return 2
		}
		if err := writeVersion(stdout); err != nil {
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
	case "showcmd", "status", "list", "delete":
		err = a.runInfoCommand(ctx, args[0], args[1:], stdin, stdout, stderr)
	case "start", "stop", "console", "monitor", "guest-agent", "vnc", "doctor", "supervise":
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
	if a == nil || !a.debug || a.debugWriter == nil {
		return
	}
	writeDebugf(a.debugWriter, format, args...)
}

func (a *App) runExternal(ctx context.Context, path string, args []string) error {
	a.debugf("external argv=%s", formatQuotedArgv(path, args))
	if a == nil || a.RunExternal == nil {
		return errors.New("external command runner is unavailable")
	}
	return a.RunExternal(ctx, path, args)
}

func writeDebugf(output io.Writer, format string, args ...any) {
	if output == nil {
		return
	}
	message := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	for _, line := range strings.SplitAfter(message, "\n") {
		if line == "" {
			continue
		}
		if _, err := io.WriteString(output, "debug: "); err != nil {
			return
		}
		if _, err := io.WriteString(output, line); err != nil {
			return
		}
	}
}

func formatQuotedArgv(path string, args []string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, strconv.Quote(path))
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}
