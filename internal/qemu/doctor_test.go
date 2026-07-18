package qemu

import (
	"context"
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

func writeCapabilityFixture(t *testing.T, output string, exitStatus int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "qemu-system-aarch64")
	fixture := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(output, "'", "'\\''") + "'\nexit " + strconv.Itoa(exitStatus) + "\n"
	if err := os.WriteFile(path, []byte(fixture), 0o700); err != nil {
		t.Fatalf("write executable fixture: %v", err)
	}
	return path
}
