package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/bradsjm/qemu-manage/internal/store"
)

const defaultReadyTimeout = 15 * time.Second

// ResponseError is an error returned by the supervisor. Code is preserved so
// callers can distinguish a shutdown timeout from a transport failure.
type ResponseError struct {
	Code    ErrorCode
	Message string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("supervisor: %s: %s", e.Code, e.Message)
}

// Control sends one request to a supervisor control socket.
func Control(ctx context.Context, socketPath string, request Request) (Response, error) {
	if err := request.Validate(); err != nil {
		return Response{}, fmt.Errorf("supervisor: %w", err)
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return Response{}, fmt.Errorf("supervisor: connect control socket: %w", err)
	}
	defer connection.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return Response{}, fmt.Errorf("supervisor: set control deadline: %w", err)
		}
	}
	if err := EncodeRequest(connection, &request); err != nil {
		return Response{}, fmt.Errorf("supervisor: send control request: %w", err)
	}
	response, err := DecodeResponse(connection)
	if err != nil {
		return Response{}, fmt.Errorf("supervisor: read control response: %w", err)
	}
	if response.Version != request.Version {
		return Response{}, fmt.Errorf("supervisor: response version %d does not match request version %d", response.Version, request.Version)
	}
	if response.ID != request.ID {
		return Response{}, fmt.Errorf("supervisor: response ID %q does not match request ID %q", response.ID, request.ID)
	}
	if !response.OK {
		return *response, &ResponseError{Code: response.Error.Code, Message: response.Error.Message}
	}
	return *response, nil
}

// ReadyMessage is the single message emitted by a supervisor during startup.
type ReadyMessage struct {
	Version int     `json:"version"`
	ID      string  `json:"id"`
	OK      bool    `json:"ok"`
	Error   *string `json:"error,omitempty"`
}

func (message ReadyMessage) validate() error {
	if err := validateEnvelope(message.Version, message.ID); err != nil {
		return err
	}
	if message.OK && message.Error != nil {
		return errors.New("successful ready message must not include error")
	}
	if !message.OK && (message.Error == nil || *message.Error == "") {
		return errors.New("failed ready message must include error")
	}
	return nil
}

// WriteReady writes one capped, newline-framed ready message.
func WriteReady(writer io.Writer, message ReadyMessage) error {
	if err := message.validate(); err != nil {
		return fmt.Errorf("invalid ready message: %w", err)
	}
	return encodeLine(writer, &message)
}

// ReadReady reads one capped, newline-framed ready message.
func ReadReady(reader io.Reader) (ReadyMessage, error) {
	var message ReadyMessage
	if err := decodeLine(reader, &message); err != nil {
		return ReadyMessage{}, fmt.Errorf("invalid ready message: %w", err)
	}
	if err := message.validate(); err != nil {
		return ReadyMessage{}, fmt.Errorf("invalid ready message: %w", err)
	}
	return message, nil
}

// StartOptions describes either a detached supervisor re-exec or the same
// supervisor state machine invoked in-process for foreground operation.
type StartOptions struct {
	Name          string
	ExpectedID    string
	Executable    string
	Paths         store.Paths
	Foreground    bool
	BootMenu      bool
	Debug         bool
	DebugWriter   io.Writer
	ReadyTimeout  time.Duration
	OnReady       func()
	RunForeground func(context.Context, io.Writer) error
}

// StartProcess starts a supervisor and succeeds only after its valid ready
// message. Detached failures always terminate and reap the child.
func StartProcess(ctx context.Context, options StartOptions) error {
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = defaultReadyTimeout
	}
	if err := validateStartOptions(options); err != nil {
		return err
	}
	if options.Foreground {
		return startForeground(ctx, options)
	}
	return startDetached(ctx, options)
}

func validateStartOptions(options StartOptions) error {
	if options.Name == "" {
		return errors.New("supervisor: VM name is empty")
	}
	if err := validateEnvelope(ProtocolVersion, options.ExpectedID); err != nil {
		return fmt.Errorf("supervisor: invalid expected ID: %w", err)
	}
	if options.Foreground {
		if options.RunForeground == nil {
			return errors.New("supervisor: foreground runner is nil")
		}
		return nil
	}
	if !filepath.IsAbs(options.Executable) {
		return errors.New("supervisor: executable path must be absolute")
	}
	if options.Paths.SupervisorStdout == "" || options.Paths.SupervisorStderr == "" {
		return errors.New("supervisor: log paths are empty")
	}
	return nil
}

func startDetached(ctx context.Context, options StartOptions) error {
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("supervisor: create readiness pipe: %w", err)
	}
	defer readyReader.Close()

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		readyWriter.Close()
		return fmt.Errorf("supervisor: open %s: %w", os.DevNull, err)
	}
	defer stdin.Close()
	stdout, err := openSupervisorLog(options.Paths.SupervisorStdout)
	if err != nil {
		readyWriter.Close()
		return err
	}
	defer stdout.Close()
	stderr, err := openSupervisorLog(options.Paths.SupervisorStderr)
	if err != nil {
		readyWriter.Close()
		return err
	}
	defer stderr.Close()

	processAttributes, err := detachedProcessAttributes()
	if err != nil {
		readyWriter.Close()
		return err
	}
	argv := detachedSupervisorArgv(options)
	process, err := os.StartProcess(options.Executable, argv, &os.ProcAttr{
		Files: []*os.File{stdin, stdout, stderr, readyWriter},
		Sys:   processAttributes,
	})
	readyWriter.Close()
	if err != nil {
		return fmt.Errorf("supervisor: start process: %w", err)
	}

	message, err := awaitReady(ctx, readyReader, options.ReadyTimeout)
	if err != nil {
		return terminateAndReap(process, err)
	}
	if err := requireMatchingReady(message, options.ExpectedID); err != nil {
		return terminateAndReap(process, err)
	}
	if options.OnReady != nil {
		options.OnReady()
	}
	debugf(options.Debug, options.DebugWriter, "detached supervisor pid=%d ready=true", process.Pid)
	if err := process.Release(); err != nil {
		return terminateAndReap(process, fmt.Errorf("supervisor: release process handle: %w", err))
	}
	return nil
}

func startForeground(ctx context.Context, options StartOptions) error {
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	result := make(chan error, 1)
	go func() {
		err := options.RunForeground(runContext, writer)
		_ = writer.CloseWithError(err)
		result <- err
	}()
	message, err := awaitReady(ctx, reader, options.ReadyTimeout)
	_ = reader.Close()
	if err != nil {
		return finishForegroundFailure(cancel, result, err)
	}
	if err := requireMatchingReady(message, options.ExpectedID); err != nil {
		return finishForegroundFailure(cancel, result, err)
	}
	if options.OnReady != nil {
		options.OnReady()
	}
	return <-result
}

func detachedSupervisorArgv(options StartOptions) []string {
	argv := make([]string, 0, 8)
	argv = append(argv, options.Executable)
	if options.Debug {
		argv = append(argv, "--debug")
	}
	argv = append(argv, "supervise", options.Name, "--ready-fd", "3", "--expected-id", options.ExpectedID)
	if options.BootMenu {
		argv = append(argv, "--boot-menu")
	}
	return argv
}

func awaitReady(ctx context.Context, reader io.Reader, timeout time.Duration) (ReadyMessage, error) {
	result := make(chan struct {
		message ReadyMessage
		err     error
	}, 1)
	go func() {
		message, err := ReadReady(reader)
		result <- struct {
			message ReadyMessage
			err     error
		}{message: message, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ReadyMessage{}, fmt.Errorf("supervisor: readiness canceled: %w", ctx.Err())
	case <-timer.C:
		return ReadyMessage{}, fmt.Errorf("supervisor: readiness timed out after %s", timeout)
	case outcome := <-result:
		if outcome.err != nil {
			return ReadyMessage{}, fmt.Errorf("supervisor: read readiness: %w", outcome.err)
		}
		return outcome.message, nil
	}
}

func requireMatchingReady(message ReadyMessage, expectedID string) error {
	if message.Version != ProtocolVersion {
		return fmt.Errorf("supervisor: ready version %d does not match %d", message.Version, ProtocolVersion)
	}
	if message.ID != expectedID {
		return fmt.Errorf("supervisor: ready ID %q does not match expected ID %q", message.ID, expectedID)
	}
	if !message.OK {
		return fmt.Errorf("supervisor: %s", *message.Error)
	}
	return nil
}

func openSupervisorLog(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("supervisor: open log %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("supervisor: secure log %q: %w", path, err)
	}
	return file, nil
}

func finishForegroundFailure(cancel context.CancelFunc, result <-chan error, cause error) error {
	cancel()
	if cleanupErr := <-result; cleanupErr != nil {
		return errors.Join(cause, fmt.Errorf("supervisor: foreground cleanup: %w", cleanupErr))
	}
	return cause
}

func terminateAndReap(process *os.Process, cause error) error {
	signalErr := terminateSupervisor(process)
	_, waitErr := process.Wait()
	if cooperativeTerminationProcessDone(signalErr) {
		signalErr = nil
	}
	return errors.Join(
		cause,
		wrapProcessCleanupError("request cooperative termination", signalErr),
		wrapProcessCleanupError("reap process", waitErr),
	)
}

func wrapProcessCleanupError(operation string, err error) error {
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return fmt.Errorf("supervisor: %s: %w", operation, err)
}
