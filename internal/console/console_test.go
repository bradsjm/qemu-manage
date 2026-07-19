package console

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func startConsoleServer(t *testing.T, serve func(net.Conn) error) string {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "qm-c-")
	if err != nil {
		t.Fatalf("create socket root: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove socket root: %v", err)
		}
	})
	path := filepath.Join(root, "console.sock")
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
			t.Errorf("console server: %v", err)
		}
	})
	return path
}

type signalBuffer struct {
	bytes.Buffer
	written chan struct{}
	once    sync.Once
}

func (b *signalBuffer) Write(p []byte) (int, error) {
	n, err := b.Buffer.Write(p)
	if n > 0 {
		b.once.Do(func() {
			close(b.written)
		})
	}
	return n, err
}

func TestConnectAndConnectMonitorProxyBytes(t *testing.T) {
	cases := []struct {
		name    string
		connect func(context.Context, string, io.Reader, io.Writer) error
	}{
		{name: "console", connect: Connect},
		{name: "monitor", connect: ConnectMonitor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			outputWritten := make(chan struct{})
			path := startConsoleServer(t, func(conn net.Conn) error {
				buffer := make([]byte, len("from-client"))
				if _, err := io.ReadFull(conn, buffer); err != nil {
					return err
				}
				received <- string(buffer)
				if _, err := io.WriteString(conn, "from-guest"); err != nil {
					return err
				}
				close(outputWritten)
				return nil
			})

			stdin, input := net.Pipe()
			t.Cleanup(func() {
				_ = stdin.Close()
				_ = input.Close()
			})
			stdout := &signalBuffer{written: make(chan struct{})}
			go func() {
				_, _ = input.Write([]byte("from-client"))
				<-stdout.written
				_ = input.Close()
			}()
			if err := tc.connect(context.Background(), path, stdin, stdout); err != nil {
				t.Fatalf("connect: %v", err)
			}
			if stdout.String() != "from-guest" {
				t.Fatalf("stdout = %q, want %q", stdout.String(), "from-guest")
			}
			if got := <-received; got != "from-client" {
				t.Fatalf("guest input = %q, want %q", got, "from-client")
			}
		})
	}
}

func TestConnectAndConnectMonitorCtrlDisconnectStopsForwarding(t *testing.T) {
	cases := []struct {
		name    string
		connect func(context.Context, string, io.Reader, io.Writer) error
	}{
		{name: "console", connect: Connect},
		{name: "monitor", connect: ConnectMonitor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			path := startConsoleServer(t, func(conn net.Conn) error {
				data, err := io.ReadAll(conn)
				if err != nil {
					return err
				}
				received <- string(data)
				return nil
			})

			stdin := bytes.NewReader(append(append([]byte("from-client"), disconnectByte), []byte("after-disconnect")...))
			if err := tc.connect(context.Background(), path, stdin, io.Discard); err != nil {
				t.Fatalf("connect: %v", err)
			}
			if got := <-received; got != "from-client" {
				t.Fatalf("guest input = %q, want %q", got, "from-client")
			}
		})
	}
}

func TestConnectPrefixesErrorsBySocketKind(t *testing.T) {
	cases := []struct {
		name    string
		connect func(context.Context, string, io.Reader, io.Writer) error
		prefix  string
	}{
		{name: "console", connect: Connect, prefix: "console"},
		{name: "monitor", connect: ConnectMonitor, prefix: "monitor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.connect(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), strings.NewReader(""), io.Discard)
			if err == nil {
				t.Fatal("connect unexpectedly succeeded")
			}
			if !strings.Contains(err.Error(), tc.prefix+": connect:") {
				t.Fatalf("error = %q, want prefix %q", err.Error(), tc.prefix+": connect:")
			}
		})
	}
}
