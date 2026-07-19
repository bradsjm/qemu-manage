package supervisor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"time"

	"qemu-manage/internal/backend"
	"qemu-manage/internal/model"
)

const (
	ProtocolVersion = 1
	MaxMessageBytes = 64 * 1024
)

type Command string

const (
	CommandStatus Command = "status"
	CommandStop   Command = "stop"
)

type ErrorCode string

const (
	ErrorInvalidRequest  ErrorCode = "invalid_request"
	ErrorUnauthorized    ErrorCode = "unauthorized"
	ErrorNotRunning      ErrorCode = "not_running"
	ErrorShutdownTimeout ErrorCode = "shutdown_timeout"
	ErrorInternal        ErrorCode = "internal"
)

type Request struct {
	Version        int     `json:"version"`
	ID             string  `json:"id"`
	Command        Command `json:"command"`
	Force          bool    `json:"force,omitempty"`
	TimeoutSeconds *int    `json:"timeout_seconds,omitempty"`
}

type Response struct {
	Version int            `json:"version"`
	ID      string         `json:"id"`
	OK      bool           `json:"ok"`
	Status  *Status        `json:"status,omitempty"`
	Error   *ProtocolError `json:"error,omitempty"`
}

type ProtocolError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type Status struct {
	State               model.RunState       `json:"state"`
	Backend             model.Backend        `json:"backend"`
	SupervisorPID       int                  `json:"supervisor_pid"`
	BackendPID          int                  `json:"backend_pid"`
	StartedAt           time.Time            `json:"started_at"`
	RunningConfigSHA256 string               `json:"running_config_sha256"`
	VNC                 *backend.VNCEndpoint `json:"vnc,omitempty"`
}

var (
	protocolIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	protocolHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (r Request) Validate() error {
	if err := validateEnvelope(r.Version, r.ID); err != nil {
		return err
	}
	switch r.Command {
	case CommandStatus:
		if r.Force || r.TimeoutSeconds != nil {
			return errors.New("status request must not include stop options")
		}
	case CommandStop:
		if r.TimeoutSeconds != nil && (*r.TimeoutSeconds < 1 || *r.TimeoutSeconds > 3600) {
			return errors.New("timeout_seconds must be between 1 and 3600")
		}
	default:
		return fmt.Errorf("unsupported command %q", r.Command)
	}
	return nil
}

func (r Response) Validate() error {
	if err := validateEnvelope(r.Version, r.ID); err != nil {
		return err
	}
	if r.OK {
		if r.Error != nil {
			return errors.New("successful response must not include error")
		}
		if r.Status != nil {
			return r.Status.Validate()
		}
		return nil
	}
	if r.Status != nil {
		return errors.New("failed response must not include status")
	}
	if r.Error == nil {
		return errors.New("failed response must include error")
	}
	return r.Error.Validate()
}

func (e ProtocolError) Validate() error {
	switch e.Code {
	case ErrorInvalidRequest, ErrorUnauthorized, ErrorNotRunning, ErrorShutdownTimeout, ErrorInternal:
	default:
		return fmt.Errorf("unsupported error code %q", e.Code)
	}
	if e.Message == "" {
		return errors.New("error message is empty")
	}
	return nil
}

func (s Status) Validate() error {
	switch s.State {
	case model.RunStateStarting, model.RunStateRunning, model.RunStatePaused, model.RunStateStopping, model.RunStateStopped, model.RunStateFailed:
	default:
		return fmt.Errorf("unsupported run state %q", s.State)
	}
	switch s.Backend {
	case model.BackendQEMU, model.BackendVZ:
	default:
		return fmt.Errorf("unsupported backend %q", s.Backend)
	}
	if s.SupervisorPID <= 0 {
		return errors.New("supervisor_pid must be positive")
	}
	if s.BackendPID <= 0 {
		return errors.New("backend_pid must be positive")
	}
	_, offset := s.StartedAt.Zone()
	if s.StartedAt.IsZero() || offset != 0 {
		return errors.New("started_at must be a nonzero UTC timestamp")
	}
	if !protocolHashPattern.MatchString(s.RunningConfigSHA256) {
		return errors.New("running_config_sha256 must be 64 lowercase hexadecimal characters")
	}
	if err := validateVNCEndpoint(s.VNC); err != nil {
		return err
	}
	return nil
}
func validateVNCEndpoint(endpoint *backend.VNCEndpoint) error {
	if endpoint == nil {
		return nil
	}
	ip := net.ParseIP(endpoint.Host)
	if ip == nil || ip.To4() == nil || ip.String() != endpoint.Host {
		return errors.New("vnc.host must be an IPv4 literal")
	}
	if endpoint.Port == 0 {
		return errors.New("vnc.port must be nonzero")
	}
	return nil
}

func EncodeRequest(w io.Writer, request *Request) error {
	if request == nil {
		return errors.New("request is nil")
	}
	if err := request.Validate(); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	return encodeLine(w, request)
}

func DecodeRequest(r io.Reader) (*Request, error) {
	var request Request
	if err := decodeLine(r, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	return &request, nil
}

func EncodeResponse(w io.Writer, response *Response) error {
	if response == nil {
		return errors.New("response is nil")
	}
	if err := response.Validate(); err != nil {
		return fmt.Errorf("invalid response: %w", err)
	}
	return encodeLine(w, response)
}

func DecodeResponse(r io.Reader) (*Response, error) {
	var response Response
	if err := decodeLine(r, &response); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	if err := response.Validate(); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	return &response, nil
}

func validateEnvelope(version int, id string) error {
	if version != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", version)
	}
	if !protocolIDPattern.MatchString(id) {
		return errors.New("id must be 32 lowercase hexadecimal characters")
	}
	return nil
}

func encodeLine(w io.Writer, value interface{}) error {
	var line bytes.Buffer
	encoder := json.NewEncoder(&line)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if line.Len() > MaxMessageBytes {
		return fmt.Errorf("message exceeds %d bytes", MaxMessageBytes)
	}
	_, err := w.Write(line.Bytes())
	return err
}

func decodeLine(r io.Reader, destination interface{}) error {
	reader := bufio.NewReaderSize(io.LimitReader(r, MaxMessageBytes+1), MaxMessageBytes+1)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return io.EOF
			}
			return fmt.Errorf("message must end with a newline: %w", io.ErrUnexpectedEOF)
		}
		return err
	}
	if len(line) > MaxMessageBytes {
		return fmt.Errorf("message exceeds %d bytes", MaxMessageBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(line[:len(line)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("message contains trailing JSON data")
		}
		return fmt.Errorf("message contains trailing data: %w", err)
	}
	return nil
}
