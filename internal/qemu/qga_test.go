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
	"path/filepath"
	"strings"
	"syscall"
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

func TestDecodeGuestAgentRequestStrict(t *testing.T) {
	request, err := DecodeGuestAgentRequest([]byte(`{"execute":" guest-info ","arguments":{"verbose":true}}`))
	if err != nil {
		t.Fatalf("DecodeGuestAgentRequest valid = %v", err)
	}
	if request.Execute != " guest-info " {
		t.Fatalf("execute = %q", request.Execute)
	}
	if string(request.Arguments) != `{"verbose":true}` {
		t.Fatalf("arguments = %s", request.Arguments)
	}
	assertJSONKeys(t, request.Arguments, "verbose")

	withoutArgs, err := DecodeGuestAgentRequest([]byte(`{"execute":"guest-ping"}`))
	if err != nil {
		t.Fatalf("DecodeGuestAgentRequest without arguments = %v", err)
	}
	if withoutArgs.Execute != "guest-ping" || withoutArgs.Arguments != nil {
		t.Fatalf("request without arguments = %#v", withoutArgs)
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "null", input: `null`, want: "JSON object"},
		{name: "missing execute", input: `{}`, want: "missing \"execute\""},
		{name: "blank execute", input: `{"execute":" \t "}`, want: "must not be blank"},
		{name: "unknown field", input: `{"execute":"guest-info","id":7}`, want: "unknown field \"id\""},
		{name: "null arguments", input: `{"execute":"guest-info","arguments":null}`, want: "must be a JSON object"},
		{name: "array arguments", input: `{"execute":"guest-info","arguments":[]}`, want: "must be a JSON object"},
		{name: "trailing", input: `{"execute":"guest-info"} {}`, want: "trailing JSON data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeGuestAgentRequest([]byte(tc.input)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DecodeGuestAgentRequest(%s) error = %v, want substring %q", tc.input, err, tc.want)
			}
		})
	}
}

func TestGuestAgentCommandWireReturnEventsAndMismatchedIDs(t *testing.T) {
	path := startQGAServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		syncRequest := readQGASync(t, reader)
		if _, err := fmt.Fprintf(conn, "stale partial garbage\xff"+`{"event":"RESET"}`+"\n"+`{"return":999,"id":999}`+"\n"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
			return err
		}
		request, err := readQGARequest(reader)
		if err != nil {
			return err
		}
		if request.Execute != "guest-file-read" || request.ID == 0 || request.ID == syncRequest.ID {
			return fmt.Errorf("request envelope = %#v", request)
		}
		var args struct {
			Handle uint64 `json:"handle"`
			Count  uint64 `json:"count"`
		}
		if err := json.Unmarshal(request.Args, &args); err != nil {
			return err
		}
		if args.Handle != 7 || args.Count != 4 {
			return fmt.Errorf("request arguments = %#v", args)
		}
		assertJSONKeys(t, request.Args, "handle", "count")
		_, err = fmt.Fprintf(conn, `{"event":"FILE_OPENED"}`+"\n"+`{"return":{},"id":999}`+"\n"+`{"return":{"buf-b64":"AQID","count":3},"id":%d}`+"\n", request.ID)
		return err
	})
	response, err := GuestAgentCommand(context.Background(), path, GuestAgentRequest{
		Execute:   "guest-file-read",
		Arguments: json.RawMessage(`{"handle":7,"count":4}`),
	})
	if err != nil {
		t.Fatalf("GuestAgentCommand: %v", err)
	}
	if string(response) != `{"buf-b64":"AQID","count":3}` {
		t.Fatalf("GuestAgentCommand response = %s", response)
	}
}

func TestGuestAgentCommandStructuredErrorsEOFAndCancellation(t *testing.T) {
	t.Run("structured error", func(t *testing.T) {
		path := startQGAServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			syncRequest := readQGASync(t, reader)
			if _, err := fmt.Fprintf(conn, "\xff"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
				return err
			}
			request, err := readQGARequest(reader)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(conn, `{"error":{"class":"GenericError","desc":"denied"},"id":%d}`+"\n", request.ID)
			return err
		})
		_, err := GuestAgentCommand(context.Background(), path, GuestAgentRequest{Execute: "guest-info"})
		var qgaErr *QGAError
		if !errors.As(err, &qgaErr) || qgaErr.Class != "GenericError" || qgaErr.Desc != "denied" {
			t.Fatalf("GuestAgentCommand structured error = %#v", err)
		}
	})

	t.Run("EOF is an error for ordinary commands", func(t *testing.T) {
		path := startQGAServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			syncRequest := readQGASync(t, reader)
			if _, err := fmt.Fprintf(conn, "\xff"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
				return err
			}
			if _, err := readQGARequest(reader); err != nil {
				return err
			}
			return nil
		})
		_, err := GuestAgentCommand(context.Background(), path, GuestAgentRequest{Execute: "guest-ping"})
		if err == nil || !errors.Is(err, io.EOF) {
			t.Fatalf("GuestAgentCommand EOF error = %v", err)
		}
	})

	t.Run("cancellation interrupts response wait", func(t *testing.T) {
		requestSeen := make(chan struct{})
		path := startQGAServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			syncRequest := readQGASync(t, reader)
			if _, err := fmt.Fprintf(conn, "\xff"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
				return err
			}
			if _, err := readQGARequest(reader); err != nil {
				return err
			}
			close(requestSeen)
			_, err := conn.Read(make([]byte, 1))
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errCh := make(chan error, 1)
		go func() {
			_, err := GuestAgentCommand(ctx, path, GuestAgentRequest{Execute: "guest-info"})
			errCh <- err
		}()
		<-requestSeen
		cancel()
		if err := <-errCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("GuestAgentCommand cancellation = %v", err)
		}
	})
}

func TestGuestAgentCommandNoResponseCommands(t *testing.T) {
	commands := []string{"guest-shutdown", "guest-suspend-disk", "guest-suspend-ram", "guest-suspend-hybrid"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			path := startQGAServer(t, func(conn net.Conn) error {
				reader := bufio.NewReader(conn)
				syncRequest := readQGASync(t, reader)
				if _, err := fmt.Fprintf(conn, "\xff"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
					return err
				}
				request, err := readQGARequest(reader)
				if err != nil {
					return err
				}
				if request.Execute != command {
					return fmt.Errorf("execute = %q", request.Execute)
				}
				return nil
			})
			response, err := GuestAgentCommand(context.Background(), path, GuestAgentRequest{Execute: command})
			if err != nil {
				t.Fatalf("GuestAgentCommand(%q): %v", command, err)
			}
			if string(response) != "null" {
				t.Fatalf("GuestAgentCommand(%q) = %s, want null", command, response)
			}
		})
	}

	t.Run("immediate structured error", func(t *testing.T) {
		path := startQGAServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			syncRequest := readQGASync(t, reader)
			if _, err := fmt.Fprintf(conn, "\xff"+`{"return":%d,"id":%d}`+"\n", syncRequest.ID, syncRequest.ID); err != nil {
				return err
			}
			request, err := readQGARequest(reader)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(conn, `{"error":{"class":"CommandDisabled","desc":"blocked"},"id":%d}`+"\n", request.ID)
			return err
		})
		_, err := GuestAgentCommand(context.Background(), path, GuestAgentRequest{Execute: "guest-shutdown"})
		var qgaErr *QGAError
		if !errors.As(err, &qgaErr) || qgaErr.Class != "CommandDisabled" || qgaErr.Desc != "blocked" {
			t.Fatalf("GuestAgentCommand no-response structured error = %#v", err)
		}
	})
}

func TestQGANoResponsePeerResetIsSuccess(t *testing.T) {
	for _, err := range []error{
		syscall.ECONNRESET,
		fmt.Errorf("read QGA response: %w", syscall.ECONNABORTED),
	} {
		if !qgaNoResponseSuccess(err) {
			t.Fatalf("qgaNoResponseSuccess(%v) = false", err)
		}
	}
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

func TestQGAFrameSizeCaps(t *testing.T) {
	t.Run("delimiter", func(t *testing.T) {
		reader := bufio.NewReader(bytes.NewReader([]byte{'a', 'b', 'c', 'd', 'e', 0xff}))
		if err := readUntilQGADelimiter(reader, 4); err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
			t.Fatalf("readUntilQGADelimiter error = %v", err)
		}
	})

	t.Run("newline terminated", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader("12345\n"))
		if _, err := readQGAFrame(reader, 4); err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
			t.Fatalf("readQGAFrame newline error = %v", err)
		}
	})

	t.Run("unterminated", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader("12345"))
		if _, err := readQGAFrame(reader, 4); err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
			t.Fatalf("readQGAFrame unterminated error = %v", err)
		}
	})
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
