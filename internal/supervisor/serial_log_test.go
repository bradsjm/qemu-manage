//go:build unix

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSerialLogFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
func privateSerialLogDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readSerialLogFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRotatingSerialLogWriteRetainsNewestBoundedBytes(t *testing.T) {
	dir := privateSerialLogDir(t)
	path := filepath.Join(dir, "serial.log")
	writer, err := openRotatingSerialLog(path, 8, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	input := []byte("AAAAAAAABBBBBBBBCCCCCCCCDDDDDDDDEEEEEEEE")
	n, err := writer.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Fatalf("written = %d, want %d", n, len(input))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	var combined []byte
	for _, name := range []string{"serial.log.2", "serial.log.1", "serial.log.0", "serial.log"} {
		filePath := filepath.Join(dir, name)
		info, err := os.Stat(filePath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 8 {
			t.Fatalf("%s size = %d, want <= 8", name, info.Size())
		}
		combined = append(combined, readSerialLogFixture(t, filePath)...)
	}
	if want := input[len(input)-32:]; !bytes.Equal(combined, want) {
		t.Fatalf("retained bytes = %q, want %q", combined, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "serial.log.3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serial.log.3 error = %v, want not exist", err)
	}
}

func TestOpenRotatingSerialLogCleansOversizedManagedFilesAtStartup(t *testing.T) {
	dir := privateSerialLogDir(t)
	path := filepath.Join(dir, "serial.log")
	writeSerialLogFixture(t, path, []byte("123456789"), 0o600)
	writeSerialLogFixture(t, path+".0", []byte("oversized"), 0o600)
	writeSerialLogFixture(t, path+".1", []byte("bounded!"), 0o600)
	writeSerialLogFixture(t, path+".3", []byte("stale"), 0o600)

	writer, err := openRotatingSerialLog(path, 8, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Size() != 0 {
		t.Fatalf("active size = %d, want 0", info.Size())
	}
	if got := readSerialLogFixture(t, path+".1"); !bytes.Equal(got, []byte("bounded!")) {
		t.Fatalf("serial.log.1 = %q, want %q", got, "bounded!")
	}
	for _, doomed := range []string{path + ".0", path + ".3"} {
		if _, err := os.Stat(doomed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s error = %v, want not exist", doomed, err)
		}
	}
}

func TestOpenRotatingSerialLogRejectsUnsafePaths(t *testing.T) {
	t.Run("active symlink", func(t *testing.T) {
		dir := privateSerialLogDir(t)
		target := filepath.Join(dir, "target.log")
		writeSerialLogFixture(t, target, []byte("target"), 0o600)
		path := filepath.Join(dir, "serial.log")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}

		writer, err := openRotatingSerialLog(path, 8, 3)
		if err == nil {
			_ = writer.Close()
			t.Fatal("openRotatingSerialLog unexpectedly succeeded")
		}
	})

	t.Run("managed backup wrong mode", func(t *testing.T) {
		dir := privateSerialLogDir(t)
		path := filepath.Join(dir, "serial.log")
		writeSerialLogFixture(t, path+".0", []byte("bounded!"), 0o644)

		writer, err := openRotatingSerialLog(path, 8, 3)
		if err == nil {
			_ = writer.Close()
			t.Fatal("openRotatingSerialLog unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), "want 0600") {
			t.Fatalf("error = %v, want mode validation", err)
		}
	})
}

type failingSerialLogWriter struct {
	writes int
	closed int
}

func (w *failingSerialLogWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, fmt.Errorf("write %d failed", w.writes)
}

func (w *failingSerialLogWriter) Close() error {
	w.closed++
	return nil
}

func TestCopySerialLogDisablesDurableWritesButDrainsInput(t *testing.T) {
	input := bytes.NewBuffer(bytes.Repeat([]byte("x"), serialLogBufferSize+17))
	output := &failingSerialLogWriter{}
	var warnings bytes.Buffer

	if err := copySerialLog(input, output, &warnings); err != nil {
		t.Fatal(err)
	}
	if input.Len() != 0 {
		t.Fatalf("input remaining = %d, want 0", input.Len())
	}
	if output.writes != 1 {
		t.Fatalf("writes = %d, want 1", output.writes)
	}
	if output.closed != 1 {
		t.Fatalf("closed = %d, want 1", output.closed)
	}
	warningText := warnings.String()
	if got := strings.Count(warningText, "serial log: durable logging disabled:"); got != 1 {
		t.Fatalf("warning count = %d, want 1; warnings = %q", got, warningText)
	}
}

func TestSerialLogSinkFinishCopiesFIFOIntoActiveLog(t *testing.T) {
	dir := privateSerialLogDir(t)
	pipePath := filepath.Join(dir, "serial-log.pipe")
	logPath := filepath.Join(dir, "serial.log")
	sink, err := startSerialLogSink(pipePath, logPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	producer, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("serial output\nsecond line\n")
	if _, err := producer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.finish(); err != nil {
		t.Fatal(err)
	}
	if got := readSerialLogFixture(t, logPath); !bytes.Equal(got, payload) {
		t.Fatalf("active log = %q, want %q", got, payload)
	}
}

func TestSerialLogSinkAbortReturnsWithoutProducerEOF(t *testing.T) {
	dir := privateSerialLogDir(t)
	pipePath := filepath.Join(dir, "serial-log.pipe")
	logPath := filepath.Join(dir, "serial.log")
	sink, err := startSerialLogSink(pipePath, logPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- sink.abort()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort blocked waiting for producer EOF")
	}
}
