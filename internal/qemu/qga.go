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
	"strconv"
	"sync/atomic"
	"time"
)

const (
	qgaCallTimeout          = 5 * time.Second
	qgaShutdownReplyTimeout = time.Second
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

// GuestShutdown asks the QEMU guest agent at path to power down the guest. QGA
// deliberately sends no response when guest-shutdown succeeds; an immediate
// structured error response is still reported to the caller.
func GuestShutdown(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("QGA socket path is empty")
	}

	callCtx, cancel := boundedQGAContext(ctx, qgaCallTimeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(callCtx, "unix", path)
	if err != nil {
		return fmt.Errorf("connect QGA socket %q: %w", path, err)
	}
	defer conn.Close()
	if deadline, ok := callCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set QGA deadline: %w", err)
		}
	}

	reader := bufio.NewReader(conn)
	if _, err := conn.Write([]byte{0xff}); err != nil {
		return fmt.Errorf("write QGA synchronization delimiter: %w", err)
	}

	syncID := qgaRequestID.Add(1)
	if err := writeQGARequest(conn, qgaRequest{
		Execute:   "guest-sync-delimited",
		Arguments: qgaSyncArguments{ID: syncID},
		ID:        syncID,
	}); err != nil {
		return fmt.Errorf("send QGA guest-sync-delimited: %w", err)
	}
	if err := awaitQGASync(reader, syncID); err != nil {
		return fmt.Errorf("synchronize QGA: %w", err)
	}

	shutdownID := qgaRequestID.Add(1)
	if err := writeQGARequest(conn, qgaRequest{
		Execute:   "guest-shutdown",
		Arguments: &qgaShutdownArguments{Mode: "powerdown"},
		ID:        shutdownID,
	}); err != nil {
		return fmt.Errorf("send QGA guest-shutdown: %w", err)
	}

	probeDeadline := time.Now().Add(qgaShutdownReplyTimeout)
	if deadline, ok := callCtx.Deadline(); ok && deadline.Before(probeDeadline) {
		probeDeadline = deadline
	}
	if err := conn.SetReadDeadline(probeDeadline); err != nil {
		return fmt.Errorf("set QGA shutdown response deadline: %w", err)
	}
	return awaitQGAShutdownResult(reader, shutdownID)
}

func boundedQGAContext(ctx context.Context, maximum time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= maximum {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, maximum)
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

func awaitQGASync(reader *bufio.Reader, expectedID uint64) error {
	// guest-sync-delimited prefixes its response with 0xff specifically so a
	// client can discard any stale, partial, or otherwise unparseable input.
	if _, err := reader.ReadBytes(0xff); err != nil {
		return err
	}

	for {
		frame, err := readQGAFrame(reader)
		if err != nil {
			return err
		}
		response, err := decodeQGAResponse(frame)
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
		if response.Error != nil {
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

func awaitQGAShutdownResult(reader *bufio.Reader, expectedID uint64) error {
	for {
		frame, err := readQGAFrame(reader)
		if err != nil {
			var netErr net.Error
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
				return nil
			}
			// The agent commonly closes its socket while the guest shuts down.
			// Once the complete request has been written, absence of a structured
			// response is the documented asynchronous success case.
			return nil
		}
		response, err := decodeQGAResponse(frame)
		if err != nil {
			return fmt.Errorf("decode QGA guest-shutdown response: %w", err)
		}
		matches, err := qgaResponseMatches(response, expectedID)
		if err != nil {
			return fmt.Errorf("decode QGA guest-shutdown response ID: %w", err)
		}
		if !matches || response.Event != "" {
			continue
		}
		if response.Error != nil {
			return response.Error
		}
		return nil
	}
}

func readQGAFrame(reader *bufio.Reader) ([]byte, error) {
	for {
		frame, err := reader.ReadBytes('\n')
		frame = bytes.TrimSpace(bytes.TrimLeft(frame, "\xff"))
		if len(frame) != 0 {
			return frame, nil
		}
		if err != nil {
			return nil, err
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
