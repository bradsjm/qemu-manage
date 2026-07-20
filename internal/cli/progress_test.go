package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

type recordingByteProgress struct {
	mu    sync.Mutex
	calls int
	total int
}

func (p *recordingByteProgress) Add(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.total += n
}

func (p *recordingByteProgress) snapshot() (calls int, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.total
}

type partialErrorByteWriter struct {
	bytes.Buffer
	limit int
	err   error
}

func (w *partialErrorByteWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		p = p[:w.limit]
	}
	n, _ := w.Buffer.Write(p)
	return n, w.err
}

type terminalSizeRecordingWriter struct {
	bytes.Buffer
	width     int
	height    int
	err       error
	sizeCalls int
}

func (w *terminalSizeRecordingWriter) terminalOutputSize() (int, int, error) {
	w.sizeCalls++
	return w.width, w.height, w.err
}

func (w *terminalSizeRecordingWriter) SizeCalls() int {
	return w.sizeCalls
}

func assertNoTerminalControlBytes(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, "\x1b") {
		t.Fatalf("output contains ANSI escape sequences: %q", output)
	}
	if strings.Contains(output, "\r") {
		t.Fatalf("output contains carriage returns: %q", output)
	}
}

func TestWithByteProgressDisabledPassesNilAndStaysSilent(t *testing.T) {
	var output bytes.Buffer
	wantErr := errors.New("operation failed")
	calls := 0

	gotErr := withByteProgress(&output, false, true, "Downloading image", "Downloaded image", 64, func(progress byteProgress) error {
		calls++
		if progress != nil {
			t.Fatal("disabled byte progress passed a non-nil progress reporter")
		}
		return wantErr
	})
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error=%v, want %v", gotErr, wantErr)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if output.Len() != 0 {
		t.Fatalf("disabled byte progress wrote %q", output.String())
	}
}

func TestWithWaitingProgressDisabledStaysSilentAndPreservesError(t *testing.T) {
	var output bytes.Buffer
	wantErr := errors.New("wait failed")
	calls := 0

	gotErr := withWaitingProgress(&output, false, true, "Starting VM", "Started VM", func() error {
		calls++
		return wantErr
	})
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error=%v, want %v", gotErr, wantErr)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if output.Len() != 0 {
		t.Fatalf("disabled waiting progress wrote %q", output.String())
	}
}

func TestWithWaitingProgressRedirectedFeedbackIsANSIFreeAndPreservesOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		workErr error
	}{
		{name: "success"},
		{name: "failure", workErr: errors.New("disk failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			gotErr := withWaitingProgress(&output, true, false, "Creating primary disk", "Created primary disk", func() error {
				return tc.workErr
			})
			if !errors.Is(gotErr, tc.workErr) {
				t.Fatalf("error=%v, want %v", gotErr, tc.workErr)
			}
			if output.Len() == 0 {
				t.Fatal("redirected waiting progress wrote nothing")
			}
			got := output.String()
			assertNoTerminalControlBytes(t, got)
			if !strings.Contains(got, "Creating primary disk") {
				t.Fatalf("output=%q, want start message", got)
			}
			if tc.workErr != nil && !strings.Contains(got, tc.workErr.Error()) {
				t.Fatalf("output=%q, want failure detail %q", got, tc.workErr)
			}
			if tc.workErr == nil && !strings.Contains(got, "Created primary disk") {
				t.Fatalf("output=%q, want success message", got)
			}
		})
	}
}

func TestWithByteProgressRedirectedFeedbackIsANSIFreeAndPreservesOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		workErr error
	}{
		{name: "success"},
		{name: "failure", workErr: errors.New("download failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			calls := 0
			gotErr := withByteProgress(&output, true, false, "Downloading image", "Downloaded image", 128, func(progress byteProgress) error {
				calls++
				if progress != nil {
					t.Fatal("redirected byte progress passed a non-nil progress reporter")
				}
				return tc.workErr
			})
			if !errors.Is(gotErr, tc.workErr) {
				t.Fatalf("error=%v, want %v", gotErr, tc.workErr)
			}
			if calls != 1 {
				t.Fatalf("calls=%d, want 1", calls)
			}
			if output.Len() == 0 {
				t.Fatal("redirected byte progress wrote nothing")
			}
			got := output.String()
			assertNoTerminalControlBytes(t, got)
			if !strings.Contains(got, "Downloading image") {
				t.Fatalf("output=%q, want start message", got)
			}
			if tc.workErr != nil && !strings.Contains(got, tc.workErr.Error()) {
				t.Fatalf("output=%q, want failure detail %q", got, tc.workErr)
			}
			if tc.workErr == nil && !strings.Contains(got, "Downloaded image") {
				t.Fatalf("output=%q, want success message", got)
			}
		})
	}
}

func TestWithWaitingProgressTerminalCompletes(t *testing.T) {
	var output bytes.Buffer
	calls := 0

	if err := withWaitingProgress(&output, true, true, "Starting VM", "VM is ready", func() error {
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if output.Len() == 0 {
		t.Fatal("terminal waiting progress wrote nothing")
	}
}

func TestWithByteProgressTerminalCompletesAndCountsBytes(t *testing.T) {
	var output bytes.Buffer
	calls := 0

	if err := withByteProgress(&output, true, true, "Copying image", "Copied image", 4, func(progress byteProgress) error {
		calls++
		if progress == nil {
			t.Fatal("terminal byte progress passed nil progress reporter")
		}
		progress.Add(1)
		progress.Add(3)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if output.Len() == 0 {
		t.Fatal("terminal byte progress wrote nothing")
	}
}

func TestPresentationWriterDelegatesTerminalSizeCapability(t *testing.T) {
	output := &terminalSizeRecordingWriter{width: 23, height: 7}
	writer := newPresentationWriter(output, true)

	width, height, err := writer.terminalOutputSize()
	if err != nil {
		t.Fatalf("terminalOutputSize returned error: %v", err)
	}
	if width != 23 || height != 7 {
		t.Fatalf("terminalOutputSize=(%d,%d), want (23,7)", width, height)
	}
	if output.SizeCalls() != 1 {
		t.Fatalf("sizeCalls=%d, want 1", output.SizeCalls())
	}
}

func TestByteBarWidthUsesWrappedTerminalSizeCapability(t *testing.T) {
	output := &terminalSizeRecordingWriter{width: 24, height: 7}
	bar := newByteBar(newPresentationWriter(output, true), "Copying image", 100)
	defer func() {
		if err := bar.Stop(); err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	}()

	bar.mu.Lock()
	bar.maxWidth = 80
	bar.showTitle = false
	bar.showPercentage = false
	bar.showElapsedTime = false
	bar.barCharacter = "#"
	bar.lastCharacter = ""
	bar.barFiller = "."
	bar.current = 50
	width := bar.widthLocked()
	frame := bar.frameLocked()
	bar.mu.Unlock()

	if width != 24 {
		t.Fatalf("widthLocked=%d, want 24", width)
	}
	if got := visibleWidth(frame); got != 24 {
		t.Fatalf("visible frame width=%d, want 24", got)
	}
	if output.SizeCalls() == 0 {
		t.Fatal("wrapped terminal size capability was never consulted")
	}
}

func TestByteBarAddAndStopAreSafeAndIdempotent(t *testing.T) {
	var output bytes.Buffer
	bar := newByteBar(&output, "Copying image", 4)
	initial := output.Len()
	if initial == 0 {
		t.Fatal("new byte bar did not render an initial frame")
	}

	bar.Add(1)
	bar.Add(3)
	if output.Len() <= initial {
		t.Fatalf("byte bar did not render progress updates: initial=%d current=%d", initial, output.Len())
	}

	if err := bar.Stop(); err != nil {
		t.Fatalf("first Stop returned error: %v", err)
	}
	stoppedOutput := output.String()

	if err := bar.Stop(); err != nil {
		t.Fatalf("second Stop returned error: %v", err)
	}
	if output.String() != stoppedOutput {
		t.Fatal("second Stop changed output after the bar was already stopped")
	}

	bar.Add(1)
	if output.String() != stoppedOutput {
		t.Fatal("Add after Stop changed output")
	}
}

func TestTerminalProgressAssignedToStderrLeavesStdoutEmpty(t *testing.T) {
	if os.Getenv("QEMU_MANAGE_PROGRESS_STDERR_HELPER") == "1" {
		if err := withWaitingProgress(os.Stderr, true, true, "Starting VM", "VM is ready", func() error {
			return nil
		}); err != nil {
			panic(err)
		}
		if err := withByteProgress(os.Stderr, true, true, "Copying disk", "Copied disk", 3, func(progress byteProgress) error {
			if progress == nil {
				return errors.New("missing byte progress reporter")
			}
			progress.Add(1)
			progress.Add(2)
			return nil
		}); err != nil {
			panic(err)
		}
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminalProgressAssignedToStderrLeavesStdoutEmpty$")
	cmd.Env = append(os.Environ(), "QEMU_MANAGE_PROGRESS_STDERR_HELPER=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper process failed: %v\nstderr=%q", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q, want empty stdout when progress targets stderr", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr was empty; helper did not exercise terminal progress")
	}
}

func TestCopyWithProgressCopiesPayloadAndCountsExactBytes(t *testing.T) {
	var output bytes.Buffer
	progress := &recordingByteProgress{}
	payload := "abcdef"

	if err := copyWithProgress(strings.NewReader(payload), &output, progress); err != nil {
		t.Fatal(err)
	}
	if output.String() != payload {
		t.Fatalf("output=%q, want %q", output.String(), payload)
	}
	calls, total := progress.snapshot()
	if calls == 0 {
		t.Fatal("progress reporter was never updated")
	}
	if total != len(payload) {
		t.Fatalf("progress total=%d, want %d", total, len(payload))
	}
}

func TestCopyWithProgressCountsPartialWritesBeforeError(t *testing.T) {
	writer := &partialErrorByteWriter{limit: 2, err: io.ErrShortWrite}
	progress := &recordingByteProgress{}

	gotErr := copyWithProgress(strings.NewReader("abcdef"), writer, progress)
	if !errors.Is(gotErr, io.ErrShortWrite) {
		t.Fatalf("error=%v, want %v", gotErr, io.ErrShortWrite)
	}
	if writer.String() != "ab" {
		t.Fatalf("output=%q, want %q", writer.String(), "ab")
	}
	calls, total := progress.snapshot()
	if calls == 0 {
		t.Fatal("progress reporter was never updated")
	}
	if total != 2 {
		t.Fatalf("progress total=%d, want 2", total)
	}
}
