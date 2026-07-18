package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func startQGAServer(t *testing.T, serve func(net.Conn) error) string {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "qm-g-")
	if err != nil {
		t.Fatalf("create socket root: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove socket root: %v", err)
		}
	})
	path := filepath.Join(root, "qga.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			err = serve(conn)
			if conn != nil {
				_ = conn.Close()
			}
		}
		_ = listener.Close()
		done <- err
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-done; err != nil {
			t.Errorf("QGA server: %v", err)
		}
	})
	return path
}

type qgaWireRequest struct {
	Execute string          `json:"execute"`
	Args    json.RawMessage `json:"arguments"`
	ID      uint64          `json:"id"`
}

func readQGARequest(reader *bufio.Reader) (qgaWireRequest, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return qgaWireRequest{}, err
	}
	var request qgaWireRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return qgaWireRequest{}, err
	}
	return request, nil
}

func readQGASync(t *testing.T, reader *bufio.Reader) qgaWireRequest {
	t.Helper()
	delimiter, err := reader.ReadByte()
	if err != nil {
		t.Fatalf("read delimiter: %v", err)
	}
	if delimiter != 0xff {
		t.Fatalf("first byte = %#x, want 0xff", delimiter)
	}
	request, err := readQGARequest(reader)
	if err != nil {
		t.Fatalf("read sync: %v", err)
	}
	if request.Execute != "guest-sync-delimited" {
		t.Fatalf("sync execute = %q", request.Execute)
	}
	var args struct {
		ID uint64 `json:"id"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatalf("sync arguments: %v", err)
	}
	if args.ID == 0 || args.ID != request.ID {
		t.Fatalf("sync token/outer IDs = %d/%d", args.ID, request.ID)
	}
	assertJSONKeys(t, request.Args, "id")
	return request
}

func TestGuestShutdownWireResyncNoiseEventsAndNoReply(t *testing.T) {
	path := startQGAServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		syncRequest := readQGASync(t, reader)
		if _, err := fmt.Fprintf(conn, "stale partial garbage\xff"+`{"event":"RESET"}`+"\n"+`{"return":999,"id":999}`+"\n"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
			return err
		}
		shutdown, err := readQGARequest(reader)
		if err != nil {
			return err
		}
		if shutdown.Execute != "guest-shutdown" || shutdown.ID == 0 || shutdown.ID == syncRequest.ID {
			return fmt.Errorf("shutdown envelope = %#v", shutdown)
		}
		var args struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(shutdown.Args, &args); err != nil {
			return err
		}
		if args.Mode != "powerdown" {
			return fmt.Errorf("shutdown mode = %q", args.Mode)
		}
		assertJSONKeys(t, shutdown.Args, "mode")
		return nil // Closing without a reply is the accepted asynchronous-success case.
	})
	if err := GuestShutdown(context.Background(), path); err != nil {
		t.Fatalf("GuestShutdown: %v", err)
	}
}

func TestGuestShutdownIgnoresMismatchedAndEventReplies(t *testing.T) {
	path := startQGAServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		syncRequest := readQGASync(t, reader)
		if _, err := fmt.Fprintf(conn, "\xff"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
			return err
		}
		shutdown, err := readQGARequest(reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn, `{"error":{"class":"Wrong","desc":"ignore"},"id":999}`+"\n"+`{"event":"SHUTDOWN","id":%d}`+"\n"+`{"return":{},"id":%d}`+"\n", shutdown.ID, shutdown.ID)
		return err
	})
	if err := GuestShutdown(context.Background(), path); err != nil {
		t.Fatalf("GuestShutdown: %v", err)
	}
}

func TestGuestShutdownStructuredErrors(t *testing.T) {
	for _, phase := range []string{"sync", "shutdown"} {
		t.Run(phase, func(t *testing.T) {
			path := startQGAServer(t, func(conn net.Conn) error {
				reader := bufio.NewReader(conn)
				syncRequest := readQGASync(t, reader)
				if phase == "sync" {
					_, err := fmt.Fprintf(conn, "\xff"+`{"error":{"class":"CommandDisabled","desc":"sync denied"},"id":%d}`+"\n", syncRequest.ID)
					return err
				}
				if _, err := fmt.Fprintf(conn, "\xff"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
					return err
				}
				shutdown, err := readQGARequest(reader)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(conn, `{"error":{"class":"GenericError","desc":"shutdown denied"},"id":%d}`+"\n", shutdown.ID)
				return err
			})
			err := GuestShutdown(context.Background(), path)
			var qgaErr *QGAError
			if !errors.As(err, &qgaErr) {
				t.Fatalf("error = %v, want QGAError", err)
			}
			if qgaErr.Class == "" || qgaErr.Desc == "" {
				t.Fatalf("structured error lost: %#v", qgaErr)
			}
		})
	}
}

func TestGuestShutdownUnavailableSocket(t *testing.T) {
	root, err := os.MkdirTemp(os.TempDir(), "qm-g-missing-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := filepath.Join(root, "missing.sock")
	err = GuestShutdown(context.Background(), path)
	if err == nil {
		t.Fatal("GuestShutdown unexpectedly succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}

func TestDecodeQGAResponseRejectsTrailingDataAndInvalidID(t *testing.T) {
	if _, err := decodeQGAResponse([]byte(`{"return":1,"id":1} {"return":2}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	response, err := decodeQGAResponse([]byte(`{"return":1,"id":1.5}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qgaResponseMatches(response, 1); err == nil {
		t.Fatal("fractional response ID accepted")
	}
}

func assertJSONKeys(t *testing.T, raw json.RawMessage, want ...string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(object) != len(want) {
		t.Fatalf("keys in %s = %v, want %v", raw, keys(object), want)
	}
	for _, key := range want {
		if _, ok := object[key]; !ok {
			t.Fatalf("key %q missing from %s", key, raw)
		}
	}
}

func keys(object map[string]json.RawMessage) []string {
	result := make([]string, 0, len(object))
	for key := range object {
		result = append(result, key)
	}
	return result
}
