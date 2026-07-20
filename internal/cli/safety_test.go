package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/pterm/pterm"
)

func runSafetyCLI(a *App, stdin io.Reader, stdout, stderr io.Writer, args ...string) int {
	return a.Run(context.Background(), args, stdin, stdout, stderr)
}

func runSafetyCLIWithInput(a *App, input string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runSafetyCLI(a, strings.NewReader(input), &stdout, &stderr, args...)
	return code, stdout.String(), stderr.String()
}

func visibleOutput(text string) string {
	clean := pterm.RemoveColorFromString(text)
	clean = strings.ReplaceAll(clean, "\r", "")
	clean = strings.ReplaceAll(clean, "\x1b[K", "")
	return strings.TrimSpace(clean)
}

func TestDeleteInteractiveConfirmation(t *testing.T) {
	t.Run("yes deletes stopped VM", func(t *testing.T) {
		a := testApp(t)
		a.IsTerminal = func(io.Reader) bool { return true }
		a.IsTerminalOutput = func(io.Writer) bool { return true }
		saveTestConfig(t, a, testConfig("home-assistant"))
		configureAbsentLaunchd(t, a)

		var stdout, stderr bytes.Buffer
		confirmCalls := 0
		a.Confirm = func(prompt string) (bool, error) {
			confirmCalls++
			if prompt != "Permanently delete home-assistant VM?" {
				t.Fatalf("prompt=%q", prompt)
			}
			if visible := visibleOutput(stdout.String()); visible == "" {
				t.Fatal("warning was not written before confirmation")
			}
			return true, nil
		}

		code := runSafetyCLI(a, strings.NewReader("ignored\n"), &stdout, &stderr, "delete", "home-assistant")
		if code != 0 || confirmCalls != 1 || visibleOutput(stderr.String()) == "" {
			t.Fatalf("code=%d confirmCalls=%d stderr=%q stdout=%q", code, confirmCalls, stderr.String(), stdout.String())
		}
		if _, err := a.Store.Load("home-assistant"); err == nil {
			t.Fatal("confirmed delete left the VM in the store")
		}
	})

	t.Run("no keeps stopped VM and reports cancellation", func(t *testing.T) {
		a := testApp(t)
		a.IsTerminal = func(io.Reader) bool { return true }
		a.IsTerminalOutput = func(io.Writer) bool { return true }
		saveTestConfig(t, a, testConfig("keep-me"))
		configureAbsentLaunchd(t, a)

		var stdout, stderr bytes.Buffer
		var warningBeforeConfirm string
		a.Confirm = func(prompt string) (bool, error) {
			if prompt != "Permanently delete keep-me VM?" {
				t.Fatalf("prompt=%q", prompt)
			}
			warningBeforeConfirm = visibleOutput(stdout.String())
			if warningBeforeConfirm == "" {
				t.Fatal("warning was not written before confirmation")
			}
			return false, nil
		}

		code := runSafetyCLI(a, strings.NewReader("ignored\n"), &stdout, &stderr, "delete", "keep-me")
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
		}
		if output := visibleOutput(stdout.String()); output == "" || output == warningBeforeConfirm {
			t.Fatalf("stdout=%q warningBeforeConfirm=%q", stdout.String(), warningBeforeConfirm)
		}
		if _, err := a.Store.Load("keep-me"); err != nil {
			t.Fatalf("cancelled delete removed VM: %v", err)
		}
	})

	t.Run("confirm errors are wrapped and keep VM", func(t *testing.T) {
		a := testApp(t)
		a.IsTerminal = func(io.Reader) bool { return true }
		a.IsTerminalOutput = func(io.Writer) bool { return true }
		saveTestConfig(t, a, testConfig("keep-me"))
		configureAbsentLaunchd(t, a)

		sentinel := errors.New("sentinel confirm failure")
		a.Confirm = func(string) (bool, error) { return false, sentinel }

		code, stdout, stderr := runSafetyCLIWithInput(a, "", "delete", "keep-me")
		if code != 1 || stdout == "" || !strings.Contains(stderr, "config: read deletion confirmation: "+sentinel.Error()) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if _, err := a.Store.Load("keep-me"); err != nil {
			t.Fatalf("prompt failure removed VM: %v", err)
		}
	})
}

func TestDeleteNoninteractiveRequiresForce(t *testing.T) {
	t.Run("nonterminal stdin requires force", func(t *testing.T) {
		a := testApp(t)
		a.IsTerminal = func(io.Reader) bool { return false }
		a.IsTerminalOutput = func(io.Writer) bool { return true }
		a.Confirm = func(string) (bool, error) {
			t.Fatal("Confirm should not be called")
			return false, nil
		}
		saveTestConfig(t, a, testConfig("keep-me"))

		code, stdout, stderr := runSafetyCLIWithInput(a, "y\n", "delete", "keep-me")
		if code != 1 || stdout != "" || strings.Contains(stdout, "\x1b[") || !strings.Contains(stderr, "--force") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if _, err := a.Store.Load("keep-me"); err != nil {
			t.Fatalf("noninteractive refusal removed VM: %v", err)
		}
	})

	t.Run("redirected stdout requires force", func(t *testing.T) {
		a := testApp(t)
		a.IsTerminal = func(io.Reader) bool { return true }
		a.IsTerminalOutput = func(io.Writer) bool { return false }
		a.Confirm = func(string) (bool, error) {
			t.Fatal("Confirm should not be called")
			return false, nil
		}
		saveTestConfig(t, a, testConfig("keep-me"))

		code, stdout, stderr := runSafetyCLIWithInput(a, "y\n", "delete", "keep-me")
		if code != 1 || stdout != "" || strings.Contains(stdout, "\x1b[") || !strings.Contains(stderr, "--force") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if _, err := a.Store.Load("keep-me"); err != nil {
			t.Fatalf("redirected-stdout refusal removed VM: %v", err)
		}
	})
}

func TestDeleteForceSkipsConfirmation(t *testing.T) {
	a := testApp(t)
	a.IsTerminal = func(io.Reader) bool { return true }
	a.IsTerminalOutput = func(io.Writer) bool { return true }
	a.Confirm = func(string) (bool, error) {
		t.Fatal("Confirm should not be called for --force")
		return false, nil
	}
	saveTestConfig(t, a, testConfig("vm"))
	configureAbsentLaunchd(t, a)

	code, stdout, stderr := runSafetyCLIWithInput(a, "", "delete", "vm", "--force")
	if code != 0 || stdout != "" || visibleOutput(stderr) == "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := a.Store.Load("vm"); err == nil {
		t.Fatal("forced delete left the VM in the store")
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
