package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/store"
)

func privateRuntimeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMetadataAtomicRoundTripAndExactMode(t *testing.T) {
	dir := privateRuntimeDir(t)
	path := filepath.Join(dir, "runtime.json")
	want := RuntimeMetadata{Version: metadataVersion, ID: testProtocolID, Name: "vm.one", SupervisorPID: 42, StartedAt: time.Date(2026, 2, 3, 4, 5, 6, 7, time.UTC)}
	if err := WriteRuntimeMetadata(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
	got, err := ReadRuntimeMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "runtime.json" {
		t.Fatalf("directory entries after atomic write = %v", entries)
	}
}

func TestMetadataRequiresExactDirectoryAndFileModes(t *testing.T) {
	dir := privateRuntimeDir(t)
	path := filepath.Join(dir, "runtime.json")
	data := []byte(`{"version":1,"id":"` + testProtocolID + `","name":"vm","supervisor_pid":1,"started_at":"2026-01-01T00:00:00Z"}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRuntimeMetadata(path); err == nil || !strings.Contains(err.Error(), "want 0600") {
		t.Fatalf("file-mode error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRuntimeMetadata(path); err == nil || !strings.Contains(err.Error(), "want 0700") {
		t.Fatalf("directory-mode error = %v", err)
	}
}

func TestMetadataNoFollow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions are Unix-specific")
	}
	dir := privateRuntimeDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "runtime.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	metadata := RuntimeMetadata{Version: 1, ID: testProtocolID, Name: "vm", SupervisorPID: 1, StartedAt: time.Now().UTC()}
	if err := WriteRuntimeMetadata(link, metadata); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("write symlink error = %v", err)
	}
	if _, err := ReadRuntimeMetadata(link); err == nil {
		t.Fatal("read followed a metadata symlink")
	}
}

func TestMetadataIdentityAndFutureTimestampRemainObservable(t *testing.T) {
	dir := privateRuntimeDir(t)
	path := filepath.Join(dir, "runtime.json")
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	metadata := RuntimeMetadata{Version: 1, ID: strings.Repeat("b", 32), Name: "other", SupervisorPID: 9, StartedAt: future}
	if err := WriteRuntimeMetadata(path, metadata); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRuntimeMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == testProtocolID {
		t.Fatal("mismatched identity was silently rewritten")
	}
	if !got.StartedAt.Equal(future) {
		t.Fatalf("future timestamp = %s, want %s", got.StartedAt, future)
	}
}

func TestCleanupRuntimePreservesLastExitAndLifetimeLock(t *testing.T) {
	dir := privateRuntimeDir(t)
	paths := store.Paths{
		RuntimeDir: dir, ControlSocket: filepath.Join(dir, "control.sock"), QMP: filepath.Join(dir, "qmp.sock"),
		QMPCommand: filepath.Join(dir, "qmp-command.sock"), QGA: filepath.Join(dir, "qga.sock"),
		Console: filepath.Join(dir, "console.sock"), Monitor: filepath.Join(dir, "monitor.sock"),
		VNCSecret: filepath.Join(dir, "vnc-password"), RuntimeMetadata: filepath.Join(dir, "runtime.json"),
		LastExitMetadata: filepath.Join(dir, "last_exit.json"), LifetimeLock: filepath.Join(dir, "lifetime.lock"),
	}
	for _, path := range []string{paths.ControlSocket, paths.QMP, paths.QMPCommand, paths.QGA, paths.Console, paths.Monitor, paths.VNCSecret, paths.RuntimeMetadata, paths.LifetimeLock} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	last := LastExitMetadata{Version: 1, ID: testProtocolID, Timestamp: time.Now().UTC(), ExitCode: 1, Error: "boom"}
	if err := WriteLastExit(paths.LastExitMetadata, last); err != nil {
		t.Fatal(err)
	}
	if err := CleanupRuntime(paths); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.ControlSocket, paths.QMP, paths.QMPCommand, paths.QGA, paths.Console, paths.Monitor, paths.VNCSecret, paths.RuntimeMetadata} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ephemeral %s remains: %v", path, err)
		}
	}
	for _, path := range []string{paths.LastExitMetadata, paths.LifetimeLock} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved %s: %v", path, err)
		}
	}
}

func TestClearFailedLastExitOnlyClearsFailure(t *testing.T) {
	dir := privateRuntimeDir(t)
	path := filepath.Join(dir, "last_exit.json")
	now := time.Now().UTC()
	if err := WriteLastExit(path, LastExitMetadata{Version: 1, ID: testProtocolID, Timestamp: now, ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	if err := ClearFailedLastExit(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("zero last exit removed: %v", err)
	}
	if err := WriteLastExit(path, LastExitMetadata{Version: 1, ID: testProtocolID, Timestamp: now, ExitCode: 2, Error: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearFailedLastExit(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed last exit remains: %v", err)
	}
}
