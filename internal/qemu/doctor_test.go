package qemu

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRunWithParentCapabilityHelpExitStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		output         string
		exitStatus     int
		wantStatus     CheckStatus
		wantEvidence   string
		rejectEvidence string
	}{
		{
			name:         "capability output is authoritative despite exit one",
			output:       "exit-with-parent=on|off\n",
			exitStatus:   1,
			wantStatus:   CheckPass,
			wantEvidence: "exit-with-parent is supported",
		},
		{
			name:           "exit one without capability remains a failure",
			output:         "available options: other-option\n",
			exitStatus:     1,
			wantStatus:     CheckFail,
			wantEvidence:   "other-option",
			rejectEvidence: "exit-with-parent is supported",
		},
		{
			name:         "exit zero with capability passes",
			output:       "available options: exit-with-parent\n",
			exitStatus:   0,
			wantStatus:   CheckPass,
			wantEvidence: "exit-with-parent is supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			binary := writeCapabilityFixture(t, tt.output, tt.exitStatus)

			got := capabilityCheck(context.Background(), binary, "run_with_parent", []string{"-run-with", "help"}, "exit-with-parent")

			if got.Name != "run_with_parent" {
				t.Fatalf("check name = %q, want %q", got.Name, "run_with_parent")
			}
			if got.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (evidence: %q)", got.Status, tt.wantStatus, got.Evidence)
			}
			if !strings.Contains(got.Evidence, tt.wantEvidence) {
				t.Errorf("evidence = %q, want it to contain %q", got.Evidence, tt.wantEvidence)
			}
			if tt.rejectEvidence != "" && strings.Contains(got.Evidence, tt.rejectEvidence) {
				t.Errorf("evidence = %q, must not contain %q", got.Evidence, tt.rejectEvidence)
			}
		})
	}
}

func TestMissingPrerequisiteEvidenceIsActionable(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name         string
		check        func() Check
		wantName     string
		wantStatus   CheckStatus
		wantEvidence []string
	}{
		{
			name: "socket_vmnet client",
			check: func() Check {
				return socketVMNetClientCheck(missing, missing)
			},
			wantName:     "socket_vmnet_client",
			wantStatus:   CheckFail,
			wantEvidence: []string{"brew install socket_vmnet", "/opt/socket_vmnet"},
		},
		{
			name: "discovered firmware",
			check: func() Check {
				code, _ := discoveredFirmwareChecks([]firmwareInstallation{{codePath: missing, variablesPath: []string{missing}}})
				return code
			},
			wantName:     "firmware_code",
			wantStatus:   CheckFail,
			wantEvidence: []string{"brew install qemu"},
		},
		{
			name: "QEMU binary",
			check: func() Check {
				return executableCheck("qemu_binary", missing, errors.New("not found"))
			},
			wantName:     "qemu_binary",
			wantStatus:   CheckFail,
			wantEvidence: []string{"brew install qemu"},
		},
		{
			name: "QEMU image tool",
			check: func() Check {
				return executableCheck("qemu_img", missing, errors.New("not found"))
			},
			wantName:     "qemu_img",
			wantStatus:   CheckFail,
			wantEvidence: []string{"brew install qemu"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.check()
			assertCheck(t, got, tt.wantName, tt.wantStatus, tt.wantEvidence...)
		})
	}
}

func TestUnavailableSocketVMNetSocketEvidenceExplainsStartingService(t *testing.T) {
	t.Parallel()

	tempDir, err := os.MkdirTemp(os.TempDir(), "qm-doctor-")
	if err != nil {
		t.Fatalf("create short temporary directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	missing := filepath.Join(tempDir, "missing.sock")
	stale := filepath.Join(tempDir, "stale.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatalf("create stale Unix socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close stale Unix socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(stale) })

	tests := []struct {
		name string
		path string
	}{
		{name: "missing", path: missing},
		{name: "not connectable", path: stale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := socketVMNetSocketCheck(context.Background(), tt.path, tt.path)
			assertCheck(t, got, "socket_vmnet_socket", CheckFail, "start", "service")
		})
	}
}

func TestSocketVMNetClientCheckInspectsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	homebrewDir := filepath.Join(root, "homebrew", "Cellar", "socket_vmnet")
	if err := os.MkdirAll(homebrewDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(homebrewDir, "socket_vmnet_client")
	if err := os.WriteFile(target, []byte("fixture"), 0o555); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "opt", "socket_vmnet_client")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got := socketVMNetClientCheck(link, link)
	assertCheck(t, got, "socket_vmnet_client", CheckWarn, "resolves to", "root-owned client copy")
}

func assertCheck(t *testing.T, got Check, wantName string, wantStatus CheckStatus, evidence ...string) {
	t.Helper()
	if got.Name != wantName {
		t.Errorf("name = %q, want %q", got.Name, wantName)
	}
	if got.Status != wantStatus {
		t.Errorf("status = %q, want %q (evidence: %q)", got.Status, wantStatus, got.Evidence)
	}
	for _, want := range evidence {
		if !strings.Contains(got.Evidence, want) {
			t.Errorf("evidence = %q, want it to contain %q", got.Evidence, want)
		}
	}
}

func writeCapabilityFixture(t *testing.T, output string, exitStatus int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "qemu-system-aarch64")
	fixture := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(output, "'", "'\\''") + "'\nexit " + strconv.Itoa(exitStatus) + "\n"
	if err := os.WriteFile(path, []byte(fixture), 0o700); err != nil {
		t.Fatalf("write executable fixture: %v", err)
	}
	return path
}
