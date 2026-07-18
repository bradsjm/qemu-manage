package cli

import (
	"fmt"
	"io"
	"strings"
)

type helpTopic struct {
	text string
}

var helpTopics = map[string]helpTopic{
	"": {text: `qemu-manage manages headless QEMU virtual machines on Apple Silicon.

Usage:
  qemu-manage COMMAND [ARGUMENTS]
  qemu-manage COMMAND --help
  qemu-manage help [COMMAND [SUBCOMMAND]]

Options:
  -h, --help  Show progressive help for the current command

Getting started:
  1. Install QEMU: brew install qemu
  2. Check the host: qemu-manage doctor
  3. Create a VM: qemu-manage create NAME --help
  4. Check and start it: qemu-manage doctor NAME && qemu-manage start NAME

Commands:
  create       Create a managed VM from an image, ISO, or blank disk
  set          Change VM resources, restart behavior, or networking
  config       Show, validate, or apply the complete JSON configuration
  showcmd      Print the exact QEMU command without running it
  start        Start a VM in the background
  stop         Gracefully stop a VM, or explicitly force it
  console      Connect to a running VM's serial console
  status       Show one VM or all VM runtime states
  list         List all managed VMs
  doctor       Check QEMU, firmware, VM files, and networking prerequisites
  autostart    Manage login or system-boot startup with launchd
  delete       Permanently delete a stopped VM and its managed files

Network choices:
  user          Built into QEMU; simplest setup, with optional port forwards
  socket_vmnet  Host/shared/bridged networking; requires socket_vmnet

Examples:
  qemu-manage --help
  qemu-manage create --help
  qemu-manage autostart enable --help

Run 'qemu-manage COMMAND --help' for options, examples, and next steps.
`},
	"create": {text: `Create a managed AArch64 VM. Source images and firmware are copied; they are not modified.

Usage:
  qemu-manage create NAME [OPTIONS]

Defaults:
  With neither --image nor --iso, create makes a blank 32GiB qcow2 disk.
  Networking starts in user mode and the guest agent starts disabled.
  QEMU and qemu-img are resolved from PATH. Firmware code and variables are
  auto-detected as a matching pair from QEMU's Homebrew or system share files.
  To override firmware discovery, provide --firmware-code and --firmware-vars together.
  HTTP(S) image URLs are downloaded directly. URL paths ending in .xz or .gz
  are decompressed while downloading; partial downloads are removed on failure.

Options:
  --image SOURCE           Local path or HTTP(S) URL to qcow2/raw image
  --iso PATH               Copy an installer ISO (default: none)
  --cpus N                 Virtual CPUs (default: 2)
  --memory SIZE            Whole MiB or GiB (default: 2GiB)
  --disk-size SIZE         Primary disk size (default: 32GiB)
  --qemu PATH              QEMU executable (default: qemu-system-aarch64 in PATH)
  --qemu-img PATH          qemu-img executable (default: qemu-img in PATH)
  --firmware-code PATH     Override the auto-detected AArch64 UEFI code image
  --firmware-vars PATH     Override the auto-detected UEFI variables template
  --restart-policy VALUE   Valid values: never, on-failure (default: never)
  --shutdown-timeout D     Positive whole-second duration (default: 180s)

Examples:
  # Inspect automatically discovered QEMU and firmware paths first.
  qemu-manage doctor

  # Download, decompress, and import Home Assistant OS directly from GitHub.
  qemu-manage create home-assistant \
    --image "https://github.com/home-assistant/operating-system/releases/download/18.0/haos_generic-aarch64-18.0.qcow2.xz" \
    --cpus 2 --memory 4GiB --disk-size 32GiB \
    --restart-policy on-failure

  # Create a blank disk and boot an installer ISO using detected firmware.
  qemu-manage create linux --iso "$HOME/Downloads/linux-arm64.iso"

NAME must come before options. Next: qemu-manage doctor NAME, then qemu-manage start NAME.
`},
	"set": {text: `Change a managed VM configuration. Omitted options keep their current values.

Usage:
  qemu-manage set NAME OPTION [OPTION ...]

Resource and lifecycle options:
  --cpus N                       Positive virtual CPU count
  --memory SIZE                  Whole MiB or GiB, such as 4096MiB or 4GiB
  --restart-policy VALUE         Valid values: never, on-failure
  --shutdown-timeout DURATION    Positive whole-second duration, such as 180s
  --guest-agent VALUE            Valid values: on, off

Network options:
  --network VALUE                Valid values: user, socket_vmnet
  --forward SPEC                 Repeatable proto:IPv4:host-port:guest-port
  --clear-forwards               Remove existing user-network forwards
  --socket-vmnet-client PATH     Absolute socket_vmnet_client path
  --socket-vmnet-socket PATH     Absolute socket_vmnet daemon socket path
  --socket-vmnet-interface NAME  Interface description, such as shared or vlan0

Examples:
  # Built-in user networking with Home Assistant available on localhost:8123.
  qemu-manage set home-assistant --network user \
    --forward tcp:127.0.0.1:8123:8123 --guest-agent on

  # Install and start the basic Homebrew socket_vmnet service.
  brew install socket_vmnet
  sudo "$(brew --prefix)/bin/brew" services start socket_vmnet

  # Use that shared socket_vmnet service; installed paths are auto-detected.
  qemu-manage set home-assistant --network socket_vmnet

  # If doctor warns that the Homebrew client is user-writable, make a root-owned copy.
  sudo install -d -o root -g wheel -m 0755 /opt/socket_vmnet/bin
  sudo install -o root -g wheel -m 0755 \
    "$(brew --prefix socket_vmnet)/bin/socket_vmnet_client" \
    /opt/socket_vmnet/bin/socket_vmnet_client
  qemu-manage set home-assistant \
    --network socket_vmnet \
    --socket-vmnet-client /opt/socket_vmnet/bin/socket_vmnet_client \
    --socket-vmnet-socket /opt/homebrew/var/run/socket_vmnet \
    --socket-vmnet-interface shared

Repeat the second sudo install command after upgrading socket_vmnet with Homebrew.
NAME must come before options. Next: qemu-manage doctor NAME.
`},
	"config": {text: `Inspect or replace the complete strict JSON VM configuration.

Usage:
  qemu-manage config SUBCOMMAND [ARGUMENTS]

Subcommands:
  show NAME          Print canonical JSON for a managed VM
  validate FILE      Strictly validate one JSON configuration file
  apply NAME FILE    Validate and atomically apply editable settings

Examples:
  qemu-manage config show home-assistant > home-assistant.json
  qemu-manage config validate home-assistant.json
  qemu-manage config apply home-assistant home-assistant.json

Run 'qemu-manage config SUBCOMMAND --help' for details.
`},
	"config show": {text: `Print a VM's complete canonical JSON configuration.

Usage:
  qemu-manage config show NAME

Examples:
  qemu-manage config show home-assistant > home-assistant.json
`},
	"config validate": {text: `Strictly validate a configuration without changing a VM.

Usage:
  qemu-manage config validate FILE

Unknown fields, trailing JSON, invalid values, and unsupported schema versions fail.

Examples:
  qemu-manage config validate home-assistant.json
`},
	"config apply": {text: `Validate and atomically replace a VM's editable JSON configuration.

Usage:
  qemu-manage config apply NAME FILE

The VM identity, backend, architecture, and autostart scope must match. Managed disk
files are not deleted, but review the replacement first with config validate.

Examples:
  qemu-manage config validate home-assistant.json
  qemu-manage config apply home-assistant home-assistant.json
`},
	"showcmd": {text: `Print the exact safely quoted QEMU command without starting the VM.

Usage:
  qemu-manage showcmd NAME

Examples:
  qemu-manage showcmd home-assistant
`},
	"start": {text: `Check prerequisites and start a VM under its authenticated supervisor.

Usage:
  qemu-manage start NAME [--foreground]

Options:
  --foreground  Keep the supervisor attached to this terminal (diagnostics)

Examples:
  qemu-manage doctor home-assistant
  qemu-manage start home-assistant
  qemu-manage status home-assistant
`},
	"stop": {text: `Request a graceful guest shutdown through QGA or QMP.

Usage:
  qemu-manage stop NAME [--timeout DURATION] [--force]

Options:
  --timeout DURATION  Override the configured whole-second shutdown timeout
  --force             Kill QEMU without guest cooperation; guest filesystem or data
                      corruption is possible

Examples:
  qemu-manage stop home-assistant
  qemu-manage stop home-assistant --timeout 5m
  qemu-manage stop home-assistant --force
`},
	"console": {text: `Connect the terminal to a running or paused VM's serial console.

Usage:
  qemu-manage console NAME

Press Ctrl-] to disconnect without stopping the VM.

Examples:
  qemu-manage console home-assistant
`},
	"status": {text: `Show runtime state and whether a configuration change requires restart.

Usage:
  qemu-manage status [NAME] [--json]

Options:
  --json  Emit stable machine-readable JSON

Examples:
  qemu-manage status
  qemu-manage status home-assistant
  qemu-manage status home-assistant --json
`},
	"list": {text: `List all managed VMs and their current runtime states.

Usage:
  qemu-manage list [--json]

Options:
  --json  Emit stable machine-readable JSON

Examples:
  qemu-manage list
`},
	"doctor": {text: `Check host or named-VM prerequisites without changing anything.

Usage:
  qemu-manage doctor [NAME] [--json]

Options:
  --json  Emit every check as machine-readable JSON

Examples:
  qemu-manage doctor
  qemu-manage doctor home-assistant

Common prerequisites:
  brew install qemu
  brew install socket_vmnet        # only for --network socket_vmnet
  sudo "$(brew --prefix)/bin/brew" services start socket_vmnet

If doctor reports a user-writable socket_vmnet client:
  sudo install -d -o root -g wheel -m 0755 /opt/socket_vmnet/bin
  sudo install -o root -g wheel -m 0755 \
    "$(brew --prefix socket_vmnet)/bin/socket_vmnet_client" \
    /opt/socket_vmnet/bin/socket_vmnet_client

Then select it with:
  qemu-manage set NAME --network socket_vmnet \
    --socket-vmnet-client /opt/socket_vmnet/bin/socket_vmnet_client \
    --socket-vmnet-socket /opt/homebrew/var/run/socket_vmnet \
    --socket-vmnet-interface shared

A named check also verifies copied firmware, disks, configured socket_vmnet paths,
and whether the helper socket is connectable.
`},
	"autostart": {text: `Manage automatic VM startup through launchd.

Usage:
  qemu-manage autostart SUBCOMMAND NAME [OPTIONS]

Subcommands:
  enable NAME [--scope VALUE]  Install and load an autostart job
  disable NAME                 Stop the VM and remove its autostart job
  status NAME                  Compare configured and installed launchd state

Valid scopes:
  login  Start when this user logs in
  boot   Start at system boot as the configured non-root user

Enabling uses RunAtLoad and starts the VM immediately. The VM must be stopped first.
System-boot changes require sudo, but qemu-manage itself must run as a normal user.

Examples:
  qemu-manage autostart enable home-assistant --scope login
  qemu-manage autostart status home-assistant

Run 'qemu-manage autostart SUBCOMMAND --help' for details.
`},
	"autostart enable": {text: `Check prerequisites, install a launchd job, and start the VM immediately.

Usage:
  qemu-manage autostart enable NAME [--scope VALUE]

Options:
  --scope VALUE  Valid values: boot, login (default: boot)

Examples:
  qemu-manage autostart enable home-assistant --scope login
  qemu-manage autostart enable home-assistant --scope boot

The VM must be stopped. Boot scope requires sudo for system launchd changes, while
QEMU itself continues to run as the configured non-root user.
`},
	"autostart disable": {text: `Gracefully stop a VM and transactionally remove its launchd job.

Usage:
  qemu-manage autostart disable NAME

Examples:
  qemu-manage autostart disable home-assistant
`},
	"autostart status": {text: `Show configured, installed, matching, and loaded launchd state.

Usage:
  qemu-manage autostart status NAME

Examples:
  qemu-manage autostart status home-assistant
`},
	"delete": {text: `Permanently delete a stopped VM and all files managed for it.

Usage:
  qemu-manage delete NAME [--force]

Options:
  --force  Skip the interactive confirmation (required for noninteractive use)

WARNING: This removes the VM configuration, copied firmware, installer, managed disk
images, logs, and runtime files. Source images originally passed to --image or --iso
are not removed. A running VM or VM with autostart is refused.

Examples:
  qemu-manage stop test-vm
  qemu-manage autostart disable test-vm
  qemu-manage delete test-vm
`},
	"supervise": {text: `Internal supervisor entry point. Users should run 'qemu-manage start' instead.

Usage:
  qemu-manage supervise NAME --ready-fd FD --expected-id ID
`},
}

func isHelpFlag(value string) bool {
	return value == "-h" || value == "-help" || value == "--help"
}

func requestedHelp(args []string) (string, bool, error) {
	if len(args) == 0 {
		return "", false, nil
	}
	if args[0] == "help" {
		if len(args) == 2 && isHelpFlag(args[1]) {
			return "", true, nil
		}
		if len(args) > 3 {
			return "", true, usageErrorf("help: expected at most COMMAND and SUBCOMMAND")
		}
		topic := strings.Join(args[1:], " ")
		if _, ok := helpTopics[topic]; !ok {
			return "", true, usageErrorf("help: unknown topic %q", topic)
		}
		return topic, true, nil
	}

	requested := false
	for _, arg := range args {
		if isHelpFlag(arg) {
			requested = true
			break
		}
	}
	if !requested {
		return "", false, nil
	}
	if (args[0] == "config" || args[0] == "autostart") && len(args) > 1 && !isHelpFlag(args[1]) {
		nested := args[0] + " " + args[1]
		if _, ok := helpTopics[nested]; !ok {
			return "", true, usageErrorf("%s: unknown subcommand %q", args[0], args[1])
		}
	}
	topic := inferHelpTopic(args)
	if _, ok := helpTopics[topic]; !ok {
		return "", true, usageErrorf("unknown command %q", args[0])
	}
	return topic, true, nil
}

func inferHelpTopic(args []string) string {
	if len(args) == 0 {
		return ""
	}
	command := args[0]
	if command == "config" || command == "autostart" {
		if len(args) > 1 {
			nested := command + " " + args[1]
			if _, ok := helpTopics[nested]; ok {
				return nested
			}
		}
	}
	if _, ok := helpTopics[command]; ok {
		return command
	}
	return ""
}

func writeHelp(output io.Writer, topic string) error {
	help, ok := helpTopics[topic]
	if !ok {
		return fmt.Errorf("unknown help topic %q", topic)
	}
	_, err := io.WriteString(output, help.text)
	return err
}
