package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

const disconnectByte = byte(0x1d)

type copyResult struct {
	err error
}

// Connect attaches stdin and stdout to a VM's private Unix console socket.
func Connect(ctx context.Context, socketPath string, stdin io.Reader, stdout io.Writer) error {
	return connect(ctx, "console", socketPath, stdin, stdout, nil)
}

// ConnectWithSetup attaches stdin and stdout after setup has completed.
func ConnectWithSetup(ctx context.Context, socketPath string, stdin io.Reader, stdout io.Writer, setup func()) error {
	return connect(ctx, "console", socketPath, stdin, stdout, setup)
}

// ConnectMonitor attaches stdin and stdout to a VM's private human monitor socket.
func ConnectMonitor(ctx context.Context, socketPath string, stdin io.Reader, stdout io.Writer) error {
	return connect(ctx, "monitor", socketPath, stdin, stdout, nil)
}

// ConnectMonitorWithSetup attaches stdin and stdout after setup has completed.
func ConnectMonitorWithSetup(ctx context.Context, socketPath string, stdin io.Reader, stdout io.Writer, setup func()) error {
	return connect(ctx, "monitor", socketPath, stdin, stdout, setup)
}

func connect(ctx context.Context, prefix, socketPath string, stdin io.Reader, stdout io.Writer, setup func()) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("%s: connect: %w", prefix, err)
	}

	var restoreOnce sync.Once
	var restoreErr error
	restore := func() error { return nil }
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		state, rawErr := term.MakeRaw(int(file.Fd()))
		if rawErr != nil {
			_ = conn.Close()
			return fmt.Errorf("%s: make terminal raw: %w", prefix, rawErr)
		}
		restore = func() error {
			restoreOnce.Do(func() {
				restoreErr = term.Restore(int(file.Fd()), state)
			})
			return restoreErr
		}
	}
	if setup != nil {
		setup()
	}
	results := make(chan copyResult, 2)
	go copyGuestOutput(prefix, conn, stdout, results)
	go copyLocalInput(prefix, conn, stdin, results)

	var primaryErr error
	completed := 0
	select {
	case <-ctx.Done():
		primaryErr = ctx.Err()
	case result := <-results:
		primaryErr = result.err
		completed = 1
	}

	// Closing the connection interrupts both network operations. Deadlines are
	// also needed because closing conn cannot wake a goroutine blocked on a
	// caller-owned terminal or file, and those streams must not be closed here.
	closeErr := conn.Close()
	deadline := time.Now()
	readDeadlineSet := setReadDeadline(stdin, deadline)
	writeDeadlineSet := setWriteDeadline(stdout, deadline)

	for completed < 2 {
		<-results
		completed++
	}

	var resetReadErr, resetWriteErr error
	if readDeadlineSet {
		resetReadErr = resetReadDeadline(prefix, stdin)
	}
	if writeDeadlineSet {
		resetWriteErr = resetWriteDeadline(prefix, stdout)
	}

	if err := restore(); err != nil {
		restoreErr = fmt.Errorf("%s: restore terminal: %w", prefix, err)
	}
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	} else if closeErr != nil {
		closeErr = fmt.Errorf("%s: close: %w", prefix, closeErr)
	}
	return errors.Join(primaryErr, closeErr, restoreErr, resetReadErr, resetWriteErr)
}

func copyGuestOutput(prefix string, conn net.Conn, stdout io.Writer, results chan<- copyResult) {
	_, err := io.Copy(stdout, conn)
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	if err != nil {
		err = fmt.Errorf("%s: read guest output: %w", prefix, err)
	}
	results <- copyResult{err: err}
}

func copyLocalInput(prefix string, conn net.Conn, stdin io.Reader, results chan<- copyResult) {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := stdin.Read(buffer)
		if n > 0 {
			input := buffer[:n]
			if index := bytes.IndexByte(input, disconnectByte); index >= 0 {
				if err := writeAll(conn, input[:index]); err != nil {
					results <- copyResult{err: fmt.Errorf("%s: write guest input: %w", prefix, err)}
					return
				}
				results <- copyResult{}
				return
			}
			if err := writeAll(conn, input); err != nil {
				results <- copyResult{err: fmt.Errorf("%s: write guest input: %w", prefix, err)}
				return
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				results <- copyResult{}
			} else {
				results <- copyResult{err: fmt.Errorf("%s: read local input: %w", prefix, readErr)}
			}
			return
		}
		if n == 0 {
			results <- copyResult{err: fmt.Errorf("%s: read local input: %w", prefix, io.ErrNoProgress)}
			return
		}
	}
}

type readDeadliner interface {
	SetReadDeadline(time.Time) error
}

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

func setReadDeadline(reader io.Reader, deadline time.Time) bool {
	deadliner, ok := reader.(readDeadliner)
	if !ok {
		return false
	}
	return deadliner.SetReadDeadline(deadline) == nil
}

func resetReadDeadline(prefix string, reader io.Reader) error {
	deadliner := reader.(readDeadliner)
	if err := deadliner.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("%s: reset local input deadline: %w", prefix, err)
	}
	return nil
}

func setWriteDeadline(writer io.Writer, deadline time.Time) bool {
	deadliner, ok := writer.(writeDeadliner)
	if !ok {
		return false
	}
	return deadliner.SetWriteDeadline(deadline) == nil
}

func resetWriteDeadline(prefix string, writer io.Writer) error {
	deadliner := writer.(writeDeadliner)
	if err := deadliner.SetWriteDeadline(time.Time{}); err != nil {
		return fmt.Errorf("%s: reset local output deadline: %w", prefix, err)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
