package qemu

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	qgaCallTimeout          = 5 * time.Second
	qgaShutdownReplyTimeout = time.Second
	maxQGAFrameBytes        = 65 << 20
)

var qgaRequestID atomic.Uint64

type qgaRequest struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
	ID        uint64 `json:"id"`
}

type qgaSyncArguments struct {
	ID uint64 `json:"id"`
}

type qgaShutdownArguments struct {
	Mode string `json:"mode"`
}

type GuestAgentRequest struct {
	Execute   string
	Arguments json.RawMessage
}

type qgaConnection struct {
	ctx            context.Context
	cancel         context.CancelFunc
	conn           net.Conn
	reader         *bufio.Reader
	finishDeadline func()
}

type qgaResponse struct {
	Return json.RawMessage `json:"return,omitempty"`
	Error  *QGAError       `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	ID     json.Number     `json:"id,omitempty"`
}

// QGAError is the structured error object returned by the guest agent.
type QGAError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *QGAError) Error() string {
	if e.Desc == "" {
		return fmt.Sprintf("QGA command error %q", e.Class)
	}
	return fmt.Sprintf("QGA command error %q: %s", e.Class, e.Desc)
}

func DecodeGuestAgentRequest(data []byte) (GuestAgentRequest, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return GuestAgentRequest{}, errors.New("guest agent request must be a JSON object")
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return GuestAgentRequest{}, fmt.Errorf("decode guest agent request: %w", err)
	}
	if raw == nil {
		return GuestAgentRequest{}, errors.New("guest agent request must be a JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return GuestAgentRequest{}, errors.New("guest agent request contains trailing JSON data")
		}
		return GuestAgentRequest{}, fmt.Errorf("decode guest agent request trailer: %w", err)
	}

	executeRaw, ok := raw["execute"]
	if !ok {
		return GuestAgentRequest{}, errors.New("guest agent request is missing \"execute\"")
	}
	delete(raw, "execute")
	var execute string
	if err := json.Unmarshal(executeRaw, &execute); err != nil {
		return GuestAgentRequest{}, errors.New("guest agent request \"execute\" must be a string")
	}
	if strings.TrimSpace(execute) == "" {
		return GuestAgentRequest{}, errors.New("guest agent request \"execute\" must not be blank")
	}

	arguments, err := validateGuestAgentArguments(raw["arguments"], raw["arguments"] != nil)
	if err != nil {
		return GuestAgentRequest{}, err
	}
	delete(raw, "arguments")
	if len(raw) != 0 {
		keys := make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return GuestAgentRequest{}, fmt.Errorf("guest agent request has unknown field %q", keys[0])
	}

	return GuestAgentRequest{Execute: execute, Arguments: arguments}, nil
}

// GuestShutdown asks the QEMU guest agent at path to power down the guest. QGA
// deliberately sends no response when guest-shutdown succeeds; an immediate
// structured error response is still reported to the caller.
func GuestShutdown(ctx context.Context, path string) error {
	conn, err := openQGAConnection(ctx, path)
	if err != nil {
		return err
	}
	defer conn.Close()

	shutdownID := qgaRequestID.Add(1)
	if err := writeQGARequest(conn.conn, qgaRequest{
		Execute:   "guest-shutdown",
		Arguments: &qgaShutdownArguments{Mode: "powerdown"},
		ID:        shutdownID,
	}); err != nil {
		return fmt.Errorf("send QGA guest-shutdown: %w", err)
	}
	_, err = awaitQGANoResponseResult(conn, shutdownID)
	return err
}

func GuestAgentCommand(ctx context.Context, path string, request GuestAgentRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.Execute) == "" {
		return nil, errors.New("QGA execute name is empty")
	}
	arguments, err := validateGuestAgentArguments(request.Arguments, len(request.Arguments) != 0)
	if err != nil {
		return nil, err
	}

	conn, err := openQGAConnection(ctx, path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	requestID := qgaRequestID.Add(1)
	wireRequest := qgaRequest{Execute: request.Execute, ID: requestID}
	if len(arguments) != 0 {
		wireRequest.Arguments = arguments
	}
	if err := writeQGARequest(conn.conn, wireRequest); err != nil {
		return nil, fmt.Errorf("send QGA %s: %w", request.Execute, err)
	}
	if qgaCommandMayNotReply(request.Execute) {
		return awaitQGANoResponseResult(conn, requestID)
	}
	return awaitQGAResult(conn.ctx, conn.reader, requestID)
}

func validateGuestAgentArguments(raw json.RawMessage, present bool) (json.RawMessage, error) {
	if !present {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, errors.New("guest agent request \"arguments\" must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return nil, errors.New("guest agent request \"arguments\" must be a JSON object")
	}
	if object == nil {
		return nil, errors.New("guest agent request \"arguments\" must be a JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("guest agent request \"arguments\" contains trailing JSON data")
		}
		return nil, errors.New("guest agent request \"arguments\" must be a single JSON object")
	}
	return append(json.RawMessage(nil), trimmed...), nil
}

func openQGAConnection(ctx context.Context, path string) (*qgaConnection, error) {
	if path == "" {
		return nil, fmt.Errorf("QGA socket path is empty")
	}

	callCtx, cancel := boundedQGAContext(ctx, qgaCallTimeout)
	conn, err := (&net.Dialer{}).DialContext(callCtx, "unix", path)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect QGA socket %q: %w", path, err)
	}
	finishDeadline, err := setQGADeadline(conn, callCtx)
	if err != nil {
		_ = conn.Close()
		cancel()
		return nil, err
	}

	connection := &qgaConnection{
		ctx:            callCtx,
		cancel:         cancel,
		conn:           conn,
		reader:         bufio.NewReader(conn),
		finishDeadline: finishDeadline,
	}
	if err := connection.synchronize(); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

func (c *qgaConnection) Close() error {
	c.cancel()
	c.finishDeadline()
	return c.conn.Close()
}

func (c *qgaConnection) synchronize() error {
	if _, err := c.conn.Write([]byte{0xff}); err != nil {
		return fmt.Errorf("write QGA synchronization delimiter: %w", err)
	}

	syncID := qgaRequestID.Add(1)
	if err := writeQGARequest(c.conn, qgaRequest{
		Execute:   "guest-sync-delimited",
		Arguments: qgaSyncArguments{ID: syncID},
		ID:        syncID,
	}); err != nil {
		return fmt.Errorf("send QGA guest-sync-delimited: %w", err)
	}
	if err := awaitQGASync(c.ctx, c.reader, syncID); err != nil {
		return fmt.Errorf("synchronize QGA: %w", err)
	}
	return nil
}

func boundedQGAContext(ctx context.Context, maximum time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= maximum {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, maximum)
}

func setQGADeadline(conn net.Conn, ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return func() {}, nil
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set QGA deadline: %w", err)
	}

	finished := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-finished:
		}
	}()
	return func() {
		close(finished)
		<-stopped
		_ = conn.SetDeadline(time.Time{})
	}, nil
}

func writeQGARequest(writer io.Writer, request qgaRequest) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	written, err := writer.Write(payload)
	if err == nil && written != len(payload) {
		return io.ErrShortWrite
	}
	return err
}

func awaitQGASync(ctx context.Context, reader *bufio.Reader, expectedID uint64) error {
	// guest-sync-delimited prefixes its response with 0xff specifically so a
	// client can discard any stale, partial, or otherwise unparseable input.
	if err := readUntilQGADelimiter(reader, maxQGAFrameBytes); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return err
	}

	for {
		response, err := readDecodedQGAResponse(ctx, reader)
		if err != nil {
			return err
		}
		if response.Event != "" {
			continue
		}
		matches, err := qgaResponseMatches(response, expectedID)
		if err != nil {
			return err
		}
		hasError := response.Error != nil
		hasReturn := len(response.Return) != 0
		if hasError == hasReturn {
			return errors.New("QGA response must contain exactly one of return or error")
		}
		if hasError {
			if matches {
				return response.Error
			}
			continue
		}
		if !matches || !qgaSyncReturnMatches(response.Return, expectedID) {
			continue
		}
		return nil
	}
}

func awaitQGAResult(ctx context.Context, reader *bufio.Reader, expectedID uint64) (json.RawMessage, error) {
	for {
		response, err := readDecodedQGAResponse(ctx, reader)
		if err != nil {
			return nil, err
		}
		if response.Event != "" {
			continue
		}
		matches, err := qgaResponseMatches(response, expectedID)
		if err != nil {
			return nil, err
		}
		if !matches {
			continue
		}
		hasError := response.Error != nil
		hasReturn := len(response.Return) != 0
		if hasError == hasReturn {
			return nil, errors.New("QGA response must contain exactly one of return or error")
		}
		if hasError {
			return nil, response.Error
		}
		return append(json.RawMessage(nil), response.Return...), nil
	}
}

func awaitQGANoResponseResult(conn *qgaConnection, expectedID uint64) (json.RawMessage, error) {
	probeCtx, cancel := boundedQGAContext(conn.ctx, qgaShutdownReplyTimeout)
	defer cancel()
	if deadline, ok := probeCtx.Deadline(); ok {
		if err := conn.conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set QGA response deadline: %w", err)
		}
	}
	result, err := awaitQGAResult(conn.ctx, conn.reader, expectedID)
	if err == nil {
		return result, nil
	}
	if qgaNoResponseSuccess(err) {
		return json.RawMessage("null"), nil
	}
	return nil, err
}

func qgaNoResponseSuccess(err error) bool {
	var netErr net.Error
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.As(err, &netErr) && netErr.Timeout()
}

func qgaCommandMayNotReply(command string) bool {
	switch command {
	case "guest-shutdown", "guest-suspend-disk", "guest-suspend-ram", "guest-suspend-hybrid":
		return true
	default:
		return false
	}
}

func readDecodedQGAResponse(ctx context.Context, reader *bufio.Reader) (qgaResponse, error) {
	frame, err := readQGAFrame(reader, maxQGAFrameBytes)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return qgaResponse{}, contextErr
		}
		return qgaResponse{}, err
	}
	return decodeQGAResponse(frame)
}

func readUntilQGADelimiter(reader *bufio.Reader, maximum int) error {
	count := 0
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return err
		}
		count++
		if count > maximum {
			return fmt.Errorf("QGA response exceeds %d bytes", maximum)
		}
		if b == 0xff {
			return nil
		}
	}
}

func readQGAFrame(reader *bufio.Reader, maximum int) ([]byte, error) {
	count := 0
	for {
		frame := make([]byte, 0, 256)
		var readErr error
		for {
			b, err := reader.ReadByte()
			if err != nil {
				readErr = err
				break
			}
			count++
			if count > maximum {
				return nil, fmt.Errorf("QGA response exceeds %d bytes", maximum)
			}
			frame = append(frame, b)
			if b == '\n' {
				break
			}
		}
		trimmed := bytes.TrimSpace(bytes.TrimLeft(frame, "\xff"))
		if len(trimmed) != 0 {
			return append([]byte(nil), trimmed...), nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func decodeQGAResponse(frame []byte) (qgaResponse, error) {
	var response qgaResponse
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return qgaResponse{}, fmt.Errorf("decode QGA response: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return qgaResponse{}, fmt.Errorf("decode QGA response: trailing JSON data")
		}
		return qgaResponse{}, fmt.Errorf("decode QGA response trailer: %w", err)
	}
	return response, nil
}

func qgaResponseMatches(response qgaResponse, expectedID uint64) (bool, error) {
	if response.ID == "" {
		return false, nil
	}
	id, err := strconv.ParseUint(response.ID.String(), 10, 64)
	if err != nil {
		return false, fmt.Errorf("invalid QGA response ID %q", response.ID)
	}
	return id == expectedID, nil
}

func qgaSyncReturnMatches(raw json.RawMessage, expectedID uint64) bool {
	if len(raw) == 0 {
		return false
	}
	id, err := strconv.ParseUint(string(bytes.TrimSpace(raw)), 10, 64)
	return err == nil && id == expectedID
}
