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
	"strings"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
)

const (
	helperEnvEnabled      = "QEMU_MANAGE_TEST_QEMU_HELPER"
	helperEnvScenario     = "QEMU_MANAGE_TEST_QEMU_SCENARIO"
	helperEnvQMPPath      = "QEMU_MANAGE_TEST_QEMU_QMP"
	helperEnvSecretPath   = "QEMU_MANAGE_TEST_QEMU_SECRET"
	helperEnvObservePath  = "QEMU_MANAGE_TEST_QEMU_OBSERVE"
	helperEnvQuitMarker   = "QEMU_MANAGE_TEST_QEMU_QUIT_MARKER"
	helperScenarioSuccess = "success"
)

type helperSecretObservation struct {
	Bytes string `json:"bytes"`
	Mode  uint32 `json:"mode"`
}

func TestQEMUHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvEnabled) != "1" {
		return
	}
	if err := runQEMUHelperProcess(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func runQEMUHelperProcess() error {
	scenario := os.Getenv(helperEnvScenario)
	qmpPath := os.Getenv(helperEnvQMPPath)
	secretPath := os.Getenv(helperEnvSecretPath)
	observePath := os.Getenv(helperEnvObservePath)
	quitMarker := os.Getenv(helperEnvQuitMarker)

	if observePath != "" {
		info, err := os.Stat(secretPath)
		if err != nil {
			return fmt.Errorf("observe secret: %w", err)
		}
		data, err := os.ReadFile(secretPath)
		if err != nil {
			return fmt.Errorf("read secret: %w", err)
		}
		encoded, err := json.Marshal(helperSecretObservation{Bytes: string(data), Mode: uint32(info.Mode().Perm())})
		if err != nil {
			return fmt.Errorf("marshal observation: %w", err)
		}
		if err := os.WriteFile(observePath, encoded, 0o600); err != nil {
			return fmt.Errorf("write observation: %w", err)
		}
	}

	if err := os.Remove(qmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale QMP socket: %w", err)
	}
	listener, err := net.Listen("unix", qmpPath)
	if err != nil {
		return fmt.Errorf("listen QMP socket: %w", err)
	}
	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		return fmt.Errorf("accept QMP connection: %w", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if _, err := conn.Write([]byte(qmpGreetingJSON())); err != nil {
		return fmt.Errorf("write QMP greeting: %w", err)
	}
	for {
		command, err := readQMPCommand(reader)
		if err != nil {
			return fmt.Errorf("read QMP command: %w", err)
		}
		switch command.Execute {
		case "qmp_capabilities":
			if _, err := fmt.Fprintf(conn, `{"return":{},"id":%d}`+"\n", command.ID); err != nil {
				return fmt.Errorf("write qmp_capabilities response: %w", err)
			}
		case "query-status":
			if _, err := fmt.Fprintf(conn, `{"return":{"status":"running"},"id":%d}`+"\n", command.ID); err != nil {
				return fmt.Errorf("write query-status response: %w", err)
			}
		case "query-vnc":
			if _, err := fmt.Fprintf(conn, `{"return":%s,"id":%d}`+"\n", helperVNCResponse(scenario), command.ID); err != nil {
				return fmt.Errorf("write query-vnc response: %w", err)
			}
		case "quit":
			if quitMarker != "" {
				if err := os.WriteFile(quitMarker, []byte("quit"), 0o600); err != nil {
					return fmt.Errorf("write quit marker: %w", err)
				}
			}
			if _, err := fmt.Fprintf(conn, `{"return":{},"id":%d}`+"\n", command.ID); err != nil {
				return fmt.Errorf("write quit response: %w", err)
			}
			return nil
		default:
			return fmt.Errorf("unexpected QMP command %q", command.Execute)
		}
	}
}

func helperVNCResponse(scenario string) string {
	switch scenario {
	case helperScenarioSuccess, "cleanup-remove-fail":
		return `{"enabled":true,"host":"127.0.0.1","service":"5907","family":"ipv4","auth":"vnc","clients":[]}`
	case "disabled":
		return `{"enabled":false}`
	case "wrong-family":
		return `{"enabled":true,"host":"127.0.0.1","service":"5907","family":"ipv6","auth":"vnc","clients":[]}`
	case "wrong-auth":
		return `{"enabled":true,"host":"127.0.0.1","service":"5907","family":"ipv4","auth":"sasl","clients":[]}`
	case "out-of-range":
		return `{"enabled":true,"host":"127.0.0.1","service":"6000","family":"ipv4","auth":"vnc","clients":[]}`
	default:
		return `{"enabled":true,"host":"127.0.0.1","service":"5907","family":"ipv4","auth":"vnc","clients":[]}`
	}
}

func TestStartVNCSecretLifecycleAndEndpointCaching(t *testing.T) {
	config, paths, command, observationPath, _ := startVNCInstanceFixture(t, helperScenarioSuccess)
	backendImpl := &Backend{StartTimeout: time.Second}
	started, err := backendImpl.Start(context.Background(), config, paths, command)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	instance, ok := started.(*instance)
	if !ok {
		t.Fatalf("instance type = %T", started)
	}
	defer func() {
		if err := instance.ForceStop(context.Background()); err != nil {
			t.Fatalf("ForceStop: %v", err)
		}
	}()
	observation := readHelperSecretObservation(t, observationPath)
	if observation.Bytes != config.VNC.Password {
		t.Fatalf("secret bytes = %q, want %q", observation.Bytes, config.VNC.Password)
	}
	if observation.Mode != 0o600 {
		t.Fatalf("secret mode = %04o, want 0600", observation.Mode)
	}
	if _, err := os.Stat(paths.VNCSecret); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("secret path still exists after start: %v", err)
	}
	if !instance.hasVNC || instance.vncHost != "127.0.0.1" || instance.vncPort != 5907 {
		t.Fatalf("cached endpoint = host %q port %d enabled %v", instance.vncHost, instance.vncPort, instance.hasVNC)
	}
}

func TestStartRejectsExistingOrSymlinkVNCSecretPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, path string)
	}{
		{
			name: "existing file",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("occupied"), 0o600); err != nil {
					t.Fatalf("write existing secret: %v", err)
				}
			},
		},
		{
			name: "symlink",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("occupied"), 0o600); err != nil {
					t.Fatalf("write target: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink secret: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, paths, _, _, _ := startVNCInstanceFixture(t, helperScenarioSuccess)
			tc.prepare(t, paths.VNCSecret)
			_, err := (&Backend{StartTimeout: time.Second}).Start(context.Background(), config, paths, backend.Command{Path: "/definitely/missing/qemu"})
			if err == nil || !strings.Contains(err.Error(), "create VNC secret") {
				t.Fatalf("Start error = %v, want secret creation failure", err)
			}
		})
	}
}

func TestStartForcesStopOnVNCVerificationOrCleanupFailure(t *testing.T) {
	cases := []struct {
		scenario        string
		wantErrorSubstr string
		wantSecretGone  bool
		removeFile      func(string) error
	}{
		{scenario: "disabled", wantErrorSubstr: "VNC is disabled", wantSecretGone: true},
		{scenario: "wrong-family", wantErrorSubstr: `family "ipv6"`, wantSecretGone: true},
		{scenario: "wrong-auth", wantErrorSubstr: `auth "sasl"`, wantSecretGone: true},
		{scenario: "out-of-range", wantErrorSubstr: "outside 5900-5909", wantSecretGone: true},
		{
			scenario:        "cleanup-remove-fail",
			wantErrorSubstr: "remove VNC secret",
			removeFile: func(string) error {
				return errors.New("injected removal failure")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.scenario, func(t *testing.T) {
			config, paths, command, _, quitMarker := startVNCInstanceFixture(t, tc.scenario)
			_, err := (&Backend{StartTimeout: time.Second, removeFile: tc.removeFile}).Start(context.Background(), config, paths, command)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrorSubstr) {
				t.Fatalf("Start error = %v, want substring %q", err, tc.wantErrorSubstr)
			}
			if _, err := os.Stat(quitMarker); err != nil {
				t.Fatalf("quit marker missing after forced stop: %v", err)
			}
			_, err = os.Stat(paths.VNCSecret)
			if tc.wantSecretGone {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("secret still exists after failed start: %v", err)
				}
			} else if err != nil {
				t.Fatalf("secret missing after cleanup failure: %v", err)
			}
		})
	}
}

func startVNCInstanceFixture(t *testing.T, scenario string) (*model.Config, backend.RuntimePaths, backend.Command, string, string) {
	t.Helper()
	root := t.TempDir()
	qmpPath := filepath.Join(root, "qmp.sock")
	secretPath := filepath.Join(root, "vnc-password")
	logPath := filepath.Join(root, "qemu.log")
	observationPath := filepath.Join(root, "secret.json")
	quitMarker := filepath.Join(root, "quit.marker")
	t.Setenv(helperEnvEnabled, "1")
	t.Setenv(helperEnvScenario, scenario)
	t.Setenv(helperEnvQMPPath, qmpPath)
	t.Setenv(helperEnvSecretPath, secretPath)
	t.Setenv(helperEnvObservePath, observationPath)
	t.Setenv(helperEnvQuitMarker, quitMarker)
	config := &model.Config{
		GuestAgent: model.GuestAgentConfig{Enabled: false},
		VNC: &model.VNCConfig{
			Bind:     "127.0.0.1",
			Port:     5900,
			PortTo:   5909,
			Password: "secret12",
		},
	}
	paths := backend.RuntimePaths{
		QMP:       qmpPath,
		QGA:       filepath.Join(root, "qga.sock"),
		QEMULog:   logPath,
		VNCSecret: secretPath,
	}
	command := backend.Command{Path: os.Args[0], Args: []string{"-test.run=TestQEMUHelperProcess", "--", "qemu-helper"}}
	return config, paths, command, observationPath, quitMarker
}

func readHelperSecretObservation(t *testing.T, path string) helperSecretObservation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read observation: %v", err)
	}
	var observation helperSecretObservation
	if err := json.Unmarshal(data, &observation); err != nil {
		t.Fatalf("decode observation: %v", err)
	}
	return observation
}
