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
	"sync"
	"time"

	"github.com/pterm/pterm"
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

// runtimeAdapter adapts lifecycle.Service to the narrower RuntimeService
// interface consumed by CLI status and delete checks.
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
		return a.runStart(ctx, args, stderr)
	case "stop":
		return a.runStop(ctx, args, stderr)
	case "restart":
		return a.runRestart(ctx, args, stderr)
	case "console":
		return a.runConsole(ctx, args, stdin, stdout, stderr)
	case "monitor":
		return a.runMonitor(ctx, args, stdin, stdout, stderr)
	case "guest-agent":
		return a.runGuestAgent(ctx, args, stdout)
	case "vnc":
		return a.runVNC(ctx, args, stdout)
	case "doctor":
		return a.runDoctor(ctx, args, stdout, stderr)
	case "supervise":
		return a.runSupervise(ctx, args)
	default:
		return usageErrorf("unknown runtime command %q", command)
	}
}

func (a *App) runStart(ctx context.Context, args []string, stderr io.Writer) error {
	name, rest, err := nameBeforeFlags("start", args)
	if err != nil {
		return err
	}
	flags := quietFlagSet("start")
	foreground := flags.Bool("foreground", false, "")
	bootMenu := flags.Bool("boot-menu", false, "")
	if err := parseNoPositionals(flags, "start", rest); err != nil {
		return err
	}
	config, err := a.loadQEMUConfig(name)
	if err != nil {
		return err
	}
	return a.startVM(ctx, config, *foreground, *bootMenu, stderr)
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
	config, err := a.loadQEMUConfig(name)
	if err != nil {
		return err
	}
	return a.stopVM(ctx, config, *timeout, *force, stderr)
}

func (a *App) runRestart(ctx context.Context, args []string, stderr io.Writer) error {
	name, rest, err := nameBeforeFlags("restart", args)
	if err != nil {
		return err
	}
	flags := quietFlagSet("restart")
	timeout := flags.Duration("timeout", 0, "")
	force := flags.Bool("force", false, "")
	foreground := flags.Bool("foreground", false, "")
	bootMenu := flags.Bool("boot-menu", false, "")
	if err := parseNoPositionals(flags, "restart", rest); err != nil {
		return err
	}
	config, err := a.loadQEMUConfig(name)
	if err != nil {
		return err
	}
	if err := a.stopVM(ctx, config, *timeout, *force, stderr); err != nil {
		return err
	}
	return a.startVM(ctx, config, *foreground, *bootMenu, stderr)
}

// stopVM asks the authenticated supervisor to stop config and reports the same
// progress frames to stderr as an interactive stop. A timeout of zero selects
// the VM's configured shutdown timeout. It is shared by stop and restart so the
// two paths cannot drift.
func (a *App) stopVM(ctx context.Context, config *model.Config, timeout time.Duration, force bool, stderr io.Writer) error {
	if a.Lifecycle == nil {
		return errors.New("runtime: lifecycle service is unavailable")
	}
	effectiveTimeout := timeout
	if effectiveTimeout == 0 {
		effectiveTimeout = time.Duration(config.ShutdownTimeoutSeconds) * time.Second
	}
	if effectiveTimeout < time.Second || effectiveTimeout > time.Hour || effectiveTimeout%time.Second != 0 {
		return &lifecycle.InvalidStopTimeoutError{Timeout: effectiveTimeout}
	}

	var startMessage string
	switch {
	case force:
		startMessage = fmt.Sprintf("Stopping %s VM — requesting forced kill", config.Name)
		_ = writeMessage(
			stderr,
			a.progressInteractive(stderr),
			messageWarning,
			"--force kills QEMU without guest cooperation; guest filesystem or data corruption is possible",
		)
	case config.GuestAgent.Enabled:
		startMessage = fmt.Sprintf("Stopping %s VM — sending guest shutdown command", config.Name)
	default:
		startMessage = fmt.Sprintf("Stopping %s VM — sending shutdown command", config.Name)
	}

	status := startLiveStatus(stderr, true, a.liveProgressInteractive(stderr), startMessage)
	acknowledged := false
	forcing := false
	err := a.Lifecycle.StopWithProgress(ctx, config, timeout, force, func(progress supervisor.StopProgress) {
		switch progress {
		case supervisor.StopProgressAcknowledged:
			acknowledged = true
			status.Update(fmt.Sprintf("Stopping %s VM — shutdown acknowledged; waiting up to %d seconds", config.Name, int(effectiveTimeout/time.Second)))
		case supervisor.StopProgressForcing:
			forcing = true
			status.Update(fmt.Sprintf("Stopping %s VM — forcing kill", config.Name))
		}
	})
	if err != nil {
		status.Fail("Failed to stop " + config.Name + " VM: " + err.Error())
		return err
	}

	switch {
	case forcing:
		status.Warning(config.Name + " VM was force-killed")
	case acknowledged:
		status.Success(config.Name + " VM stopped cleanly")
	default:
		status.Info(config.Name + " VM was already stopped")
	}
	return nil
}

// startVM launches config under its authenticated supervisor and waits for
// readiness, reporting the same progress as an interactive start. It is shared
// by start and restart so the two paths cannot drift.
func (a *App) startVM(ctx context.Context, config *model.Config, foreground, bootMenu bool, stderr io.Writer) error {
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
	a.debugf(
		"start name=%q foreground=%t boot_menu=%t supervisor_stdout=%q supervisor_stderr=%q",
		config.Name,
		foreground,
		bootMenu,
		paths.SupervisorStdout,
		paths.SupervisorStderr,
	)

	status := startLiveStatus(
		stderr,
		true,
		a.liveProgressInteractive(stderr),
		fmt.Sprintf("Starting %s VM — checking prerequisites and waiting for readiness", config.Name),
	)
	ready := make(chan struct{})
	var readyOnce sync.Once
	startDone := make(chan error, 1)
	go func() {
		startDone <- supervisor.StartProcess(ctx, supervisor.StartOptions{
			Name:         config.Name,
			ExpectedID:   config.ID,
			Executable:   executable,
			Paths:        paths,
			Foreground:   foreground,
			BootMenu:     bootMenu,
			Debug:        a.debug,
			DebugWriter:  a.debugWriter,
			ReadyTimeout: supervisorReadyTimeout,
			OnReady: func() {
				readyOnce.Do(func() { close(ready) })
			},
			RunForeground: func(runCtx context.Context, ready io.Writer) error {
				return a.Supervisor.Supervise(runCtx, config.Name, config.ID, ready, supervisor.SuperviseOptions{
					BootMenu:    bootMenu,
					Debug:       a.debug,
					DebugWriter: a.debugWriter,
				})
			},
		})
	}()
	if err := awaitVMStart(config.Name, status, ready, startDone); err != nil {
		return fmt.Errorf("runtime: start %q: %w", config.Name, err)
	}
	return nil
}

func awaitVMStart(vmName string, status *liveStatus, ready <-chan struct{}, startDone <-chan error) error {
	startMessage := fmt.Sprintf("Starting %s VM — checking prerequisites and waiting for readiness", vmName)
	successMessage := vmName + " VM is ready"

	failStart := func(err error) error {
		status.Fail(startMessage + " failed: " + err.Error())
		return err
	}
	waitForReady := func() {
		<-ready
		status.Success(successMessage)
	}

	select {
	case err := <-startDone:
		if err != nil {
			return failStart(err)
		}
		waitForReady()
		return nil
	default:
	}

	select {
	case err := <-startDone:
		if err != nil {
			return failStart(err)
		}
		waitForReady()
		return nil
	case <-ready:
		status.Success(successMessage)
		return <-startDone
	}
}

func withConnectionProgress(output io.Writer, interactive bool, startMessage, successMessage string, connect func(func()) error) error {
	status := startLiveStatus(output, true, interactive, startMessage)
	ready := make(chan struct{})
	var readyOnce sync.Once
	done := make(chan error, 1)
	go func() {
		done <- connect(func() {
			status.Success(successMessage)
			readyOnce.Do(func() { close(ready) })
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			status.Fail(startMessage + " failed: " + err.Error())
			return err
		}
		status.Success(successMessage)
		return nil
	case <-ready:
		return <-done
	}
}

func (a *App) runConsole(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
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
	paths := a.Store.Paths(config)
	var replay io.Reader
	if a.IsTerminalOutput != nil && a.IsTerminalOutput(stdout) {
		history, tailErr := tailActiveSerialLog(paths.SerialLog, 20, 64*1024)
		if tailErr != nil {
			a.debugf("console replay unavailable: %v", tailErr)
		} else if len(history) != 0 {
			replay = io.MultiReader(
				strings.NewReader("\r\n--- serial log: active file, up to 20 lines ---\r\n"),
				bytes.NewReader(history),
				strings.NewReader("\r\n--- live console; Ctrl-] to disconnect ---\r\n"),
			)
		}
	}
	return withConnectionProgress(
		stderr,
		a.liveProgressInteractive(stderr),
		fmt.Sprintf("Connecting to %s VM console", name),
		fmt.Sprintf("Connected to %s VM console; press Ctrl-] to disconnect", name),
		func(setup func()) error {
			return console.ConnectWithSetup(ctx, paths.Console, stdin, stdout, func() {
				setup()
				if replay != nil {
					if _, copyErr := io.Copy(stdout, replay); copyErr != nil {
						a.debugf("console replay unavailable: %v", copyErr)
					}
				}
			})
		},
	)
}

// openActiveSerialLog opens the current serial log after re-validating that the
// active path is the expected owner-only regular file for replay and tailing.
func openActiveSerialLog(path string) (file *os.File, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("serial log: open: %w", err)
	}
	openedFile := os.NewFile(uintptr(fd), path)
	defer func() {
		if err == nil {
			return
		}
		if closeErr := openedFile.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("serial log: close: %w", closeErr))
		}
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, fmt.Errorf("serial log: stat: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("serial log: file is not a regular file")
	}
	if stat.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("serial log: file is owned by uid %d, want %d", stat.Uid, os.Getuid())
	}
	if stat.Mode&0o777 != 0o600 {
		return nil, fmt.Errorf("serial log: file mode is %04o, want 0600", stat.Mode&0o777)
	}
	return openedFile, nil
}

// tailActiveSerialLog reads at most maxBytes from the active serial log and then
// trims that window to the last maxLines complete lines.
func tailActiveSerialLog(path string, maxLines int, maxBytes int64) ([]byte, error) {
	if maxLines <= 0 || maxBytes <= 0 {
		return nil, nil
	}
	file, err := openActiveSerialLog(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := stat.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	history := make([]byte, size-offset)
	if len(history) != 0 {
		n, readErr := file.ReadAt(history, offset)
		history = history[:n]
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
	}
	if offset > 0 {
		if newline := bytes.IndexByte(history, '\n'); newline >= 0 {
			history = history[newline+1:]
		}
	}
	if len(history) == 0 {
		return nil, nil
	}
	separators := maxLines - 1
	startIndex := len(history) - 1
	if history[len(history)-1] == '\n' {
		separators = maxLines
		startIndex--
	}
	for index := startIndex; index >= 0; index-- {
		if history[index] != '\n' {
			continue
		}
		separators--
		if separators == 0 {
			history = history[index+1:]
			break
		}
	}
	return history, nil
}

func (a *App) runMonitor(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (err error) {
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
		return withConnectionProgress(
			stderr,
			a.liveProgressInteractive(stderr),
			fmt.Sprintf("Connecting to QEMU monitor for %s VM", name),
			fmt.Sprintf("Connected to QEMU monitor for %s VM", name),
			func(setup func()) error {
				return console.ConnectMonitorWithSetup(ctx, paths.Monitor, stdin, stdout, setup)
			},
		)
	}
	if a.DialQMP == nil {
		return errors.New("runtime: monitor is unavailable")
	}
	var client MonitorClient
	// Dial QMP first so one-shot monitor commands fail before entering the control
	// path.
	if err := withWaitingProgress(
		stderr,
		true,
		a.liveProgressInteractive(stderr),
		fmt.Sprintf("Connecting to QEMU monitor for %s VM", name),
		fmt.Sprintf("Connected to QEMU monitor for %s VM", name),
		func() error {
			var err error
			client, err = a.DialQMP(ctx, paths.QMPCommand)
			if err != nil {
				return fmt.Errorf("runtime: monitor: %w", err)
			}
			return nil
		},
	); err != nil {
		return err
	}
	// Always close the QMP client after the command path finishes.
	defer func() {
		if closeErr := client.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("runtime: monitor: %w", closeErr)
		}
	}()
	// Run one human monitor command, print its response, and normalize the trailing
	// newline for terminal callers.
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
	if err := writeMessage(stdout, a.progressInteractive(stdout), messageInfo, "VNC password copied to clipboard"); err != nil {
		return err
	}
	return writeMessage(stdout, a.progressInteractive(stdout), messageSuccess, fmt.Sprintf("Opening vnc://%s:%d", endpoint.Host, endpoint.Port))
}

func (a *App) runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
	var checks []qemu.Check
	collect := func() error {
		checks = qemu.Doctor(ctx, config, paths)
		return nil
	}
	if *jsonOutput {
		if err := collect(); err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(checks); err != nil {
			return fmt.Errorf("qemu: write doctor JSON: %w", err)
		}
		if err := qemu.RequiredPassed(checks); err != nil {
			return &silentError{err: err}
		}
		return nil
	}
	startMessage := "Running host prerequisite checks"
	successMessage := "Completed host prerequisite checks"
	if name != "" {
		startMessage = fmt.Sprintf("Running prerequisite checks for %s VM", name)
		successMessage = fmt.Sprintf("Completed prerequisite checks for %s VM", name)
	}
	if err := withWaitingProgress(stderr, true, a.liveProgressInteractive(stderr), startMessage, successMessage, collect); err != nil {
		return err
	}
	rows := make([][]string, 0, len(checks))
	interactive := a.progressInteractive(stdout)
	for _, check := range checks {
		rows = append(rows, []string{check.Name, displayCheckStatus(check.Status, interactive), check.Evidence})
	}
	if err := writeTable(stdout, interactive, []string{"CHECK", "STATUS", "EVIDENCE"}, rows); err != nil {
		return fmt.Errorf("qemu: write doctor output: %w", err)
	}
	return qemu.RequiredPassed(checks)
}

func displayCheckStatus(status qemu.CheckStatus, interactive bool) string {
	text := string(status)
	if !interactive {
		return text
	}
	switch status {
	case qemu.CheckPass:
		return applyStyle(pterm.NewStyle(pterm.FgLightGreen), text)
	case qemu.CheckWarn:
		return applyStyle(pterm.NewStyle(pterm.FgLightYellow), text)
	case qemu.CheckFail:
		return applyStyle(pterm.NewStyle(pterm.FgLightRed), text)
	default:
		return text
	}
}

func (a *App) runSupervise(ctx context.Context, args []string) error {
	name, rest, err := nameBeforeFlags("supervise", args)
	if err != nil {
		return err
	}
	flags := quietFlagSet("supervise")
	readyFD := flags.Int("ready-fd", -1, "")
	expectedID := flags.String("expected-id", "", "")
	bootMenu := flags.Bool("boot-menu", false, "")
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
	return a.Supervisor.Supervise(ctx, name, *expectedID, ready, supervisor.SuperviseOptions{
		BootMenu:    *bootMenu,
		Debug:       a.debug,
		DebugWriter: a.debugWriter,
	})
}

func (a *App) loadQEMUConfig(name string) (*model.Config, error) {
	return a.Store.Load(name)
}

// backendPaths projects store-managed paths into the runtime/backend view
// shared by diagnostics and supervision.
func backendPaths(paths store.Paths) backend.RuntimePaths {
	return backend.RuntimePaths{
		VMDir:         paths.VMDir,
		QMP:           paths.QMP,
		QMPCommand:    paths.QMPCommand,
		QGA:           paths.QGA,
		Console:       paths.Console,
		Monitor:       paths.Monitor,
		QEMULog:       paths.QEMULog,
		SerialLogPipe: paths.SerialLogPipe,
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
		&paths.QEMULog, &paths.SerialLog, &paths.SerialLogPipe,
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
