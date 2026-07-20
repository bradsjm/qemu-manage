package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pterm/pterm"
)

type failingOutputWriter struct {
	err error
}

func (w failingOutputWriter) Write([]byte) (int, error) {
	return 0, w.err
}

type gatedOutputWriter struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	writes  int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedOutputWriter() *gatedOutputWriter {
	return &gatedOutputWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *gatedOutputWriter) Write(p []byte) (int, error) {
	blocked := false
	w.once.Do(func() {
		blocked = true
		close(w.started)
	})
	if blocked {
		<-w.release
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	return w.buffer.Write(p)
}

func (w *gatedOutputWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func (w *gatedOutputWriter) WriteCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

type recordingOutputWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	chunks []string
}

func (w *recordingOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.chunks = append(w.chunks, string(p))
	return w.buffer.Write(p)
}

func (w *recordingOutputWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func (w *recordingOutputWriter) Chunks() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.chunks...)
}

func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestWriteMessageNilWriterIsSafe(t *testing.T) {
	for _, level := range []messageLevel{messageInfo, messageSuccess, messageWarning, messageError} {
		if err := writeMessage(nil, false, level, "plain message"); err != nil {
			t.Fatalf("level=%v returned error for nil redirected writer: %v", level, err)
		}
		if err := writeMessage(nil, true, level, "terminal message"); err != nil {
			t.Fatalf("level=%v returned error for nil terminal writer: %v", level, err)
		}
	}
}

func TestWriteMessagePropagatesWriterErrors(t *testing.T) {
	wantErr := errors.New("write failed")
	for _, interactive := range []bool{false, true} {
		if err := writeMessage(failingOutputWriter{err: wantErr}, interactive, messageInfo, "message"); !errors.Is(err, wantErr) {
			t.Fatalf("interactive=%v error=%v, want %v", interactive, err, wantErr)
		}
	}
}

func TestPresentationWriterHandlesNilAndPropagatesErrors(t *testing.T) {
	writer := newPresentationWriter(nil, true)
	n, err := writer.Write([]byte("abc"))
	if err != nil {
		t.Fatalf("nil writer returned error: %v", err)
	}
	if n != 3 {
		t.Fatalf("nil writer wrote %d bytes, want 3", n)
	}

	writer = newPresentationWriter(failingOutputWriter{err: io.ErrClosedPipe}, false)
	n, err = writer.Write([]byte("abc"))
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error=%v, want %v", err, io.ErrClosedPipe)
	}
	if n != 0 {
		t.Fatalf("error writer reported %d bytes, want 0", n)
	}
}

func TestPtermWriterTerminalPassThroughAndRedirectedStripping(t *testing.T) {
	terminal := &bytes.Buffer{}
	if got := ptermWriter(terminal, true); got != terminal {
		t.Fatalf("interactive writer=%T, want original writer", got)
	}

	redirected := &bytes.Buffer{}
	writer := ptermWriter(redirected, false)
	input := "\x1b[31mred\x1b[0m plain"
	n, err := writer.Write([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Fatalf("Write returned %d, want %d", n, len(input))
	}
	if redirected.String() != "red plain" {
		t.Fatalf("redirected output=%q, want %q", redirected.String(), "red plain")
	}

	nilWriter := ptermWriter(nil, false)
	n, err = nilWriter.Write([]byte(input))
	if err != nil {
		t.Fatalf("nil redirected writer returned error: %v", err)
	}
	if n != len(input) {
		t.Fatalf("nil redirected writer wrote %d bytes, want %d", n, len(input))
	}
}

func TestProgressInteractiveUsesPresentationWriterTerminalBit(t *testing.T) {
	a := &App{
		IsTerminalOutput: func(io.Writer) bool { return false },
	}
	if !a.progressInteractive(newPresentationWriter(io.Discard, true)) {
		t.Fatal("progressInteractive did not honor presentationWriter terminal bit")
	}
	if a.progressInteractive(newPresentationWriter(io.Discard, false)) {
		t.Fatal("progressInteractive reported noninteractive presentationWriter as interactive")
	}
}

func TestDebugModeProgressGatingProducesSerializedPlainWrites(t *testing.T) {
	output := &recordingOutputWriter{}
	stderr := newPresentationWriter(output, true)
	a := &App{
		IsTerminalOutput: func(io.Writer) bool { return false },
		debug:            true,
	}
	if !a.progressInteractive(stderr) {
		t.Fatal("presentation writer terminal bit was not preserved")
	}
	if a.liveProgressInteractive(stderr) {
		t.Fatal("debug mode should disable live terminal progress")
	}

	a.debugWriter = ptermWriter(stderr, a.progressInteractive(stderr))
	a.debugLogger = pterm.DefaultLogger.WithWriter(a.debugWriter).WithTime(false).WithLevel(pterm.LogLevelDebug)

	if err := withWaitingProgress(stderr, true, a.liveProgressInteractive(stderr), "Starting work", "Finished work", func() error {
		a.debugf("diagnostic token")
		return nil
	}); err != nil {
		t.Fatalf("withWaitingProgress returned error: %v", err)
	}

	joined := output.String()
	if strings.Contains(joined, "\r") {
		t.Fatalf("debug-gated progress wrote carriage returns: %q", joined)
	}
	startIndex := strings.Index(joined, "Starting work...")
	debugIndex := strings.Index(joined, "diagnostic token")
	successIndex := strings.Index(joined, "Finished work")
	if startIndex < 0 || debugIndex < 0 || successIndex < 0 || !(startIndex < debugIndex && debugIndex < successIndex) {
		t.Fatalf("writes were not ordered start->debug->success: %q", joined)
	}

	for _, chunk := range output.Chunks() {
		if strings.Contains(chunk, "\r") {
			t.Fatalf("chunk contains carriage return frame: %q", chunk)
		}
		markers := 0
		for _, marker := range []string{"Starting work...", "diagnostic token", "Finished work"} {
			if strings.Contains(chunk, marker) {
				markers++
			}
		}
		if markers > 1 {
			t.Fatalf("chunk merged overlapping writes: %q", chunk)
		}
	}
}

func TestLiveStatusDisabledIsSilent(t *testing.T) {
	var output bytes.Buffer
	status := startLiveStatus(&output, false, true, "starting")
	status.Update("updated")
	status.Success("done")
	status.Info("info")
	status.Warning("warning")
	status.Fail("failed")
	if output.Len() != 0 {
		t.Fatalf("disabled live status wrote %q", output.String())
	}
}

func TestLiveStatusRedirectedWritesChangedMessagesWithoutControlBytes(t *testing.T) {
	var output bytes.Buffer
	status := startLiveStatus(&output, true, false, "starting")
	status.Update("starting")
	status.Update("working")
	status.Update("working")
	status.Success("done")

	got := output.String()
	assertNoTerminalControlBytes(t, got)
	if strings.Count(got, "starting...") != 1 {
		t.Fatalf("output=%q, want one starting line", got)
	}
	if strings.Count(got, "working...") != 1 {
		t.Fatalf("output=%q, want one changed update line", got)
	}
	if !strings.Contains(got, "done") {
		t.Fatalf("output=%q, want resolution message", got)
	}
}

func TestLiveStatusConcurrentUpdateAndResolutionDoNotDeadlock(t *testing.T) {
	writer := newGatedOutputWriter()
	status := startLiveStatus(writer, true, true, "starting")
	waitForSignal(t, writer.started, "initial render")

	updateDone := make([]chan struct{}, 3)
	for i, message := range []string{"working", "still working", "almost done"} {
		updateDone[i] = make(chan struct{})
		go func(done chan struct{}, message string) {
			defer close(done)
			status.Update(message)
		}(updateDone[i], message)
	}
	for i, done := range updateDone {
		waitForSignal(t, done, "update return "+string(rune('1'+i)))
	}

	resolveDone := make([]chan struct{}, 4)
	resolvers := []func(string){status.Success, status.Info, status.Warning, status.Fail}
	for i, resolve := range resolvers {
		resolveDone[i] = make(chan struct{})
		go func(done chan struct{}, resolve func(string), message string) {
			defer close(done)
			resolve(message)
		}(resolveDone[i], resolve, "finished")
	}

	close(writer.release)
	for i, done := range resolveDone {
		waitForSignal(t, done, "resolution return "+string(rune('1'+i)))
	}
	if writer.WriteCount() == 0 {
		t.Fatal("live status never wrote terminal output")
	}

	writesAfterResolution := writer.WriteCount()
	status.Update("after resolution")
	status.Success("resolved twice")
	status.Fail("resolved twice")
	if writer.WriteCount() != writesAfterResolution {
		t.Fatalf("live status wrote after resolution: before=%d after=%d", writesAfterResolution, writer.WriteCount())
	}
	if writer.String() == "" {
		t.Fatal("writer captured no terminal output")
	}
}
