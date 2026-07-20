package supervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartForegroundInvokesOnReadyAfterMatchingReadiness(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- StartProcess(context.Background(), StartOptions{
			Name:         "vm",
			ExpectedID:   "0123456789abcdef0123456789abcdef",
			Foreground:   true,
			ReadyTimeout: time.Second,
			OnReady:      func() { close(ready) },
			RunForeground: func(_ context.Context, writer io.Writer) error {
				if err := WriteReady(writer, ReadyMessage{Version: ProtocolVersion, ID: "0123456789abcdef0123456789abcdef", OK: true}); err != nil {
					return err
				}
				<-release
				return nil
			},
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("start returned before readiness callback: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("start returned before foreground release: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStartForegroundDoesNotInvokeOnReadyForMismatchedReadiness(t *testing.T) {
	called := false
	err := StartProcess(context.Background(), StartOptions{
		Name:         "vm",
		ExpectedID:   "0123456789abcdef0123456789abcdef",
		Foreground:   true,
		ReadyTimeout: time.Second,
		OnReady:      func() { called = true },
		RunForeground: func(_ context.Context, writer io.Writer) error {
			return WriteReady(writer, ReadyMessage{Version: ProtocolVersion, ID: "fedcba9876543210fedcba9876543210", OK: true})
		},
	})
	if err == nil || called {
		t.Fatalf("error=%v, callback=%t; want readiness error and no callback", err, called)
	}
}

func TestControlWithProgressPreservesCoalescedResponses(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "qm-c-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	socketPath := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()

		request, err := DecodeRequest(connection)
		if err != nil {
			serverDone <- err
			return
		}
		if request.Command != CommandStop {
			serverDone <- errors.New("unexpected command")
			return
		}

		progress := StopProgressAcknowledged
		var frames bytes.Buffer
		if err := EncodeResponse(&frames, &Response{Version: ProtocolVersion, ID: request.ID, OK: true, Progress: &progress}); err != nil {
			serverDone <- err
			return
		}
		if err := EncodeResponse(&frames, &Response{Version: ProtocolVersion, ID: request.ID, OK: true}); err != nil {
			serverDone <- err
			return
		}
		if _, err := connection.Write(frames.Bytes()); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	var got []StopProgress
	response, err := ControlWithProgress(context.Background(), socketPath, Request{
		Version: ProtocolVersion,
		ID:      testProtocolID,
		Command: CommandStop,
	}, func(progress StopProgress) {
		got = append(got, progress)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("response = %#v", response)
	}
	if len(got) != 1 || got[0] != StopProgressAcknowledged {
		t.Fatalf("progress callbacks = %#v", got)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDebugfRequiresExplicitEnablement(t *testing.T) {
	var output bytes.Buffer
	debugf(false, &output, "hidden")
	if output.Len() != 0 {
		t.Fatalf("disabled debug wrote %q", output.String())
	}
	debugf(true, &output, "visible")
	if output.String() != "debug: visible\n" {
		t.Fatalf("enabled debug output=%q", output.String())
	}
}
