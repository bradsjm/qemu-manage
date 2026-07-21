package cli

import (
	"bytes"
	"errors"
	"strconv"
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

			requireHelpSuccess(t, a, args...)
		})
	}
}

func TestCommandAndNestedHelp(t *testing.T) {
	a := testApp(t)
	cases := []struct {
		name  string
		args  []string
		want  []string
		avoid []string
	}{
		{
			name: "create before name",
			args: []string{"create", "--help"},
			want: []string{
				"create NAME",
				"Source and storage:",
				"Resources and lifecycle:",
				"Networking:",
				"Display:",
				"Guest integration:",
				"Firmware and executables:",
				"Repeatable devices and drives:",
				"--keyboard-layout LAYOUT",
				"--rtc-base VALUE",
				"--socket-vmnet-interface NAME",
				"--mac MAC",
				"locally administered unicast MAC; generated if omitted",
				"--share PATH",
				"--cloud-init-user-data PATH",
				"managed read-only",
				"NoCloud ISO labelled CIDATA",
				"VM UUID as the",
				"instance-id",
				"single folder",
				"//10.0.2.4/qemu",
				"sudo mount -t cifs //10.0.2.4/qemu /mnt/share -o username=guest",
				"brew install samba",
			},
		},
		{
			name: "create after name",
			args: []string{"create", "example", "--help"},
			want: []string{
				"create NAME",
				"Relative drive files become absolute external references and must stay readable",
				"Bus/address can change after a device",
				"QEMU_MANAGE_SOCKET_VMNET_CLIENT",
				"Examples:",
			},
		},
		{
			name: "set",
			args: []string{"set", "--help"},
			want: []string{
				"set NAME",
				"Resources and lifecycle:",
				"Networking:",
				"Display and guest integration:",
				"--keyboard-layout LAYOUT",
				"--rtc-base VALUE",
				"--metrics-port VALUE",
				"QEMU_MANAGE_SOCKET_VMNET_CLIENT",
			},
			avoid: []string{"--socket-vmnet-client", "--socket-vmnet-socket"},
		},
		{name: "start", args: []string{"start", "--help"}, want: []string{"start NAME", "--foreground", "--boot-menu", "not persisted", "showcmd"}},
		{name: "restart", args: []string{"restart", "--help"}, want: []string{"restart NAME", "--timeout DURATION", "--force", "--boot-menu", "--foreground", "already stopped", "abort", "Examples:"}},
		{name: "showcmd", args: []string{"showcmd", "--help"}, want: []string{"showcmd NAME", "--boot-menu", "durable VM configuration"}},
		{name: "log", args: []string{"log", "--help"}, want: []string{"log NAME", "active", "rotated backups", "stdout"}},
		{name: "monitor", args: []string{"monitor", "--help"}, want: []string{"monitor NAME", "\"info status\"", "Stdout is only the", "Ctrl-]", "restarted once", "Examples:"}},
		{name: "guest-agent", args: []string{"guest-agent", "--help"}, want: []string{"guest-agent NAME REQUEST", `{"execute":"guest-info"}`, "set NAME --guest-agent on", "compact JSON return value", "Examples:"}},
		{name: "config", args: []string{"config", "--help"}, want: []string{"config", "show", "validate", "apply", "Examples:"}},
		{name: "config show", args: []string{"config", "show", "--help"}, want: []string{"config show NAME", "Examples:"}},
		{name: "autostart", args: []string{"autostart", "--help"}, want: []string{"autostart", "enable", "disable", "status", "Examples:"}},
		{name: "autostart enable", args: []string{"autostart", "enable", "--help"}, want: []string{"autostart enable NAME", "--scope", "--start", "boot", "login", "Examples:"}},
		{name: "info", args: []string{"info", "--help"}, want: []string{"info NAME", "--json", "stopped", "not running", "monitoring", "authenticated", "set NAME --metrics-port PORT", "Examples:"}},
		{name: "vnc", args: []string{"vnc", "--help"}, want: []string{"vnc NAME", "Screen Sharing", "clipboard", "Examples:"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			help := requireHelpSuccess(t, a, tc.args...)
			for _, text := range tc.want {
				if !strings.Contains(help, text) {
					t.Errorf("help does not contain %q: %q", text, help)
				}
			}
			for _, text := range tc.avoid {
				if strings.Contains(help, text) {
					t.Errorf("help unexpectedly contains %q: %q", text, help)
				}
			}
		})
	}
}

func TestRootHelpEnvironmentTableQuotesAndOrdersValues(t *testing.T) {
	lookup := func(name string) (string, bool) {
		switch name {
		case "QEMU_MANAGE_DATA_ROOT":
			return "/tmp/qm data", true
		case "QEMU_MANAGE_SOCKET_VMNET_CLIENT":
			return "/opt/socket_vmnet/bin/socket_vmnet_client\nnext", true
		case "QEMU_MANAGE_SOCKET_VMNET_SOCKET":
			return "", true
		default:
			return "", false
		}
	}

	var output bytes.Buffer
	if err := writeHelp(&output, "", lookup); err != nil {
		t.Fatalf("writeHelp failed: %v", err)
	}
	help := output.String()

	wantOrder := []string{
		"QEMU_MANAGE_DATA_ROOT",
		"QEMU_MANAGE_RUNTIME_ROOT",
		"QEMU_MANAGE_LOG_ROOT",
		"QEMU_MANAGE_SOCKET_VMNET_CLIENT",
		"QEMU_MANAGE_SOCKET_VMNET_SOCKET",
	}
	last := -1
	for _, name := range wantOrder {
		index := strings.Index(help, name)
		if index < 0 {
			t.Fatalf("root help missing %q: %q", name, help)
		}
		if index <= last {
			t.Fatalf("root help order is unstable around %q: %q", name, help)
		}
		last = index
	}

	for _, want := range []string{
		`Current: ` + strconv.Quote("/tmp/qm data"),
		`Current: ` + strconv.Quote("/opt/socket_vmnet/bin/socket_vmnet_client\nnext"),
		"QEMU_MANAGE_RUNTIME_ROOT",
		"QEMU_MANAGE_LOG_ROOT",
		"QEMU_MANAGE_SOCKET_VMNET_SOCKET",
		"Current: unset",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("root help missing %q: %q", want, help)
		}
	}
}

func TestRootHelpNilLookupTreatsEnvironmentAsUnset(t *testing.T) {
	var output bytes.Buffer
	if err := writeHelp(&output, "", nil); err != nil {
		t.Fatalf("writeHelp failed: %v", err)
	}
	if count := strings.Count(output.String(), "Current: unset"); count != 5 {
		t.Fatalf("unset count = %d, want 5; help=%q", count, output.String())
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
		{name: "create metrics port", args: []string{"create", "example", "--metrics-port", "1023"}, want: "1024 and 65535"},
		{name: "set metrics port", args: []string{"set", "example", "--metrics-port", "65536"}, want: "1024 and 65535, or off"},
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
