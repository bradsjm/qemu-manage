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
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("console: connect: %w", err)
	}

	var restoreOnce sync.Once
	var restoreErr error
	restore := func() error { return nil }
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		state, rawErr := term.MakeRaw(int(file.Fd()))
		if rawErr != nil {
			_ = conn.Close()
			return fmt.Errorf("console: make terminal raw: %w", rawErr)
		}
		restore = func() error {
			restoreOnce.Do(func() {
				restoreErr = term.Restore(int(file.Fd()), state)
			})
			return restoreErr
		}
	}

	results := make(chan copyResult, 2)
	go copyGuestOutput(conn, stdout, results)
	go copyLocalInput(conn, stdin, results)

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
		resetReadErr = resetReadDeadline(stdin)
	}
	if writeDeadlineSet {
		resetWriteErr = resetWriteDeadline(stdout)
	}

	if err := restore(); err != nil {
		restoreErr = fmt.Errorf("console: restore terminal: %w", err)
	}
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	} else if closeErr != nil {
		closeErr = fmt.Errorf("console: close: %w", closeErr)
	}
	return errors.Join(primaryErr, closeErr, restoreErr, resetReadErr, resetWriteErr)
}

func copyGuestOutput(conn net.Conn, stdout io.Writer, results chan<- copyResult) {
	_, err := io.Copy(stdout, conn)
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	if err != nil {
		err = fmt.Errorf("console: read guest output: %w", err)
	}
	results <- copyResult{err: err}
}

func copyLocalInput(conn net.Conn, stdin io.Reader, results chan<- copyResult) {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := stdin.Read(buffer)
		if n > 0 {
			input := buffer[:n]
			if index := bytes.IndexByte(input, disconnectByte); index >= 0 {
				if err := writeAll(conn, input[:index]); err != nil {
					results <- copyResult{err: fmt.Errorf("console: write guest input: %w", err)}
					return
				}
				results <- copyResult{}
				return
			}
			if err := writeAll(conn, input); err != nil {
				results <- copyResult{err: fmt.Errorf("console: write guest input: %w", err)}
				return
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				results <- copyResult{}
			} else {
				results <- copyResult{err: fmt.Errorf("console: read local input: %w", readErr)}
			}
			return
		}
		if n == 0 {
			results <- copyResult{err: fmt.Errorf("console: read local input: %w", io.ErrNoProgress)}
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

func resetReadDeadline(reader io.Reader) error {
	deadliner := reader.(readDeadliner)
	if err := deadliner.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("console: reset local input deadline: %w", err)
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

func resetWriteDeadline(writer io.Writer) error {
	deadliner := writer.(writeDeadliner)
	if err := deadliner.SetWriteDeadline(time.Time{}); err != nil {
		return fmt.Errorf("console: reset local output deadline: %w", err)
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
