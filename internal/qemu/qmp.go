package qemu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
)

const qmpOperationTimeout = 15 * time.Second

// QMPError is an error returned by QEMU. Data contains the complete structured
// error object supplied by QMP; callers need not interpret its description.
type QMPError struct {
	Class       string
	Description string
	Data        json.RawMessage
}

// Error returns the structured QMP error in printable form.
func (e *QMPError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("QMP error %q", e.Class)
	}
	return fmt.Sprintf("QMP error %q: %s", e.Class, e.Description)
}

// UnexpectedStatusError reports a live QMP state which qemu-manage does not
// model. Status is the unmodified value returned by query-status.
type UnexpectedStatusError struct {
	Status string
}

// Error reports the unexpected status string returned by QMP.
func (e *UnexpectedStatusError) Error() string {
	return fmt.Sprintf("unexpected QMP status %q", e.Status)
}

// VNCInfo is the validated subset of query-vnc returned to callers.
type VNCInfo struct {
	Enabled bool
	Host    string
	Service string
	Family  string
	Auth    string
}

type qmpVNCInfo struct {
	Enabled bool              `json:"enabled"`
	Host    string            `json:"host"`
	Service string            `json:"service"`
	Family  string            `json:"family"`
	Auth    string            `json:"auth"`
	Clients []json.RawMessage `json:"clients"`
}

// QMPClient is a persistent, synchronous QMP connection. All commands,
// including their responses, are serialized by gate. Unlike a mutex, gate
// allows a command's context to expire while it is waiting for the stream.
type QMPClient struct {
	gate               chan struct{}
	conn               net.Conn
	dec                *json.Decoder
	nextID             int64
	closed             bool
	version            backend.QEMUVersion
	lifecycleEvents    map[string]uint64
	blockIOErrorEvents map[qmpBlockIOErrorKey]uint64
}

type qmpGreeting struct {
	QMP struct {
		Version struct {
			QEMU struct {
				Major int `json:"major"`
				Minor int `json:"minor"`
				Micro int `json:"micro"`
			} `json:"qemu"`
			Package string `json:"package"`
		} `json:"version"`
		Capabilities []string `json:"capabilities"`
	} `json:"QMP"`
}

// qmpDeviceLabelPattern bounds the device labels we accept from monitoring
// responses and events to QEMU's expected identifier shape.
var qmpDeviceLabelPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// qmpLifecycleEventNames is the fixed, bounded monitoring allowlist. Unknown
// QMP events are intentionally ignored so snapshots cannot grow arbitrary
// labels from server-defined names.
var qmpLifecycleEventNames = map[string]string{
	"SHUTDOWN":       "shutdown",
	"POWERDOWN":      "powerdown",
	"RESET":          "reset",
	"STOP":           "stop",
	"RESUME":         "resume",
	"SUSPEND":        "suspend",
	"SUSPEND_DISK":   "suspend_disk",
	"WAKEUP":         "wakeup",
	"GUEST_PANICKED": "guest_panicked",
	"WATCHDOG":       "watchdog",
}

type qmpBlockIOErrorKey struct {
	Device    string
	Operation string
	NoSpace   bool
}

// qmpCommand is one outbound JSON-RPC request frame on the QMP stream.
type qmpCommand struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments,omitempty"`
	ID        int64          `json:"id"`
}

// qmpResponse captures either a command reply or an asynchronous event from
// the shared QMP stream.
type qmpResponse struct {
	Return json.RawMessage `json:"return"`
	Error  json.RawMessage `json:"error"`
	Event  string          `json:"event"`
	ID     json.RawMessage `json:"id"`
	Data   json.RawMessage `json:"data"`
}

// NewQMPClient connects to path, validates the server greeting, and negotiates
// command mode. Construction is bounded so an unresponsive private socket
// cannot stall backend startup indefinitely.
func NewQMPClient(path string) (*QMPClient, error) {
	return NewQMPClientContext(context.Background(), path)
}

// NewQMPClientContext is NewQMPClient with caller-supplied cancellation and
// timeout control.
func NewQMPClientContext(ctx context.Context, path string) (*QMPClient, error) {
	callCtx, cancel := boundedQMPContext(ctx, qmpOperationTimeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(callCtx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect QMP socket: %w", err)
	}
	client := &QMPClient{
		gate:               make(chan struct{}, 1),
		conn:               conn,
		dec:                json.NewDecoder(conn),
		lifecycleEvents:    make(map[string]uint64, len(qmpLifecycleEventNames)),
		blockIOErrorEvents: make(map[qmpBlockIOErrorKey]uint64),
	}
	for _, event := range qmpLifecycleEventNames {
		client.lifecycleEvents[event] = 0
	}
	client.gate <- struct{}{}
	if err := client.initialize(callCtx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

func boundedQMPContext(ctx context.Context, maximum time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= maximum {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, maximum)
}

func (c *QMPClient) initialize(ctx context.Context) error {
	finish, err := c.setContextDeadline(ctx)
	if err != nil {
		return err
	}

	var greeting qmpGreeting
	if err := c.dec.Decode(&greeting); err != nil {
		finish()
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return fmt.Errorf("decode QMP greeting: %w", err)
	}
	finish()
	// The version object is mandatory. QEMU major versions are positive; this
	// also rejects {}, {"QMP":{}}, events, and command responses as greetings.
	if greeting.QMP.Version.QEMU.Major <= 0 || greeting.QMP.Version.QEMU.Minor < 0 || greeting.QMP.Version.QEMU.Micro < 0 || greeting.QMP.Capabilities == nil {
		return errors.New("invalid QMP greeting")
	}
	c.version = backend.QEMUVersion{
		Major:   greeting.QMP.Version.QEMU.Major,
		Minor:   greeting.QMP.Version.QEMU.Minor,
		Micro:   greeting.QMP.Version.QEMU.Micro,
		Package: greeting.QMP.Version.Package,
	}

	result, err := c.executeLocked(ctx, "qmp_capabilities", nil)
	if err != nil {
		return fmt.Errorf("negotiate QMP capabilities: %w", err)
	}
	var capabilitiesResult map[string]json.RawMessage
	if err := json.Unmarshal(result, &capabilitiesResult); err != nil || capabilitiesResult == nil {
		return errors.New("invalid qmp_capabilities response")
	}
	return nil
}

// Close safely closes the connection. It is idempotent.
func (c *QMPClient) Close() error {
	<-c.gate
	defer func() { c.gate <- struct{}{} }()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// Version returns the QEMU version advertised in the initial greeting.
func (c *QMPClient) Version() backend.QEMUVersion { return c.version }

// RawStatus returns the unmodified query-status state string from QMP.
func (c *QMPClient) RawStatus(ctx context.Context) (string, error) {
	result, err := c.execute(ctx, "query-status", nil)
	if err != nil {
		return "", err
	}
	var status struct {
		Status *string `json:"status"`
	}
	if err := json.Unmarshal(result, &status); err != nil {
		return "", fmt.Errorf("decode query-status response: %w", err)
	}
	if status.Status == nil || *status.Status == "" {
		return "", errors.New("query-status response has empty status")
	}
	return *status.Status, nil
}

// EventCounters snapshots the lifecycle and block I/O event counts accumulated
// on this connection.
func (c *QMPClient) EventCounters(ctx context.Context) (backend.QEMUEventCounters, error) {
	select {
	case <-ctx.Done():
		return backend.QEMUEventCounters{}, ctx.Err()
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	lifecycle := make(map[string]uint64, len(c.lifecycleEvents))
	for event, count := range c.lifecycleEvents {
		lifecycle[event] = count
	}
	block := make([]backend.QEMUBlockIOError, 0, len(c.blockIOErrorEvents))
	for key, count := range c.blockIOErrorEvents {
		block = append(block, backend.QEMUBlockIOError{
			Device: key.Device, Operation: key.Operation, NoSpace: key.NoSpace, Count: count,
		})
	}
	sort.Slice(block, func(i, j int) bool {
		if block[i].Device != block[j].Device {
			return block[i].Device < block[j].Device
		}
		if block[i].Operation != block[j].Operation {
			return block[i].Operation < block[j].Operation
		}
		return !block[i].NoSpace && block[j].NoSpace
	})
	return backend.QEMUEventCounters{Lifecycle: lifecycle, BlockIO: block}, nil
}

func (c *QMPClient) recordEvent(response qmpResponse) {
	if event, ok := qmpLifecycleEventNames[response.Event]; ok {
		c.lifecycleEvents[event]++
		return
	}
	if response.Event != "BLOCK_IO_ERROR" {
		return
	}
	var data struct {
		Device    string `json:"device"`
		Operation string `json:"operation"`
		NoSpace   bool   `json:"nospace"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil ||
		(data.Device != "" && !qmpDeviceLabelPattern.MatchString(data.Device)) ||
		(data.Operation != "read" && data.Operation != "write" && data.Operation != "flush" && data.Operation != "unmap") {
		return
	}
	c.blockIOErrorEvents[qmpBlockIOErrorKey{Device: data.Device, Operation: data.Operation, NoSpace: data.NoSpace}]++
}

// Status queries QEMU and maps only the states represented by model.RunState.
func (c *QMPClient) Status(ctx context.Context) (model.RunState, error) {
	status, err := c.RawStatus(ctx)
	if err != nil {
		return model.RunStateFailed, err
	}
	switch status {
	case string(model.RunStateRunning):
		return model.RunStateRunning, nil
	case string(model.RunStatePaused):
		return model.RunStatePaused, nil
	default:
		return model.RunStateFailed, &UnexpectedStatusError{Status: status}
	}
}

// QueryVNC returns the validated query-vnc state for the current VM.
func (c *QMPClient) QueryVNC(ctx context.Context) (VNCInfo, error) {
	result, err := c.execute(ctx, "query-vnc", nil)
	if err != nil {
		return VNCInfo{}, err
	}
	var info *qmpVNCInfo
	decoder := json.NewDecoder(bytes.NewReader(result))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&info); err != nil {
		return VNCInfo{}, fmt.Errorf("decode query-vnc response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return VNCInfo{}, errors.New("decode query-vnc response: trailing data")
	}
	if info == nil {
		return VNCInfo{}, errors.New("query-vnc response is null")
	}
	if info.Enabled {
		if info.Host == "" || info.Service == "" || info.Family == "" || info.Auth == "" {
			return VNCInfo{}, errors.New("query-vnc response is missing enabled VNC fields")
		}
		if len(info.Clients) != 0 {
			return VNCInfo{}, errors.New("query-vnc response reports connected clients")
		}
	}
	return VNCInfo{
		Enabled: info.Enabled,
		Host:    info.Host,
		Service: info.Service,
		Family:  info.Family,
		Auth:    info.Auth,
	}, nil
}

// HumanMonitorCommand executes one HMP command through QMP and returns its
// textual output.
func (c *QMPClient) HumanMonitorCommand(ctx context.Context, commandLine string) (string, error) {
	if strings.TrimSpace(commandLine) == "" {
		return "", errors.New("human monitor command is empty")
	}
	result, err := c.execute(ctx, "human-monitor-command", map[string]any{"command-line": commandLine})
	if err != nil {
		return "", err
	}
	var output *string
	decoder := json.NewDecoder(bytes.NewReader(result))
	if err := decoder.Decode(&output); err != nil {
		return "", fmt.Errorf("decode human-monitor-command response: %w", err)
	}
	if output == nil {
		return "", errors.New("decode human-monitor-command response: return must be a JSON string")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("decode human-monitor-command response: trailing data")
	}
	return *output, nil
}

// SystemPowerdown asks QEMU to inject an ACPI power button event.
func (c *QMPClient) SystemPowerdown(ctx context.Context) error {
	_, err := c.execute(ctx, "system_powerdown", nil)
	return err
}

// Quit asks QEMU to exit immediately through QMP.
func (c *QMPClient) Quit(ctx context.Context) error {
	_, err := c.execute(ctx, "quit", nil)
	return err
}

func (c *QMPClient) execute(ctx context.Context, command string, arguments map[string]any) (json.RawMessage, error) {
	// gate gives one caller exclusive ownership of the JSON-RPC stream so a
	// single request can write and then drain replies without another caller
	// consuming the matching response.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()

	// Cancellation and gate release can become ready together. Check again
	// before touching the persistent stream so a canceled waiter cannot consume
	// an ID, write a command, or install a socket deadline.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.executeLocked(ctx, command, arguments)
}

func (c *QMPClient) executeLocked(ctx context.Context, command string, arguments map[string]any) (json.RawMessage, error) {
	if c.closed {
		return nil, net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	finish, err := c.setContextDeadline(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()

	// Tag each request with a monotonically increasing ID so the shared decode
	// loop can distinguish our reply from asynchronous events or stale frames.
	c.nextID++
	id := c.nextID
	frame, err := json.Marshal(qmpCommand{Execute: command, Arguments: arguments, ID: id})
	if err != nil {
		return nil, fmt.Errorf("encode QMP command: %w", err)
	}
	frame = append(frame, '\n')
	if _, err := io.Copy(c.conn, &byteReader{data: frame}); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, fmt.Errorf("write QMP command: %w", err)
	}

	// QMP multiplexes async events and command replies on the same socket, so
	// keep draining until the response with our ID arrives.
	for {
		var response qmpResponse
		if err := c.dec.Decode(&response); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			return nil, fmt.Errorf("decode QMP response: %w", err)
		}
		if response.Event != "" {
			c.recordEvent(response)
			continue
		}
		responseID, valid := numericID(response.ID)
		if !valid || responseID != id {
			continue
		}
		hasError := len(response.Error) != 0 && string(response.Error) != "null"
		hasReturn := len(response.Return) != 0
		if hasError == hasReturn {
			return nil, errors.New("QMP response must contain exactly one of return or error")
		}
		if hasError {
			var qmpErr struct {
				Class string `json:"class"`
				Desc  string `json:"desc"`
			}
			if err := json.Unmarshal(response.Error, &qmpErr); err != nil || qmpErr.Class == "" {
				return nil, errors.New("invalid structured QMP error response")
			}
			return nil, &QMPError{Class: qmpErr.Class, Description: qmpErr.Desc, Data: append(json.RawMessage(nil), response.Error...)}
		}
		if !hasReturn {
			return nil, errors.New("QMP response has neither return nor error")
		}
		return append(json.RawMessage(nil), response.Return...), nil
	}
}

// numericID extracts a JSON-RPC response ID only when QMP encoded it as an
// integer.
func numericID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var id int64
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0, false
	}
	return id, true
}

// setContextDeadline installs the per-operation socket deadline and keeps a
// watcher that forces an immediate timeout if the caller's context expires
// before the current QMP operation finishes.
func (c *QMPClient) setContextDeadline(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(qmpOperationTimeout)
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set QMP deadline: %w", err)
	}

	finished := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			_ = c.conn.SetDeadline(time.Now())
		case <-finished:
		}
	}()
	return func() {
		close(finished)
		// The interrupter must finish before its deadline is cleared. Otherwise
		// it could wake after a later operation has installed its own deadline
		// and overwrite that deadline with an immediate timeout.
		<-stopped
		_ = c.conn.SetDeadline(time.Time{})
	}, nil
}

// byteReader lets io.Copy handle short Unix-socket writes without another
// buffering layer.
type byteReader struct {
	data []byte
}

// Read satisfies io.Reader over the remaining buffered frame bytes.
func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
