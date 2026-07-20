package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
)

const testProtocolID = "0123456789abcdef0123456789abcdef"

func TestProtocolRoundTrip(t *testing.T) {
	timeout := 37
	request := &Request{Version: ProtocolVersion, ID: testProtocolID, Command: CommandStop, TimeoutSeconds: &timeout}
	var wire bytes.Buffer
	if err := EncodeRequest(&wire, request); err != nil {
		t.Fatal(err)
	}
	if got := wire.String(); !strings.HasSuffix(got, "\n") {
		t.Fatalf("encoded request lacks newline: %q", got)
	}
	decoded, err := DecodeRequest(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Command != request.Command || decoded.TimeoutSeconds == nil || *decoded.TimeoutSeconds != timeout {
		t.Fatalf("decoded request = %#v", decoded)
	}

	status := &Status{
		State:               model.RunStateRunning,
		Backend:             model.BackendQEMU,
		SupervisorPID:       11,
		BackendPID:          12,
		StartedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		RunningConfigSHA256: strings.Repeat("a", 64),
		VNC:                 &backend.VNCEndpoint{Host: "127.0.0.1", Port: 5901},
	}
	wire.Reset()
	if err := EncodeResponse(&wire, &Response{Version: ProtocolVersion, ID: testProtocolID, OK: true, Status: status}); err != nil {
		t.Fatal(err)
	}
	response, err := DecodeResponse(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status == nil || response.Status.State != model.RunStateRunning || response.Status.VNC == nil || *response.Status.VNC != *status.VNC {
		t.Fatalf("decoded response = %#v", response)
	}
}

func TestProtocolRejectsMalformedFraming(t *testing.T) {
	valid := `{"version":1,"id":"` + testProtocolID + `","command":"status"}`
	tests := []struct{ name, input, want string }{
		{"unknown field", valid[:len(valid)-1] + `,"extra":true}` + "\n", "unknown field"},
		{"trailing JSON", valid + ` {}` + "\n", "trailing JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRequest(strings.NewReader(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}

	var request Request
	if err := newFramedReader(strings.NewReader(valid)).decode(&request); err == nil || !strings.Contains(err.Error(), "must end with a newline") {
		t.Fatalf("missing newline error = %v", err)
	}
	if err := newFramedReader(strings.NewReader("")).decode(&request); !errors.Is(err, io.EOF) {
		t.Fatalf("clean EOF error = %v", err)
	}

	padding := MaxMessageBytes - len(valid) - 1
	if padding < 0 {
		t.Fatalf("valid request length %d exceeds frame limit", len(valid))
	}
	exact := valid + strings.Repeat(" ", padding) + "\n"
	if err := newFramedReader(strings.NewReader(exact)).decode(&request); err != nil {
		t.Fatalf("exact-size frame error = %v", err)
	}
	if request.Command != CommandStatus || request.ID != testProtocolID {
		t.Fatalf("decoded request = %#v", request)
	}

	oversized := valid + strings.Repeat(" ", padding+1) + "\n"
	if err := newFramedReader(strings.NewReader(oversized)).decode(&request); err == nil || err.Error() != fmt.Sprintf("message exceeds %d bytes", MaxMessageBytes) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestRequestOptionConsistency(t *testing.T) {
	one, zero, tooLarge := 1, 0, 3601
	tests := []struct {
		name    string
		request Request
		valid   bool
	}{
		{"status", Request{Version: 1, ID: testProtocolID, Command: CommandStatus}, true},
		{"status force", Request{Version: 1, ID: testProtocolID, Command: CommandStatus, Force: true}, false},
		{"status timeout", Request{Version: 1, ID: testProtocolID, Command: CommandStatus, TimeoutSeconds: &one}, false},
		{"stop defaults", Request{Version: 1, ID: testProtocolID, Command: CommandStop}, true},
		{"stop force", Request{Version: 1, ID: testProtocolID, Command: CommandStop, Force: true}, true},
		{"zero timeout", Request{Version: 1, ID: testProtocolID, Command: CommandStop, TimeoutSeconds: &zero}, false},
		{"large timeout", Request{Version: 1, ID: testProtocolID, Command: CommandStop, TimeoutSeconds: &tooLarge}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.request.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("Validate error = %v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestResponseOptionConsistency(t *testing.T) {
	goodStatus := &Status{
		State:               model.RunStatePaused,
		Backend:             model.BackendQEMU,
		SupervisorPID:       1,
		BackendPID:          2,
		StartedAt:           time.Now().UTC(),
		RunningConfigSHA256: strings.Repeat("0", 64),
		VNC:                 &backend.VNCEndpoint{Host: "127.0.0.1", Port: 5900},
	}
	goodError := &ProtocolError{Code: ErrorInternal, Message: "failed"}
	acknowledged := StopProgressAcknowledged
	unknownProgress := StopProgress("unknown")
	tests := []struct {
		name     string
		response Response
		valid    bool
	}{
		{"success empty", Response{Version: 1, ID: testProtocolID, OK: true}, true},
		{"success status", Response{Version: 1, ID: testProtocolID, OK: true, Status: goodStatus}, true},
		{"success error", Response{Version: 1, ID: testProtocolID, OK: true, Error: goodError}, false},
		{"success progress", Response{Version: 1, ID: testProtocolID, OK: true, Progress: &acknowledged}, true},
		{"success progress and status", Response{Version: 1, ID: testProtocolID, OK: true, Status: goodStatus, Progress: &acknowledged}, false},
		{"success unknown progress", Response{Version: 1, ID: testProtocolID, OK: true, Progress: &unknownProgress}, false},
		{"failure error", Response{Version: 1, ID: testProtocolID, Error: goodError}, true},
		{"failure missing error", Response{Version: 1, ID: testProtocolID}, false},
		{"failure status", Response{Version: 1, ID: testProtocolID, Status: goodStatus, Error: goodError}, false},
		{"failure progress", Response{Version: 1, ID: testProtocolID, Progress: &acknowledged, Error: goodError}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.response.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("Validate error = %v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestStatusValidateVNCEndpoint(t *testing.T) {
	base := Status{
		State:               model.RunStateRunning,
		Backend:             model.BackendQEMU,
		SupervisorPID:       1,
		BackendPID:          2,
		StartedAt:           time.Now().UTC(),
		RunningConfigSHA256: strings.Repeat("a", 64),
	}
	tests := []struct {
		name   string
		vnc    *backend.VNCEndpoint
		valid  bool
		errSub string
	}{
		{name: "omitted", valid: true},
		{name: "ipv4", vnc: &backend.VNCEndpoint{Host: "0.0.0.0", Port: 5900}, valid: true},
		{name: "ipv6", vnc: &backend.VNCEndpoint{Host: "::1", Port: 5900}, errSub: "IPv4"},
		{name: "ipv4 mapped ipv6", vnc: &backend.VNCEndpoint{Host: "::ffff:127.0.0.1", Port: 5900}, errSub: "IPv4"},
		{name: "zero port", vnc: &backend.VNCEndpoint{Host: "127.0.0.1", Port: 0}, errSub: "nonzero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := base
			status.VNC = tt.vnc
			err := status.Validate()
			if tt.valid {
				if err != nil {
					t.Fatalf("Validate error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errSub) {
				t.Fatalf("Validate error = %v, want containing %q", err, tt.errSub)
			}
		})
	}
}
