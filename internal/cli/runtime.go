package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jedib0t/go-pretty/v6/table"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
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
		return a.runStart(ctx, args, stderr)
	case "stop":
		return a.runStop(ctx, args, stderr)
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
		*foreground,
		*bootMenu,
		paths.SupervisorStdout,
		paths.SupervisorStderr,
	)
	ready := make(chan struct{})
	var readyOnce sync.Once
	startDone := make(chan error, 1)
	progressErr := withProgress(stderr, true, a.progressInteractive(stderr), "Starting VM (checking prerequisites and waiting for readiness)", 0, progress.UnitsDefault, func(tracker *progress.Tracker) error {
		go func() {
			startDone <- supervisor.StartProcess(ctx, supervisor.StartOptions{
				Name:         config.Name,
				ExpectedID:   config.ID,
				Executable:   executable,
				Paths:        paths,
				Foreground:   *foreground,
				BootMenu:     *bootMenu,
				Debug:        a.debug,
				DebugWriter:  a.debugWriter,
				ReadyTimeout: supervisorReadyTimeout,
				OnReady: func() {
					if tracker != nil {
						tracker.MarkAsDone()
					}
					readyOnce.Do(func() { close(ready) })
				},
				RunForeground: func(runCtx context.Context, ready io.Writer) error {
					return a.Supervisor.Supervise(runCtx, config.Name, config.ID, ready, supervisor.SuperviseOptions{
						BootMenu:    *bootMenu,
						Debug:       a.debug,
						DebugWriter: a.debugWriter,
					})
				},
			})
		}()
		select {
		case err := <-startDone:
			return err
		case <-ready:
			return nil
		}
	})
	if progressErr != nil {
		return fmt.Errorf("runtime: start %q: %w", name, progressErr)
	}
	startErr := <-startDone
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
	return withWaitingProgress(stderr, true, a.progressInteractive(stderr), "Stopping VM (waiting for shutdown response)", func() error {
		return a.Lifecycle.Stop(ctx, config, *timeout, *force)
	})
}
func withConnectionProgress(output io.Writer, interactive bool, message string, connect func(func()) error) error {
	ready := make(chan struct{})
	var readyOnce sync.Once
	done := make(chan error, 1)
	completed := false
	progressErr := withWaitingProgress(output, true, interactive, message, func() error {
		go func() {
			done <- connect(func() {
				readyOnce.Do(func() { close(ready) })
			})
		}()
		select {
		case err := <-done:
			completed = true
			return err
		case <-ready:
			return nil
		}
	})
	if progressErr != nil {
		return progressErr
	}
	if completed {
		return nil
	}
	return <-done
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
	return withConnectionProgress(stderr, a.progressInteractive(stderr), "Connecting to VM console", func(setup func()) error {
		return console.ConnectWithSetup(ctx, paths.Console, stdin, stdout, func() {
			setup()
			if replay != nil {
				if _, copyErr := io.Copy(stdout, replay); copyErr != nil {
					a.debugf("console replay unavailable: %v", copyErr)
				}
			}
		})
	})
}

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
		return withConnectionProgress(stderr, a.progressInteractive(stderr), "Connecting to QEMU monitor", func(setup func()) error {
			return console.ConnectMonitorWithSetup(ctx, paths.Monitor, stdin, stdout, setup)
		})
	}
	if a.DialQMP == nil {
		return errors.New("runtime: monitor is unavailable")
	}
	var client MonitorClient
	if err := withWaitingProgress(stderr, true, a.progressInteractive(stderr), "Connecting to QEMU monitor", func() error {
		var err error
		client, err = a.DialQMP(ctx, paths.QMPCommand)
		if err != nil {
			return fmt.Errorf("runtime: monitor: %w", err)
		}
		return nil
	}); err != nil {
		return err
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
	if err := withWaitingProgress(stderr, true, a.progressInteractive(stderr), "Running prerequisite checks", collect); err != nil {
		return err
	}
	rows := make([]table.Row, 0, len(checks))
	for _, check := range checks {
		rows = append(rows, table.Row{check.Name, check.Status, check.Evidence})
	}
	if err := writeTable(stdout, table.Row{"CHECK", "STATUS", "EVIDENCE"}, rows); err != nil {
		return fmt.Errorf("qemu: write doctor output: %w", err)
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
