package cli

import (
	"errors"
	"strings"
	"testing"
)

func requireHelpSuccess(t *testing.T, a *App, args ...string) string {
	t.Helper()
	code, stdout, stderr := runCLI(a, args...)
	if code != 0 {
		t.Fatalf("args=%q: exit code = %d, want 0; stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("args=%q: stderr = %q, want empty", args, stderr)
	}
	if !strings.Contains(stdout, "Usage:") {
		t.Fatalf("args=%q: stdout does not contain Usage: %q", args, stdout)
	}
	return stdout
}

func requireUsageFailure(t *testing.T, a *App, args []string, want ...string) string {
	t.Helper()
	code, stdout, stderr := runCLI(a, args...)
	if code != 2 {
		t.Fatalf("args=%q: exit code = %d, want 2; stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("args=%q: stdout = %q, want empty", args, stdout)
	}
	for _, text := range want {
		if !strings.Contains(stderr, text) {
			t.Errorf("args=%q: stderr does not contain %q: %q", args, text, stderr)
		}
	}
	return stderr
}

func TestRootHelpBypassesRootAndInitialization(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"help", "--help"}, {"help", "-h"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			a := testApp(t)
			a.Geteuid = func() int { return 0 }
			a.initializationError = errors.New("initialization must be bypassed for help")

			help := requireHelpSuccess(t, a, args...)
			for _, section := range []string{"Options:", "Examples:"} {
				if !strings.Contains(help, section) {
					t.Errorf("root help does not contain %q: %q", section, help)
				}
			}
			if strings.Contains(help, "supervise") {
				t.Errorf("root help exposes hidden supervise command: %q", help)
			}
		})
	}
}

func TestCommandAndNestedHelp(t *testing.T) {
	a := testApp(t)
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{name: "create before name", args: []string{"create", "--help"}, want: []string{"create NAME", "Options:", "Examples:"}},
		{name: "create after name", args: []string{"create", "example", "--help"}, want: []string{"create NAME", "Options:", "Examples:"}},
		{name: "config", args: []string{"config", "--help"}, want: []string{"config", "show", "validate", "apply", "Examples:"}},
		{name: "config show", args: []string{"config", "show", "--help"}, want: []string{"config show NAME", "Examples:"}},
		{name: "autostart", args: []string{"autostart", "--help"}, want: []string{"autostart", "enable", "disable", "status", "Examples:"}},
		{name: "autostart enable", args: []string{"autostart", "enable", "--help"}, want: []string{"autostart enable NAME", "--scope", "boot", "login", "Examples:"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			help := requireHelpSuccess(t, a, tc.args...)
			for _, text := range tc.want {
				if !strings.Contains(help, text) {
					t.Errorf("help does not contain %q: %q", text, help)
				}
			}
		})
	}
}

func TestExplicitSuperviseHelpDoesNotExposeItInRootHelp(t *testing.T) {
	a := testApp(t)
	root := requireHelpSuccess(t, a, "help")
	if strings.Contains(root, "supervise") {
		t.Fatalf("root help exposes hidden supervise command: %q", root)
	}

	supervise := requireHelpSuccess(t, a, "help", "supervise")
	if !strings.Contains(supervise, "supervise NAME") {
		t.Fatalf("explicit supervise help lacks contextual usage: %q", supervise)
	}
}

func TestCreateUsageErrorsIncludeContextualHelp(t *testing.T) {
	a := testApp(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing name", args: []string{"create"}, want: "missing NAME"},
		{name: "missing firmware", args: []string{"create", "example"}, want: "--firmware-code and --firmware-vars are required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireUsageFailure(t, a, tc.args, tc.want, "Usage:", "create NAME", "Examples:")
		})
	}
}

func TestInvalidValuesListExactValidValues(t *testing.T) {
	a := testApp(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "create restart policy", args: []string{"create", "example", "--restart-policy", "always", "--firmware-code", "code.fd", "--firmware-vars", "vars.fd"}, want: "never, on-failure"},
		{name: "set network", args: []string{"set", "example", "--network", "bridge"}, want: "user, socket_vmnet"},
		{name: "set guest agent", args: []string{"set", "example", "--guest-agent", "maybe"}, want: "on, off"},
		{name: "set restart policy", args: []string{"set", "example", "--restart-policy", "always"}, want: "never, on-failure"},
		{name: "autostart scope", args: []string{"autostart", "enable", "example", "--scope", "session"}, want: "boot, login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireUsageFailure(t, a, tc.args, tc.want, "Usage:")
		})
	}
}

func TestUnknownCommandPrintsRootHelp(t *testing.T) {
	a := testApp(t)
	stderr := requireUsageFailure(t, a, []string{"frobnicate"}, "unknown command", "Usage:", "Examples:")
	if strings.Contains(stderr, "supervise") {
		t.Fatalf("unknown-command root help exposes hidden supervise command: %q", stderr)
	}
}

func TestUnknownNestedHelpReturnsUsageFailure(t *testing.T) {
	a := testApp(t)
	for _, args := range [][]string{
		{"config", "unknown", "--help"},
		{"autostart", "unknown", "--help"},
	} {
		requireUsageFailure(t, a, args, "unknown subcommand", "Usage:", "Subcommands:", "Examples:")
	}
}
