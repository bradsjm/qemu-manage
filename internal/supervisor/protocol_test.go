package supervisor

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"qemu-manage/internal/model"
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

	status := &Status{State: model.RunStateRunning, Backend: model.BackendQEMU, SupervisorPID: 11, BackendPID: 12, StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), RunningConfigSHA256: strings.Repeat("a", 64)}
	wire.Reset()
	if err := EncodeResponse(&wire, &Response{Version: ProtocolVersion, ID: testProtocolID, OK: true, Status: status}); err != nil {
		t.Fatal(err)
	}
	response, err := DecodeResponse(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status == nil || response.Status.State != model.RunStateRunning {
		t.Fatalf("decoded response = %#v", response)
	}
}

func TestProtocolRejectsMalformedFraming(t *testing.T) {
	valid := `{"version":1,"id":"` + testProtocolID + `","command":"status"}`
	tests := []struct{ name, input, want string }{
		{"unknown field", valid[:len(valid)-1] + `,"extra":true}` + "\n", "unknown field"},
		{"trailing JSON", valid + ` {}` + "\n", "trailing JSON"},
		{"missing newline", valid, "must end with a newline"},
		{"oversize", strings.Repeat(" ", MaxMessageBytes) + "\n", "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRequest(strings.NewReader(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
	_, err := DecodeRequest(strings.NewReader(""))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("clean EOF error = %v", err)
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
	goodStatus := &Status{State: model.RunStatePaused, Backend: model.BackendQEMU, SupervisorPID: 1, BackendPID: 2, StartedAt: time.Now().UTC(), RunningConfigSHA256: strings.Repeat("0", 64)}
	goodError := &ProtocolError{Code: ErrorInternal, Message: "failed"}
	tests := []struct {
		name     string
		response Response
		valid    bool
	}{
		{"success empty", Response{Version: 1, ID: testProtocolID, OK: true}, true},
		{"success status", Response{Version: 1, ID: testProtocolID, OK: true, Status: goodStatus}, true},
		{"success error", Response{Version: 1, ID: testProtocolID, OK: true, Error: goodError}, false},
		{"failure error", Response{Version: 1, ID: testProtocolID, Error: goodError}, true},
		{"failure missing error", Response{Version: 1, ID: testProtocolID}, false},
		{"failure status", Response{Version: 1, ID: testProtocolID, Status: goodStatus, Error: goodError}, false},
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
