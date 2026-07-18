package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func runSafetyCLIWithInput(a *App, input string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := a.Run(context.Background(), args, strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestDeleteInteractiveConfirmation(t *testing.T) {
	t.Run("yes deletes stopped VM", func(t *testing.T) {
		a := testApp(t)
		a.IsTerminal = func(io.Reader) bool { return true }
		saveTestConfig(t, a, testConfig("home-assistant"))
		configureAbsentLaunchd(t, a)

		code, stdout, stderr := runSafetyCLIWithInput(a, "y\n", "delete", "home-assistant")
		if code != 0 || stderr != "" {
			t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
		}
		for _, want := range []string{"WARNING", "home-assistant", "[y/N]"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("confirmation prompt %q does not contain %q", stdout, want)
			}
		}
		prompt := strings.ToLower(stdout)
		for _, want := range []string{"permanent", "disk", "config"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("confirmation prompt %q does not contain %q", stdout, want)
			}
		}
		if _, err := a.Store.Load("home-assistant"); err == nil {
			t.Fatal("confirmed delete left the VM in the store")
		}
	})

	for _, tc := range []struct {
		name  string
		input string
	}{
		{name: "blank defaults to no", input: "\n"},
		{name: "explicit no", input: "n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			a.IsTerminal = func(io.Reader) bool { return true }
			saveTestConfig(t, a, testConfig("keep-me"))
			configureAbsentLaunchd(t, a)

			code, _, stderr := runSafetyCLIWithInput(a, tc.input, "delete", "keep-me")
			if code != 0 || stderr != "" {
				t.Fatalf("cancel should succeed quietly: code=%d stderr=%q", code, stderr)
			}
			if _, err := a.Store.Load("keep-me"); err != nil {
				t.Fatalf("cancelled delete removed VM: %v", err)
			}
		})
	}
}

func TestDeleteNoninteractiveRequiresForce(t *testing.T) {
	a := testApp(t)
	a.IsTerminal = func(io.Reader) bool { return false }
	saveTestConfig(t, a, testConfig("keep-me"))

	code, _, stderr := runSafetyCLIWithInput(a, "y\n", "delete", "keep-me")
	if code != 1 || !strings.Contains(stderr, "--force") {
		t.Fatalf("code=%d stderr=%q; want refusal with --force guidance", code, stderr)
	}
	if _, err := a.Store.Load("keep-me"); err != nil {
		t.Fatalf("noninteractive refusal removed VM: %v", err)
	}
}

func TestStopHelpWarnsForceCanCorruptGuestData(t *testing.T) {
	a := testApp(t)
	code, stdout, stderr := runSafetyCLIWithInput(a, "", "stop", "--help")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	help := strings.ToLower(stdout)
	for _, want := range []string{"--force", "guest", "filesystem", "data", "corrupt"} {
		if !strings.Contains(help, want) {
			t.Errorf("stop help %q does not contain %q", stdout, want)
		}
	}
}
