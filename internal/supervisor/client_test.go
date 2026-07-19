package supervisor

import (
	"bytes"
	"context"
	"io"
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
