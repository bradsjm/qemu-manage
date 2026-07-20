package cli

import (
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
)

func withProgress(output io.Writer, enabled, interactive bool, message string, total int64, units progress.Units, operation func(*progress.Tracker) error) error {
	if output == nil {
		output = io.Discard
	}
	if !enabled {
		return operation(nil)
	}
	if !interactive {
		_, _ = fmt.Fprintf(output, "%s...\n", message)
		err := operation(nil)
		if err == nil {
			_, _ = fmt.Fprintf(output, "%s complete\n", message)
		} else {
			_, _ = fmt.Fprintf(output, "%s failed: %v\n", message, err)
		}
		return err
	}

	if total < 0 {
		total = 0
	}
	tracker := &progress.Tracker{
		AutoStopDisabled: true,
		Message:          message,
		Total:            total,
		Units:            units,
	}
	writer := progress.NewWriter()
	writer.SetOutputWriter(output)
	writer.SetAutoStop(false)
	style := progress.StyleDefault
	style.Options.DoneString = "complete"
	writer.SetStyle(style)
	writer.SetUpdateFrequency(25 * time.Millisecond)
	writer.AppendTracker(tracker)
	renderDone := make(chan struct{})
	go func() {
		writer.Render()
		close(renderDone)
	}()
	for !writer.IsRenderInProgress() {
		runtime.Gosched()
	}

	err := operation(tracker)
	if err == nil {
		tracker.MarkAsDone()
	} else {
		tracker.MarkAsErrored()
	}
	writer.Stop()
	<-renderDone
	return err
}

func withWaitingProgress(output io.Writer, enabled, interactive bool, message string, operation func() error) error {
	return withProgress(output, enabled, interactive, message, 0, progress.UnitsDefault, func(_ *progress.Tracker) error {
		return operation()
	})
}

func copyWithProgress(input io.Reader, output io.Writer, total int64, tracker *progress.Tracker) error {
	if tracker == nil {
		_, err := io.Copy(output, input)
		return err
	}
	if total > 0 && tracker.Total == 0 {
		tracker.UpdateTotal(total)
	}
	_, err := io.Copy(progressWriter{output: output, tracker: tracker}, input)
	return err
}

// progressWriter forwards bytes to an output writer while reporting written
// progress to a tracker.
type progressWriter struct {
	output  io.Writer
	tracker *progress.Tracker
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	if n > 0 {
		w.tracker.Increment(int64(n))
	}
	return n, err
}
