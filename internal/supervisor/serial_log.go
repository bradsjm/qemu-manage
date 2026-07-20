package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	serialLogMaxSize    int64 = 2 * 1024 * 1024
	serialLogMaxBackups       = 3
	serialLogBufferSize       = 32 * 1024
)

// serialLogSink drains the serial FIFO and forwards it into a rotating durable
// log file until the sink is finished or aborted
type serialLogSink struct {
	reader  io.ReadCloser
	dummy   io.Closer
	done    chan error
	mu      sync.Mutex
	aborted bool
}

func startSerialLogSink(pipePath, logPath string, warningWriter io.Writer) (*serialLogSink, error) {
	output, err := openRotatingSerialLog(logPath, serialLogMaxSize, serialLogMaxBackups)
	if err != nil {
		return nil, err
	}
	reader, dummy, err := openSerialLogPipe(pipePath)
	if err != nil {
		return nil, errors.Join(err, output.Close())
	}
	if warningWriter == nil {
		warningWriter = io.Discard
	}
	sink := &serialLogSink{reader: reader, dummy: dummy, done: make(chan error, 1)}
	go func() {
		err := copySerialLog(reader, output, warningWriter)
		_ = reader.Close()
		sink.done <- err
	}()
	return sink, nil
}

func (s *serialLogSink) finish() error {
	if s == nil {
		return nil
	}
	dummyErr := s.dummy.Close()
	return errors.Join(dummyErr, <-s.done)
}

func (s *serialLogSink) abort() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.aborted = true
	s.mu.Unlock()
	dummyErr := s.dummy.Close()
	readerErr := s.reader.Close()
	copyErr := <-s.done
	if errors.Is(copyErr, os.ErrClosed) {
		copyErr = nil
	}
	return errors.Join(dummyErr, readerErr, copyErr)
}

// Keep copying until the FIFO closes. If durable writes fail, warn once and
// keep draining serial output instead of blocking the VM on logging errors.
func copySerialLog(input io.Reader, output io.WriteCloser, warningWriter io.Writer) error {
	buffer := make([]byte, serialLogBufferSize)
	for {
		n, readErr := input.Read(buffer)
		if n > 0 && output != nil {
			if _, writeErr := output.Write(buffer[:n]); writeErr != nil {
				cause := errors.Join(writeErr, output.Close())
				_, _ = fmt.Fprintf(warningWriter, "serial log: durable logging disabled: %v\n", cause)
				output = nil
			}
		}
		if readErr != nil {
			if output != nil {
				closeErr := output.Close()
				output = nil
				if errors.Is(readErr, io.EOF) {
					return closeErr
				}
				return errors.Join(readErr, closeErr)
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// rotatingSerialLog appends to the active log and renames numbered backups when
// the active file reaches maxSize
type rotatingSerialLog struct {
	path       string
	file       *os.File
	size       int64
	maxSize    int64
	maxBackups int
}

// openRotatingSerialLog validates the active log and numbered backups, removes
// stale oversized files, and opens the active file ready for size-based rotation.
func openRotatingSerialLog(path string, maxSize int64, maxBackups int) (*rotatingSerialLog, error) {
	if maxSize <= 0 || maxBackups <= 0 {
		return nil, errors.New("serial log: invalid rotation limits")
	}
	if err := inspectSerialLogDirectory(path); err != nil {
		return nil, err
	}
	for index := 0; index < maxBackups; index++ {
		backup := fmt.Sprintf("%s.%d", path, index)
		info, err := inspectSerialLogFile(backup)
		if err != nil {
			return nil, err
		}
		if info.exists && info.size > maxSize {
			if err := os.Remove(backup); err != nil {
				return nil, fmt.Errorf("serial log: remove oversized backup %q: %w", backup, err)
			}
		}
	}
	stale := fmt.Sprintf("%s.%d", path, maxBackups)
	if info, err := inspectSerialLogFile(stale); err != nil {
		return nil, err
	} else if info.exists {
		if err := os.Remove(stale); err != nil {
			return nil, fmt.Errorf("serial log: remove stale backup %q: %w", stale, err)
		}
	}
	file, size, err := openSecureSerialLog(path)
	if err != nil {
		return nil, err
	}
	if size > maxSize {
		if err := file.Truncate(0); err != nil {
			return nil, errors.Join(fmt.Errorf("serial log: truncate active log: %w", err), file.Close())
		}
		size = 0
	}
	return &rotatingSerialLog{path: path, file: file, size: size, maxSize: maxSize, maxBackups: maxBackups}, nil
}

func (w *rotatingSerialLog) Write(input []byte) (int, error) {
	consumed := 0
	for len(input) > 0 {
		if w.size == w.maxSize {
			if err := w.rotate(); err != nil {
				return consumed, err
			}
		}
		remaining := w.maxSize - w.size
		chunk := input
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		n, err := w.file.Write(chunk)
		consumed += n
		w.size += int64(n)
		input = input[n:]
		if err != nil {
			return consumed, fmt.Errorf("serial log: write: %w", err)
		}
		if n != len(chunk) {
			return consumed, io.ErrShortWrite
		}
	}
	return consumed, nil
}

func (w *rotatingSerialLog) rotate() error {
	for index := 0; index < w.maxBackups; index++ {
		if _, err := inspectSerialLogFile(fmt.Sprintf("%s.%d", w.path, index)); err != nil {
			return err
		}
	}
	if _, err := inspectSerialLogFile(w.path); err != nil {
		return err
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("serial log: close active log: %w", err)
	}
	w.file = nil
	if w.maxBackups > 0 {
		oldest := fmt.Sprintf("%s.%d", w.path, w.maxBackups-1)
		if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("serial log: remove oldest backup: %w", err)
		}
		for index := w.maxBackups - 2; index >= 0; index-- {
			source := fmt.Sprintf("%s.%d", w.path, index)
			destination := fmt.Sprintf("%s.%d", w.path, index+1)
			if err := os.Rename(source, destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("serial log: rotate backup: %w", err)
			}
		}
		if err := os.Rename(w.path, w.path+".0"); err != nil {
			return fmt.Errorf("serial log: rotate active log: %w", err)
		}
	}
	file, size, err := openSecureSerialLog(w.path)
	if err != nil {
		return err
	}
	w.file, w.size = file, size
	return nil
}

func (w *rotatingSerialLog) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

type serialLogFileInfo struct {
	exists bool
	size   int64
}
