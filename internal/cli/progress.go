package cli

import (
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pterm/pterm"
	"golang.org/x/term"
)

type byteProgress interface {
	Add(int)
}

type terminalSizeCarrier interface {
	terminalOutputSize() (width int, height int, err error)
}

type byteBar struct {
	output                    io.Writer
	title                     string
	total                     int64
	current                   int64
	startedAt                 time.Time
	titleStyle                *pterm.Style
	barStyle                  *pterm.Style
	barCharacter              string
	lastCharacter             string
	barFiller                 string
	showTitle                 bool
	showPercentage            bool
	showElapsedTime           bool
	maxWidth                  int
	elapsedTimeRoundingFactor time.Duration

	mu      sync.Mutex
	stopped bool
	err     error
}

func newByteBar(output io.Writer, title string, total int64) *byteBar {
	if output == nil {
		output = io.Discard
	}
	defaults := pterm.DefaultProgressbar
	bar := &byteBar{
		output:                    output,
		title:                     title,
		total:                     total,
		startedAt:                 time.Now(),
		titleStyle:                defaults.TitleStyle,
		barStyle:                  defaults.BarStyle,
		barCharacter:              defaults.BarCharacter,
		lastCharacter:             defaults.LastCharacter,
		barFiller:                 defaults.BarFiller,
		showTitle:                 defaults.ShowTitle,
		showPercentage:            defaults.ShowPercentage,
		showElapsedTime:           defaults.ShowElapsedTime,
		maxWidth:                  defaults.MaxWidth,
		elapsedTimeRoundingFactor: defaults.ElapsedTimeRoundingFactor,
	}
	bar.render()
	return bar
}

func (b *byteBar) Add(count int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.current += int64(count)
	b.renderLocked()
}

func (b *byteBar) Stop() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return b.err
	}
	b.stopped = true
	b.writeLocked("\r\033[K")
	return b.err
}

func (b *byteBar) render() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.renderLocked()
}

func (b *byteBar) renderLocked() {
	if b.stopped || b.total <= 0 {
		return
	}
	b.writeLocked("\r\033[K" + b.frameLocked())
}

func (b *byteBar) writeLocked(text string) {
	if b.err != nil {
		return
	}
	if _, err := io.WriteString(b.output, text); err != nil {
		b.err = err
	}
}

func (b *byteBar) frameLocked() string {
	titleStyle := b.titleStyle
	if titleStyle == nil {
		titleStyle = pterm.NewStyle()
	}
	barStyle := b.barStyle
	if barStyle == nil {
		barStyle = pterm.NewStyle()
	}

	var prefix string
	if b.showTitle && b.title != "" {
		prefix = titleStyle.Sprint(b.title) + " "
	}

	displayCurrent := b.current
	if displayCurrent < 0 {
		displayCurrent = 0
	}
	if displayCurrent > b.total {
		displayCurrent = b.total
	}

	ratio := float64(displayCurrent) / float64(b.total)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}

	var suffix strings.Builder
	if b.showPercentage {
		suffix.WriteString(" ")
		suffix.WriteString(fmt.Sprintf("%3d%%", int(math.Round(ratio*100))))
	}
	if b.showElapsedTime {
		elapsed := time.Since(b.startedAt)
		if b.elapsedTimeRoundingFactor > 0 {
			elapsed = elapsed.Round(b.elapsedTimeRoundingFactor)
		}
		if suffix.Len() == 0 {
			suffix.WriteString(" ")
		} else {
			suffix.WriteString(" | ")
		}
		suffix.WriteString(elapsed.String())
	}

	barWidth := b.widthLocked() - visibleWidth(prefix) - visibleWidth(suffix.String())
	if barWidth < 10 {
		barWidth = 10
	}

	filledWidth := ratio * float64(barWidth)
	fullCells := int(math.Floor(filledWidth))
	edgeCells := 0
	if filledWidth > float64(fullCells) && fullCells < barWidth {
		edgeCells = 1
	}
	emptyCells := barWidth - fullCells - edgeCells
	if emptyCells < 0 {
		emptyCells = 0
	}

	var bar strings.Builder
	if fullCells > 0 || edgeCells > 0 {
		filled := strings.Repeat(b.barCharacter, fullCells)
		if edgeCells == 1 {
			filled += b.lastCharacter
		}
		bar.WriteString(barStyle.Sprint(filled))
	}
	if emptyCells > 0 {
		bar.WriteString(strings.Repeat(b.barFiller, emptyCells))
	}

	return prefix + bar.String() + suffix.String()
}

func (b *byteBar) widthLocked() int {
	if sized, ok := b.output.(terminalSizeCarrier); ok {
		width, _, err := sized.terminalOutputSize()
		if err == nil && width > 0 {
			if b.maxWidth > 0 && width > b.maxWidth {
				return b.maxWidth
			}
			return width
		}
	}
	if fd, ok := b.output.(interface{ Fd() uintptr }); ok {
		width, _, err := term.GetSize(int(fd.Fd()))
		if err == nil && width > 0 {
			if b.maxWidth > 0 && width > b.maxWidth {
				return b.maxWidth
			}
			return width
		}
	}
	return b.maxWidth
}

func visibleWidth(text string) int {
	return utf8.RuneCountInString(pterm.RemoveColorFromString(text))
}

func withByteProgress(
	output io.Writer,
	enabled, interactive bool,
	startMessage, successMessage string,
	total int64,
	operation func(byteProgress) error,
) error {
	if output == nil {
		output = io.Discard
	}
	if !enabled {
		return operation(nil)
	}
	if !interactive {
		status := startLiveStatus(output, true, false, startMessage)
		err := operation(nil)
		if err != nil {
			status.Fail(startMessage + " failed: " + err.Error())
			return err
		}
		status.Success(successMessage)
		return nil
	}
	if total <= 0 {
		status := startLiveStatus(output, true, true, startMessage)
		err := operation(nil)
		if err != nil {
			status.Fail(startMessage + " failed: " + err.Error())
			return err
		}
		status.Success(successMessage)
		return nil
	}

	bar := newByteBar(output, startMessage, total)
	err := operation(bar)
	progressErr := bar.Stop()
	if err != nil {
		_ = writeMessage(output, true, messageError, startMessage+" failed: "+err.Error())
		return err
	}
	if progressErr != nil {
		return progressErr
	}
	if err := writeMessage(output, true, messageSuccess, successMessage); err != nil {
		return err
	}
	return nil
}

func withWaitingProgress(
	output io.Writer,
	enabled, interactive bool,
	startMessage, successMessage string,
	operation func() error,
) error {
	return withByteProgress(output, enabled, interactive, startMessage, successMessage, 0, func(_ byteProgress) error {
		return operation()
	})
}

func copyWithProgress(input io.Reader, output io.Writer, progress byteProgress) error {
	if progress == nil {
		_, err := io.Copy(output, input)
		return err
	}
	_, err := io.Copy(progressWriter{output: output, progress: progress}, input)
	return err
}

type progressWriter struct {
	output   io.Writer
	progress byteProgress
}

func (w progressWriter) Write(buffer []byte) (int, error) {
	n, err := w.output.Write(buffer)
	if n > 0 {
		w.progress.Add(n)
	}
	return n, err
}
