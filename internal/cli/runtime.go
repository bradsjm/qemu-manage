package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/console"
	"github.com/bradsjm/qemu-manage/internal/lifecycle"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/qemu"
	"github.com/bradsjm/qemu-manage/internal/store"
	"github.com/bradsjm/qemu-manage/internal/supervisor"
)

// supervisorReadyTimeout covers the four sequential five-second doctor probes
// (version, HVF, exit-with-parent, and socket_vmnet) plus QEMU's independent
// 15-second QMP readiness window, with ten seconds for process setup and IPC.
const supervisorReadyTimeout = 45 * time.Second

type runtimeAdapter struct {
	service *lifecycle.Service
}

func newRuntimeAdapter(service *lifecycle.Service) RuntimeService {
	return &runtimeAdapter{service: service}
}

func (r *runtimeAdapter) Status(ctx context.Context, config *model.Config) (StatusRow, error) {
	if r == nil || r.service == nil {
		return StatusRow{}, errors.New("runtime: lifecycle service is unavailable")
	}
	result, err := r.service.Status(ctx, config)
	if err != nil {
		return StatusRow{}, err
	}
	row := StatusRow{
		Name:                result.Name,
		State:               result.State,
		Backend:             string(result.Backend),
		CurrentConfigSHA256: result.CurrentConfigSHA256,
		RunningConfigSHA256: result.RunningConfigSHA256,
		VNC:                 result.VNC,
		Error:               result.Error,
	}
	if result.PID > 0 {
		pid := result.PID
		row.PID = &pid
	}
	row.RestartRequired = row.RunningConfigSHA256 != "" && row.RunningConfigSHA256 != row.CurrentConfigSHA256
	return row, nil
}

func (r *runtimeAdapter) DeleteAllowed(ctx context.Context, config *model.Config) (bool, error) {
	if r == nil || r.service == nil {
		return false, errors.New("runtime: lifecycle service is unavailable")
	}
	if err := r.service.DeleteAllowed(ctx, config); err != nil {
		return false, err
	}
	return true, nil
}

func (a *App) runRuntimeCommand(ctx context.Context, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch command {
	case "start":
		return a.runStart(ctx, args)
	case "stop":
		return a.runStop(ctx, args, stderr)
	case "console":
		return a.runConsole(ctx, args, stdin, stdout)
	case "monitor":
		return a.runMonitor(ctx, args, stdin, stdout)
	case "guest-agent":
		return a.runGuestAgent(ctx, args, stdout)
	case "vnc":
		return a.runVNC(ctx, args, stdout)
	case "doctor":
		return a.runDoctor(ctx, args, stdout)
	case "supervise":
		return a.runSupervise(ctx, args)
	default:
		return usageErrorf("unknown runtime command %q", command)
	}
}

func (a *App) runStart(ctx context.Context, args []string) error {
	name, rest, err := nameBeforeFlags("start", args)
	if err != nil {
		return err
	}
	flags := quietFlagSet("start")
	foreground := flags.Bool("foreground", false, "")
	if err := parseNoPositionals(flags, "start", rest); err != nil {
		return err
	}
	config, err := a.loadQEMUConfig(name)
	if err != nil {
		return err
	}
	if a.Supervisor == nil {
		return errors.New("runtime: supervisor service is unavailable")
	}
	executable, err := filepath.Abs(a.ExecutablePath)
	if err != nil || a.ExecutablePath == "" {
		if err == nil {
			err = errors.New("executable path is empty")
		}
		return fmt.Errorf("runtime: resolve executable path: %w", err)
	}
	paths, err := absoluteStorePaths(a.Store.Paths(config))
	if err != nil {
		return err
	}
	startErr := supervisor.StartProcess(ctx, supervisor.StartOptions{
		Name:         config.Name,
		ExpectedID:   config.ID,
		Executable:   executable,
		Paths:        paths,
		Foreground:   *foreground,
		ReadyTimeout: supervisorReadyTimeout,
		RunForeground: func(runCtx context.Context, ready io.Writer) error {
			return a.Supervisor.Supervise(runCtx, config.Name, config.ID, ready)
		},
	})
	if startErr != nil {
		return fmt.Errorf("runtime: start %q: %w", name, startErr)
	}
	return nil
}

func (a *App) runStop(ctx context.Context, args []string, stderr io.Writer) error {
	name, rest, err := nameBeforeFlags("stop", args)
	if err != nil {
		return err
	}
	flags := quietFlagSet("stop")
	timeout := flags.Duration("timeout", 0, "")
	force := flags.Bool("force", false, "")
	if err := parseNoPositionals(flags, "stop", rest); err != nil {
		return err
	}
	config, err := a.Store.Load(name)
	if err != nil {
		return err
	}
	if a.Lifecycle == nil {
		return errors.New("runtime: lifecycle service is unavailable")
	}
	if *force {
		fmt.Fprintf(
			stderr,
			"warning: --force kills QEMU without guest cooperation; guest filesystem or data corruption is possible\n",
		)
	}
	return a.Lifecycle.Stop(ctx, config, *timeout, *force)
}

func (a *App) runConsole(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	name, rest, err := nameBeforeFlags("console", args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usageErrorf("console: unexpected arguments")
	}
	config, err := a.Store.Load(name)
	if err != nil {
		return err
	}
	if a.Lifecycle == nil {
		return errors.New("runtime: lifecycle service is unavailable")
	}
	status, err := a.Lifecycle.Status(ctx, config)
	if err != nil {
		return err
	}
	if status.State != model.RunStateRunning && status.State != model.RunStatePaused {
		return fmt.Errorf("runtime: VM %q is %s; console requires running or paused", name, status.State)
	}
	if err := console.Connect(ctx, a.Store.Paths(config).Console, stdin, stdout); err != nil {
		return err
	}
	return nil
}

func (a *App) runMonitor(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (err error) {
	name, rest, err := nameBeforeFlags("monitor", args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usageErrorf("monitor: expected at most one COMMAND")
	}
	if len(rest) == 1 && strings.TrimSpace(rest[0]) == "" {
		return usageErrorf("monitor: COMMAND must not be empty")
	}
	config, err := a.loadQEMUConfig(name)
	if err != nil {
		return err
	}
	status, err := a.statusRow(ctx, config)
	if err != nil {
		return err
	}
	if status.State != model.RunStateRunning && status.State != model.RunStatePaused {
		return fmt.Errorf("runtime: VM %q is %s; monitor requires running or paused", name, status.State)
	}
	paths := a.Store.Paths(config)
	if len(rest) == 0 {
		return console.ConnectMonitor(ctx, paths.Monitor, stdin, stdout)
	}
	if a.DialQMP == nil {
		return errors.New("runtime: monitor is unavailable")
	}
	client, err := a.DialQMP(ctx, paths.QMPCommand)
	if err != nil {
		return fmt.Errorf("runtime: monitor: %w", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("runtime: monitor: %w", closeErr)
		}
	}()
	output, err := client.HumanMonitorCommand(ctx, rest[0])
	if err != nil {
		return fmt.Errorf("runtime: monitor: %w", err)
	}
	if _, err := io.WriteString(stdout, output); err != nil {
		return err
	}
	if strings.HasSuffix(output, "\n") {
		return nil
	}
	_, err = io.WriteString(stdout, "\n")
	return err
}

func (a *App) runGuestAgent(ctx context.Context, args []string, stdout io.Writer) error {
	name, rest, err := nameBeforeFlags("guest-agent", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return usageErrorf("guest-agent: missing REQUEST")
	}
	if len(rest) > 1 {
		return usageErrorf("guest-agent: unexpected arguments")
	}
	request, err := qemu.DecodeGuestAgentRequest([]byte(rest[0]))
	if err != nil {
		return usageErrorf("guest-agent: %v", err)
	}
	config, err := a.loadQEMUConfig(name)
	if err != nil {
		return err
	}
	if !config.GuestAgent.Enabled {
		return fmt.Errorf("runtime: VM %q does not have the guest agent enabled", name)
	}
	status, err := a.statusRow(ctx, config)
	if err != nil {
		return err
	}
	if status.State != model.RunStateRunning && status.State != model.RunStatePaused {
		return fmt.Errorf("runtime: VM %q is %s; guest-agent requires running or paused", name, status.State)
	}
	if a.CallGuestAgent == nil {
		return errors.New("runtime: guest-agent is unavailable")
	}
	payload, err := a.CallGuestAgent(ctx, a.Store.Paths(config).QGA, request)
	if err != nil {
		return fmt.Errorf("runtime: guest-agent: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return fmt.Errorf("runtime: guest-agent: compact JSON return: %w", err)
	}
	if err := compact.WriteByte('\n'); err != nil {
		return err
	}
	_, err = stdout.Write(compact.Bytes())
	return err
}
func (a *App) runVNC(ctx context.Context, args []string, stdout io.Writer) error {
	name, rest, err := nameBeforeFlags("vnc", args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usageErrorf("vnc: unexpected arguments")
	}
	config, err := a.loadQEMUConfig(name)
	if err != nil {
		return err
	}
	if config.VNC == nil {
		return fmt.Errorf("runtime: VM %q does not have VNC enabled", name)
	}
	status, err := a.statusRow(ctx, config)
	if err != nil {
		return err
	}
	if status.State != model.RunStateRunning && status.State != model.RunStatePaused {
		return fmt.Errorf("runtime: VM %q is %s; VNC requires running or paused", name, status.State)
	}
	if status.RestartRequired {
		return fmt.Errorf("runtime: VM %q requires restart before VNC can use the current password", name)
	}
	if status.VNC == nil {
		return fmt.Errorf("runtime: VM %q has no live VNC endpoint", name)
	}
	if a.OpenVNC == nil {
		return errors.New("vnc: viewer is unavailable")
	}
	endpoint := *status.VNC
	if endpoint.Host == "0.0.0.0" {
		endpoint.Host = "127.0.0.1"
	}
	if err := a.OpenVNC(ctx, endpoint, config.VNC.Password); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "VNC password copied to clipboard; opening vnc://%s:%d\n", endpoint.Host, endpoint.Port)
	return err
}

func (a *App) runDoctor(ctx context.Context, args []string, stdout io.Writer) error {
	name := ""
	if len(args) != 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	flags := quietFlagSet("doctor")
	jsonOutput := flags.Bool("json", false, "")
	if err := parseNoPositionals(flags, "doctor", args); err != nil {
		return err
	}
	config := model.Config{}
	var paths backend.RuntimePaths
	if name != "" {
		loaded, err := a.loadQEMUConfig(name)
		if err != nil {
			return err
		}
		config = *loaded
		paths = backendPaths(a.Store.Paths(loaded))
	}
	checks := qemu.Doctor(ctx, config, paths)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(checks); err != nil {
			return fmt.Errorf("qemu: write doctor JSON: %w", err)
		}
		return qemu.RequiredPassed(checks)
	}
	if _, err := fmt.Fprintln(stdout, "CHECK\tSTATUS\tEVIDENCE"); err != nil {
		return fmt.Errorf("qemu: write doctor output: %w", err)
	}
	for _, check := range checks {
		if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", check.Name, check.Status, check.Evidence); err != nil {
			return fmt.Errorf("qemu: write doctor output: %w", err)
		}
	}
	return qemu.RequiredPassed(checks)
}

func (a *App) runSupervise(ctx context.Context, args []string) error {
	name, rest, err := nameBeforeFlags("supervise", args)
	if err != nil {
		return err
	}
	flags := quietFlagSet("supervise")
	readyFD := flags.Int("ready-fd", -1, "")
	expectedID := flags.String("expected-id", "", "")
	if err := parseNoPositionals(flags, "supervise", rest); err != nil {
		return err
	}
	if *readyFD < 3 {
		return usageErrorf("supervise: --ready-fd must be at least 3")
	}
	if *expectedID == "" {
		return usageErrorf("supervise: --expected-id is required")
	}
	ready := os.NewFile(uintptr(*readyFD), "supervisor-ready")
	if ready == nil {
		return errors.New("runtime: inherited readiness FD is invalid")
	}
	unix.CloseOnExec(*readyFD)
	defer ready.Close()
	if a.Supervisor == nil {
		err := errors.New("runtime: supervisor service is unavailable")
		writeReadyFailure(ready, *expectedID, err)
		return err
	}
	return a.Supervisor.Supervise(ctx, name, *expectedID, ready)
}

func (a *App) loadQEMUConfig(name string) (*model.Config, error) {
	return a.Store.Load(name)
}

func backendPaths(paths store.Paths) backend.RuntimePaths {
	return backend.RuntimePaths{
		VMDir:      paths.VMDir,
		QMP:        paths.QMP,
		QMPCommand: paths.QMPCommand,
		QGA:        paths.QGA,
		Console:    paths.Console,
		Monitor:    paths.Monitor,
		QEMULog:    paths.QEMULog,
		SerialLog:  paths.SerialLog,
	}
}

func writeReadyFailure(ready io.Writer, id string, cause error) {
	message := cause.Error()
	_ = supervisor.WriteReady(ready, supervisor.ReadyMessage{Version: supervisor.ProtocolVersion, ID: id, OK: false, Error: &message})
}

func absoluteStorePaths(paths store.Paths) (store.Paths, error) {
	values := []*string{
		&paths.VMDir, &paths.Config, &paths.RuntimeDir, &paths.ControlSocket, &paths.LifetimeLock,
		&paths.QMP, &paths.QMPCommand, &paths.QGA, &paths.Console, &paths.Monitor, &paths.VNCSecret,
		&paths.RuntimeMetadata, &paths.LastExitMetadata, &paths.SupervisorStdout, &paths.SupervisorStderr,
		&paths.QEMULog, &paths.SerialLog,
	}
	for _, value := range values {
		absolute, err := filepath.Abs(*value)
		if err != nil {
			return store.Paths{}, fmt.Errorf("runtime: resolve store path %q: %w", *value, err)
		}
		*value = absolute
	}
	return paths, nil
}
