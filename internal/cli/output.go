package cli

import (
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pterm/pterm"
	"golang.org/x/term"
)

type messageLevel uint8

const (
	messageInfo messageLevel = iota
	messageSuccess
	messageWarning
	messageError
)

type presentationWriter struct {
	output      io.Writer
	interactive bool
	mu          sync.Mutex
}

func newPresentationWriter(output io.Writer, interactive bool) *presentationWriter {
	return &presentationWriter{
		output:      normalizeOutput(output),
		interactive: interactive,
	}
}

func (w *presentationWriter) Write(p []byte) (int, error) {
	if w == nil {
		return io.Discard.Write(p)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	return w.output.Write(p)
}

type terminalOutputCarrier interface {
	terminalOutputInteractive() bool
}

var errTerminalSizeUnavailable = errors.New("terminal size unavailable")

func (w *presentationWriter) terminalOutputInteractive() bool {
	return w != nil && w.interactive
}

func (w *presentationWriter) terminalOutputSize() (width int, height int, err error) {
	if w == nil {
		return 0, 0, errTerminalSizeUnavailable
	}
	if sized, ok := w.output.(terminalSizeCarrier); ok {
		return sized.terminalOutputSize()
	}
	fd, ok := w.output.(interface{ Fd() uintptr })
	if !ok {
		return 0, 0, errTerminalSizeUnavailable
	}
	return term.GetSize(int(fd.Fd()))
}

func writeMessage(output io.Writer, interactive bool, level messageLevel, message string) error {
	output = normalizeOutput(output)
	message = normalizeMessage(message)
	if interactive {
		printer := prefixPrinterForLevel(level)
		_, err := io.WriteString(output, printer.Sprintln(message))
		return err
	}

	_, err := io.WriteString(output, redirectedMessagePrefix(level)+": "+message+"\n")
	return err
}

func ptermWriter(output io.Writer, interactive bool) io.Writer {
	output = normalizeOutput(output)
	if interactive {
		return output
	}
	return ansiStrippingWriter{output: output}
}

type ansiStrippingWriter struct {
	output io.Writer
}

func (w ansiStrippingWriter) Write(p []byte) (int, error) {
	output := normalizeOutput(w.output)
	clean := pterm.RemoveColorFromString(string(p))
	if clean == "" {
		return len(p), nil
	}

	n, err := io.WriteString(output, clean)
	if err == nil && n == len(clean) {
		return len(p), nil
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

type liveStatus struct {
	output      io.Writer
	enabled     bool
	interactive bool
	theme       spinnerTheme
	stop        chan struct{}
	done        chan struct{}
	request     chan struct{}
	resolveOnce sync.Once
	stateMu     sync.Mutex
	outputMu    sync.Mutex
	message     string
	startedAt   time.Time
	sequenceIdx int
	resolved    bool
}

func startLiveStatus(output io.Writer, enabled, interactive bool, message string) *liveStatus {
	status := &liveStatus{
		output:      normalizeOutput(output),
		enabled:     enabled,
		interactive: interactive,
		message:     normalizeMessage(message),
	}
	if !enabled {
		return status
	}
	if !interactive {
		status.writeRaw(status.message + "...\n")
		return status
	}

	status.theme = defaultSpinnerTheme()
	status.stop = make(chan struct{})
	status.done = make(chan struct{})
	status.request = make(chan struct{}, 1)
	status.startedAt = time.Now()
	go status.runTerminal()
	return status
}

func (s *liveStatus) Update(message string) {
	if s == nil || !s.enabled {
		return
	}

	message = normalizeMessage(message)

	s.stateMu.Lock()
	if s.resolved || message == s.message {
		s.stateMu.Unlock()
		return
	}
	s.message = message
	interactive := s.interactive
	s.stateMu.Unlock()

	if interactive {
		s.requestRender()
		return
	}

	s.writeRaw(message + "...\n")
}

func (s *liveStatus) Success(message string) {
	s.resolve(messageSuccess, message)
}

func (s *liveStatus) Info(message string) {
	s.resolve(messageInfo, message)
}

func (s *liveStatus) Warning(message string) {
	s.resolve(messageWarning, message)
}

func (s *liveStatus) Fail(message string) {
	s.resolve(messageError, message)
}

func (s *liveStatus) resolve(level messageLevel, message string) {
	if s == nil {
		return
	}

	message = normalizeMessage(message)

	s.resolveOnce.Do(func() {
		s.stateMu.Lock()
		s.message = message
		s.resolved = true
		interactive := s.interactive
		enabled := s.enabled
		s.stateMu.Unlock()

		if !enabled {
			return
		}
		if !interactive {
			_ = writeMessage(s.output, false, level, message)
			return
		}

		close(s.stop)
		<-s.done

		s.outputMu.Lock()
		defer s.outputMu.Unlock()
		_, _ = io.WriteString(s.output, "\r\033[K")
		_ = writeMessage(s.output, true, level, message)
	})
}

func (s *liveStatus) runTerminal() {
	defer close(s.done)

	delay := s.theme.delay
	if delay <= 0 {
		delay = time.Millisecond
	}

	ticker := time.NewTicker(delay)
	defer ticker.Stop()

	s.renderTerminal()
	for {
		select {
		case <-ticker.C:
			s.renderTerminal()
		case <-s.request:
			s.renderTerminal()
		case <-s.stop:
			return
		}
	}
}

func (s *liveStatus) renderTerminal() {
	if s == nil {
		return
	}

	frame, message, timer, ok := s.nextTerminalFrame()
	if !ok {
		return
	}

	var builder strings.Builder
	builder.WriteString("\r\033[K")
	builder.WriteString(applyStyle(s.theme.style, frame))
	builder.WriteByte(' ')
	builder.WriteString(applyStyle(s.theme.messageStyle, message))
	if timer != "" {
		builder.WriteString(applyStyle(s.theme.timerStyle, timer))
	}

	s.writeRaw(builder.String())
}

func (s *liveStatus) nextTerminalFrame() (string, string, string, bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if s.resolved {
		return "", "", "", false
	}

	frame := ""
	if len(s.theme.sequence) > 0 {
		frame = s.theme.sequence[s.sequenceIdx%len(s.theme.sequence)]
		s.sequenceIdx++
	}

	timer := ""
	if s.theme.showTimer {
		elapsed := time.Since(s.startedAt)
		if s.theme.timerRoundingFactor > 0 {
			elapsed = elapsed.Round(s.theme.timerRoundingFactor)
		}
		timer = " (" + elapsed.String() + ")"
	}

	return frame, s.message, timer, true
}

func (s *liveStatus) requestRender() {
	if s == nil || s.request == nil {
		return
	}

	select {
	case s.request <- struct{}{}:
	default:
	}
}

func (s *liveStatus) writeRaw(message string) {
	if s == nil || message == "" {
		return
	}

	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	_, _ = io.WriteString(s.output, message)
}

type spinnerTheme struct {
	sequence            []string
	delay               time.Duration
	style               *pterm.Style
	messageStyle        *pterm.Style
	showTimer           bool
	timerRoundingFactor time.Duration
	timerStyle          *pterm.Style
}

func defaultSpinnerTheme() spinnerTheme {
	spinner := pterm.DefaultSpinner
	sequence := append([]string(nil), spinner.Sequence...)
	if len(sequence) == 0 {
		sequence = []string{""}
	}
	return spinnerTheme{
		sequence:            sequence,
		delay:               spinner.Delay,
		style:               cloneStyle(spinner.Style),
		messageStyle:        cloneStyle(spinner.MessageStyle),
		showTimer:           spinner.ShowTimer,
		timerRoundingFactor: spinner.TimerRoundingFactor,
		timerStyle:          cloneStyle(spinner.TimerStyle),
	}
}

func cloneStyle(style *pterm.Style) *pterm.Style {
	if style == nil {
		return nil
	}

	copyStyle := append(pterm.Style(nil), (*style)...)
	return &copyStyle
}

func clonePrefixPrinter(printer pterm.PrefixPrinter) pterm.PrefixPrinter {
	printer.MessageStyle = cloneStyle(printer.MessageStyle)
	printer.Prefix.Style = cloneStyle(printer.Prefix.Style)
	printer.Scope.Style = cloneStyle(printer.Scope.Style)
	printer.Writer = nil
	return printer
}

func prefixPrinterForLevel(level messageLevel) pterm.PrefixPrinter {
	switch level {
	case messageSuccess:
		return clonePrefixPrinter(pterm.Success)
	case messageWarning:
		return clonePrefixPrinter(pterm.Warning)
	case messageError:
		return clonePrefixPrinter(pterm.Error)
	case messageInfo:
		fallthrough
	default:
		return clonePrefixPrinter(pterm.Info)
	}
}

func redirectedMessagePrefix(level messageLevel) string {
	switch level {
	case messageSuccess:
		return "SUCCESS"
	case messageWarning:
		return "WARNING"
	case messageError:
		return "ERROR"
	case messageInfo:
		fallthrough
	default:
		return "INFO"
	}
}

func normalizeOutput(output io.Writer) io.Writer {
	if output == nil {
		return io.Discard
	}
	return output
}

func normalizeMessage(message string) string {
	return strings.TrimRight(message, "\n")
}

func applyStyle(style *pterm.Style, text string) string {
	if style == nil || text == "" {
		return text
	}
	return style.Sprint(text)
}

func (a *App) liveProgressInteractive(output io.Writer) bool {
	if a == nil || a.debug {
		return false
	}
	return a.progressInteractive(output)
}
