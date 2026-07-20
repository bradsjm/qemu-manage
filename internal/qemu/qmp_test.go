package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/model"
)

type qmpTestServer struct {
	path string
	done chan error
}

func startQMPServer(t *testing.T, serve func(net.Conn) error) qmpTestServer {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "qm-p-")
	if err != nil {
		t.Fatalf("create QMP socket root: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(root)
	})
	path := filepath.Join(root, "qmp.sock")
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
			t.Errorf("QMP server: %v", err)
		}
	})
	return qmpTestServer{path: path, done: done}
}

func readQMPCommand(reader *bufio.Reader) (qmpCommand, error) {
	var command qmpCommand
	err := json.NewDecoder(reader).Decode(&command)
	return command, err
}

func qmpGreetingJSON() string {
	return `{"QMP":{"version":{"qemu":{"major":11,"minor":0,"micro":1},"package":"test"},"capabilities":[]}}` + "\n"
}

func initializeQMP(conn net.Conn, reader *bufio.Reader) error {
	if _, err := conn.Write([]byte(qmpGreetingJSON())); err != nil {
		return err
	}
	command, err := readQMPCommand(reader)
	if err != nil {
		return err
	}
	if command.Execute != "qmp_capabilities" || command.ID != 1 {
		return fmt.Errorf("capabilities command = %#v", command)
	}
	_, err = fmt.Fprintf(conn, `{"return":{},"id":%d}`+"\n", command.ID)
	return err
}

func TestQMPFramingEventsIDsAndStatusMapping(t *testing.T) {
	statuses := []string{"running", "paused", "shutdown"}
	server := startQMPServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		greeting := qmpGreetingJSON()
		if _, err := conn.Write([]byte(greeting[:13])); err != nil {
			return err
		}
		if _, err := conn.Write([]byte(greeting[13:])); err != nil {
			return err
		}
		command, err := readQMPCommand(reader)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(conn, `{"event":"RESET","data":{}}`+"\n"+`{"return":{},"id":999}`+"\n"+`{"return":{},"id":%d}`+"\n", command.ID); err != nil {
			return err
		}
		for _, status := range statuses {
			command, err = readQMPCommand(reader)
			if err != nil {
				return err
			}
			if command.Execute != "query-status" {
				return fmt.Errorf("execute = %q", command.Execute)
			}
			if _, err = fmt.Fprintf(conn, `{"event":"STOP"}`+"\n"+`{"return":{"status":%q},"id":0}`+"\n"+`{"return":{"status":%q},"id":%d}`+"\n", status, status, command.ID); err != nil {
				return err
			}
		}
		return nil
	})
	client, err := NewQMPClient(server.path)
	if err != nil {
		t.Fatalf("NewQMPClient: %v", err)
	}
	defer client.Close()
	for _, tc := range []struct {
		want       model.RunState
		unexpected string
	}{{model.RunStateRunning, ""}, {model.RunStatePaused, ""}, {model.RunStateFailed, "shutdown"}} {
		got, err := client.Status(context.Background())
		if tc.unexpected == "" {
			if err != nil || got != tc.want {
				t.Errorf("Status = %q, %v; want %q, nil", got, err, tc.want)
			}
		} else {
			var statusErr *UnexpectedStatusError
			if got != model.RunStateFailed || !errors.As(err, &statusErr) || statusErr.Status != tc.unexpected {
				t.Errorf("unexpected status = %q, %v", got, err)
			}
		}
	}
}

func TestQMPMonitoringQueriesAndEvents(t *testing.T) {
	server := startQMPServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if err := initializeQMP(conn, reader); err != nil {
			return err
		}
		responses := []string{
			`{"event":"SHUTDOWN"}` + "\n" +
				`{"event":"BLOCK_IO_ERROR","data":{"device":"disk-b","operation":"write","nospace":true,"description":"redacted"}}` + "\n" +
				`{"return":{"status":"guest-panicked","unknown":true},"id":%d}` + "\n",
			`{"return":[{"device":"disk-b","stats":{"rd_bytes":0,"wr_operations":2,"rd_total_time_ns":1000000000},"idle_time_ns":0,"unknown":true},{"device":"","stats":{}},{"device":"disk-a","stats":{"unmap_bytes":3,"failed_rd_operations":0}}],"id":%d}` + "\n",
			`{"return":[{"device":"disk-b","io-status":"ok"},{"device":"disk-a"},{"device":""}],"id":%d}` + "\n",
		}
		for index, response := range responses {
			command, err := readQMPCommand(reader)
			if err != nil {
				return err
			}
			want := []string{"query-status", "query-blockstats", "query-block"}[index]
			if command.Execute != want {
				return fmt.Errorf("command %d = %q, want %q", index, command.Execute, want)
			}
			if _, err := fmt.Fprintf(conn, response, command.ID); err != nil {
				return err
			}
		}
		return nil
	})
	client, err := NewQMPClient(server.path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if got := client.Version(); got.Major != 11 || got.Minor != 0 || got.Micro != 1 || got.Package != "test" {
		t.Fatalf("Version() = %#v", got)
	}
	status, err := client.RawStatus(context.Background())
	if err != nil || status != "guest-panicked" {
		t.Fatalf("RawStatus() = %q, %v", status, err)
	}
	blocks, err := client.QueryBlocks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[0].Device != "disk-a" || blocks[1].Device != "disk-b" {
		t.Fatalf("blocks not filtered and sorted: %#v", blocks)
	}
	if blocks[0].UnmapBytes == nil || *blocks[0].UnmapBytes != 3 ||
		blocks[0].FailedOperations["read"] != 0 || blocks[0].ReadBytes != nil {
		t.Fatalf("optional samples not preserved: %#v", blocks[0])
	}
	if blocks[1].ReadBytes == nil || *blocks[1].ReadBytes != 0 ||
		blocks[1].ReadSeconds == nil || *blocks[1].ReadSeconds != 1 ||
		blocks[1].IdleSeconds == nil || *blocks[1].IdleSeconds != 0 ||
		blocks[1].IOStatus == nil || *blocks[1].IOStatus != "ok" {
		t.Fatalf("block normalization failed: %#v", blocks[1])
	}
	events, err := client.EventCounters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if events.Lifecycle["shutdown"] != 1 || events.Lifecycle["reset"] != 0 {
		t.Fatalf("lifecycle events = %#v", events.Lifecycle)
	}
	if len(events.BlockIO) != 1 || events.BlockIO[0].Device != "disk-b" ||
		events.BlockIO[0].Operation != "write" || !events.BlockIO[0].NoSpace || events.BlockIO[0].Count != 1 {
		t.Fatalf("block I/O events = %#v", events.BlockIO)
	}
}

func TestQMPMonitoringRejectsNegativeCounters(t *testing.T) {
	server := startQMPServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if err := initializeQMP(conn, reader); err != nil {
			return err
		}
		command, err := readQMPCommand(reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn, `{"return":[{"device":"disk","stats":{"rd_bytes":-1}}],"id":%d}`+"\n", command.ID)
		return err
	})
	client, err := NewQMPClient(server.path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.QueryBlocks(context.Background()); err == nil || !strings.Contains(err.Error(), "must be nonnegative") {
		t.Fatalf("QueryBlocks() error = %v", err)
	}
}

func TestQMPQueryVNCFramingAndFiltering(t *testing.T) {
	server := startQMPServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if err := initializeQMP(conn, reader); err != nil {
			return err
		}
		command, err := readQMPCommand(reader)
		if err != nil {
			return err
		}
		if command.Execute != "query-vnc" || command.ID != 2 {
			return fmt.Errorf("query-vnc command = %#v", command)
		}
		response := fmt.Sprintf(
			`{"event":"VNC_INITIALIZED"}`+"\n"+
				`{"return":{"enabled":true,"host":"127.0.0.1","service":"5907","family":"ipv4","auth":"vnc","clients":[]},"id":0}`+"\n"+
				`{"return":{"enabled":true,"host":"127.0.0.1","service":"5907","family":"ipv4","auth":"vnc","clients":[]},"id":%d}`+"\n",
			command.ID,
		)
		if _, err := conn.Write([]byte(response[:39])); err != nil {
			return err
		}
		_, err = conn.Write([]byte(response[39:]))
		return err
	})
	client, err := NewQMPClient(server.path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	info, err := client.QueryVNC(context.Background())
	if err != nil {
		t.Fatalf("QueryVNC: %v", err)
	}
	want := VNCInfo{Enabled: true, Host: "127.0.0.1", Service: "5907", Family: "ipv4", Auth: "vnc"}
	if info != want {
		t.Fatalf("QueryVNC = %#v, want %#v", info, want)
	}
}

func TestQMPQueryVNCResponseValidation(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		server := startQMPServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			if err := initializeQMP(conn, reader); err != nil {
				return err
			}
			command, err := readQMPCommand(reader)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(conn, `{"return":{"enabled":false},"id":%d}`+"\n", command.ID)
			return err
		})
		client, err := NewQMPClient(server.path)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		info, err := client.QueryVNC(context.Background())
		if err != nil {
			t.Fatalf("QueryVNC: %v", err)
		}
		if info.Enabled || info.Host != "" || info.Service != "" || info.Family != "" || info.Auth != "" {
			t.Fatalf("disabled QueryVNC = %#v", info)
		}
	})

	cases := []struct {
		name     string
		response string
		want     string
	}{
		{
			name:     "non-object",
			response: `{"return":1,"id":%d}` + "\n",
			want:     "decode query-vnc response",
		},
		{
			name:     "unknown field",
			response: `{"return":{"enabled":false,"unexpected":true},"id":%d}` + "\n",
			want:     "unknown field",
		},
		{
			name:     "missing enabled fields",
			response: `{"return":{"enabled":true,"service":"5907","family":"ipv4","auth":"vnc","clients":[]},"id":%d}` + "\n",
			want:     "missing enabled VNC fields",
		},
		{
			name:     "connected clients",
			response: `{"return":{"enabled":true,"host":"127.0.0.1","service":"5907","family":"ipv4","auth":"vnc","clients":[{}]},"id":%d}` + "\n",
			want:     "connected clients",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := startQMPServer(t, func(conn net.Conn) error {
				reader := bufio.NewReader(conn)
				if err := initializeQMP(conn, reader); err != nil {
					return err
				}
				command, err := readQMPCommand(reader)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(conn, tc.response, command.ID)
				return err
			})
			client, err := NewQMPClient(server.path)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.QueryVNC(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("QueryVNC error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestQMPGreetingAndCapabilityFailures(t *testing.T) {
	cases := []struct{ name, greeting, response string }{
		{"missing version", `{"QMP":{"capabilities":[]}}` + "\n", ""},
		{"malformed greeting", "not-json\n", ""},
		{"capability error", qmpGreetingJSON(), `{"error":{"class":"CommandNotFound","desc":"disabled"},"id":1}` + "\n"},
		{"invalid capability result", qmpGreetingJSON(), `{"return":null,"id":1}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := startQMPServer(t, func(conn net.Conn) error {
				if _, err := conn.Write([]byte(tc.greeting)); err != nil {
					return err
				}
				if tc.response != "" {
					if _, err := readQMPCommand(bufio.NewReader(conn)); err != nil {
						return err
					}
					_, err := conn.Write([]byte(tc.response))
					return err
				}
				return nil
			})
			client, err := NewQMPClient(server.path)
			if client != nil {
				_ = client.Close()
			}
			if err == nil {
				t.Fatal("NewQMPClient unexpectedly succeeded")
			}
		})
	}
}

func TestQMPStructuredErrorPreserved(t *testing.T) {
	server := startQMPServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if err := initializeQMP(conn, reader); err != nil {
			return err
		}
		command, err := readQMPCommand(reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn, `{"error":{"class":"GenericError","desc":"power denied","extra":7},"id":%d}`+"\n", command.ID)
		return err
	})
	client, err := NewQMPClient(server.path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.SystemPowerdown(context.Background())
	var qmpErr *QMPError
	if !errors.As(err, &qmpErr) || qmpErr.Class != "GenericError" || qmpErr.Description != "power denied" || !json.Valid(qmpErr.Data) {
		t.Fatalf("error = %#v", err)
	}
	if !jsonContainsKey(qmpErr.Data, "extra") {
		t.Fatalf("structured error data lost: %s", qmpErr.Data)
	}
}

func TestQMPHumanMonitorCommand(t *testing.T) {
	t.Run("success and filtering", func(t *testing.T) {
		server := startQMPServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			if err := initializeQMP(conn, reader); err != nil {
				return err
			}
			command, err := readQMPCommand(reader)
			if err != nil {
				return err
			}
			if command.Execute != "human-monitor-command" || command.ID != 2 {
				return fmt.Errorf("monitor command = %#v", command)
			}
			if len(command.Arguments) != 1 || command.Arguments["command-line"] != "  info status  " {
				return fmt.Errorf("monitor arguments = %#v", command.Arguments)
			}
			_, err = fmt.Fprintf(conn, `{"event":"RESET"}`+"\n"+`{"return":"ignore","id":999}`+"\n"+`{"return":"status: running","id":%d}`+"\n", command.ID)
			return err
		})
		client, err := NewQMPClient(server.path)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		output, err := client.HumanMonitorCommand(context.Background(), "  info status  ")
		if err != nil {
			t.Fatalf("HumanMonitorCommand: %v", err)
		}
		if output != "status: running" {
			t.Fatalf("HumanMonitorCommand = %q", output)
		}
	})

	t.Run("blank command rejected", func(t *testing.T) {
		server := startQMPServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			if err := initializeQMP(conn, reader); err != nil {
				return err
			}
			_, err := conn.Read(make([]byte, 1))
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		})
		client, err := NewQMPClient(server.path)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.HumanMonitorCommand(context.Background(), " \t "); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("blank HumanMonitorCommand error = %v", err)
		}
	})

	t.Run("non-string response rejected", func(t *testing.T) {
		server := startQMPServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			if err := initializeQMP(conn, reader); err != nil {
				return err
			}
			command, err := readQMPCommand(reader)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(conn, `{"return":1,"id":%d}`+"\n", command.ID)
			return err
		})
		client, err := NewQMPClient(server.path)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.HumanMonitorCommand(context.Background(), "info version"); err == nil || !strings.Contains(err.Error(), "decode human-monitor-command response") {
			t.Fatalf("non-string HumanMonitorCommand error = %v", err)
		}
	})

	t.Run("null response rejected", func(t *testing.T) {
		server := startQMPServer(t, func(conn net.Conn) error {
			reader := bufio.NewReader(conn)
			if err := initializeQMP(conn, reader); err != nil {
				return err
			}
			command, err := readQMPCommand(reader)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(conn, `{"return":null,"id":%d}`+"\n", command.ID)
			return err
		})
		client, err := NewQMPClient(server.path)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.HumanMonitorCommand(context.Background(), "info version"); err == nil || !strings.Contains(err.Error(), "return must be a JSON string") {
			t.Fatalf("null HumanMonitorCommand error = %v", err)
		}
	})
}

func TestNewQMPClientContextCancellationDuringGreeting(t *testing.T) {
	accepted := make(chan struct{})
	server := startQMPServer(t, func(conn net.Conn) error {
		close(accepted)
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
		client, err := NewQMPClientContext(ctx, server.path)
		if client != nil {
			_ = client.Close()
		}
		errCh <- err
	}()
	<-accepted
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("NewQMPClientContext greeting cancellation = %v", err)
	}
}

func TestNewQMPClientContextCancellationDuringCapabilityNegotiation(t *testing.T) {
	capabilitiesSeen := make(chan struct{})
	server := startQMPServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if _, err := conn.Write([]byte(qmpGreetingJSON())); err != nil {
			return err
		}
		command, err := readQMPCommand(reader)
		if err != nil {
			return err
		}
		if command.Execute != "qmp_capabilities" || command.ID != 1 {
			return fmt.Errorf("capabilities command = %#v", command)
		}
		close(capabilitiesSeen)
		_, err = conn.Read(make([]byte, 1))
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		client, err := NewQMPClientContext(ctx, server.path)
		if client != nil {
			_ = client.Close()
		}
		errCh <- err
	}()
	<-capabilitiesSeen
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("NewQMPClientContext capability cancellation = %v", err)
	}
}

func TestQMPCancelledQueuedCommandDoesNotConsumeStream(t *testing.T) {
	firstSeen := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var ids []int64
	server := startQMPServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if err := initializeQMP(conn, reader); err != nil {
			return err
		}
		first, err := readQMPCommand(reader)
		if err != nil {
			return err
		}
		mu.Lock()
		ids = append(ids, first.ID)
		mu.Unlock()
		close(firstSeen)
		<-releaseFirst
		if _, err = fmt.Fprintf(conn, `{"return":{},"id":%d}`+"\n", first.ID); err != nil {
			return err
		}
		third, err := readQMPCommand(reader)
		if err != nil {
			return err
		}
		mu.Lock()
		ids = append(ids, third.ID)
		mu.Unlock()
		_, err = fmt.Fprintf(conn, `{"return":{},"id":%d}`+"\n", third.ID)
		return err
	})
	client, err := NewQMPClient(server.path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	firstDone := make(chan error, 1)
	go func() { firstDone <- client.SystemPowerdown(context.Background()) }()
	<-firstSeen
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Quit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation = %v", err)
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first command: %v", err)
	}
	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancelDeadline()
	if err := client.Quit(deadline); err != nil {
		t.Fatalf("command after cancellation: %v", err)
	}
	mu.Lock()
	got := append([]int64(nil), ids...)
	mu.Unlock()
	if fmt.Sprint(got) != "[2 3]" {
		t.Fatalf("wire IDs = %v; canceled waiter consumed an ID", got)
	}
}

func jsonContainsKey(data []byte, key string) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(data, &object) == nil && object[key] != nil
}
