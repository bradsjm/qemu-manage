package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

type helpTopic struct {
	text string
}

type helpEnvVar struct {
	name        string
	description string
}

var rootHelpEnvVars = []helpEnvVar{
	{
		name:        "QEMU_MANAGE_DATA_ROOT",
		description: "Absolute owner-controlled VM data directory (default: ~/Library/Application Support/qemu-manage/vms).",
	},
	{
		name:        "QEMU_MANAGE_RUNTIME_ROOT",
		description: "Absolute owner-controlled runtime directory; keep it short (default: /tmp/qemu-manage-<uid>).",
	},
	{
		name:        "QEMU_MANAGE_LOG_ROOT",
		description: "Absolute owner-controlled log directory (default: ~/Library/Logs/qemu-manage).",
	},
	{
		name:        "QEMU_MANAGE_SOCKET_VMNET_CLIENT",
		description: "Absolute root-owned socket_vmnet_client path; otherwise discovered from Homebrew or MacPorts.",
	},
	{
		name:        "QEMU_MANAGE_SOCKET_VMNET_SOCKET",
		description: "Absolute socket_vmnet daemon socket path; otherwise discovered from Homebrew or MacPorts.",
	},
}

var helpTopics = map[string]helpTopic{
	"": {text: ``},
	"create": {text: `Create a managed AArch64 VM. Source images and firmware are copied; extra drive files are referenced in place and are not modified.

Usage:
  qemu-manage create NAME [OPTIONS]

Defaults:
  With neither --image nor --iso, create makes a blank 32GiB qcow2 disk.
  New VMs start with user networking, the guest agent off, RTC base utc, and a
  concrete machine pinned from the selected QEMU binary as virt-N.N.
  A locally administered unicast MAC is generated unless --mac is provided.
  QEMU and qemu-img are resolved from PATH. Firmware code and variables are
  auto-detected as a matching pair from QEMU's Homebrew or system share files.
  To override firmware discovery, provide --firmware-code and --firmware-vars together.
  HTTP(S) image URLs are downloaded directly. URL paths ending in .xz or .gz
  are decompressed while downloading; partial downloads are removed on failure.

Source and storage:
  --image SOURCE           Local path or HTTP(S) URL to qcow2/raw image
  --iso PATH               Copy an installer ISO (default: none)
  --cloud-init-user-data PATH
                           Copy one user-data file into a managed NoCloud seed
  --disk-size SIZE         Primary disk size (default: 32GiB)

Resources and lifecycle:
  --cpus N                 Virtual CPUs (default: 2)
  --memory SIZE            Whole MiB or GiB (default: 2GiB)
  --restart-policy VALUE   Valid values: never, on-failure (default: never)
  --shutdown-timeout D     Positive whole-second duration (default: 180s)
  --rtc-base VALUE         Valid values: utc, localtime (default: utc)
  --metrics-port PORT      Loopback monitoring HTTP port, 1024-65535 (default: off)

Networking:
  --network VALUE             Valid values: user, socket_vmnet (default: user)
  --mac MAC                   Optional lowercase six-byte colon-separated hexadecimal
                              locally administered unicast MAC; generated if omitted
  --forward SPEC              Repeatable proto:IPv4:host-port:guest-port (user only)
  --share PATH                Export one host folder over SMB (user network only)
  --socket-vmnet-interface NAME
                               shared, or a host interface such as en0
                               (socket_vmnet only; default: shared)

Creating with a non-shared socket_vmnet interface automatically installs and
starts a root-owned bridged socket_vmnet LaunchDaemon after requesting sudo.
Only socket_vmnet from Homebrew must already be installed. The persisted socket
and client paths work for both later manual starts and launchd autostart.

Display:
  --vnc                     Enable QEMU VNC
  --vnc-password VALUE      Required with --vnc; 1-8 UTF-8 bytes
  --vnc-bind IPV4           VNC bind IPv4 address (default: 127.0.0.1)
  --vnc-port PORT           Minimum VNC TCP port (default: 5900)
  --vnc-port-to PORT        Maximum VNC TCP port (default: 5999)
  --keyboard-layout LAYOUT  Valid only with --vnc (default: en-us)

Guest integration:
  --guest-agent VALUE       Valid values: on, off (default: off)

Firmware and executables:
  --qemu PATH              QEMU executable (default: qemu-system-aarch64 in PATH)
  --qemu-img PATH          qemu-img executable (default: qemu-img in PATH)
  --firmware-code PATH     Override the auto-detected AArch64 UEFI code image
  --firmware-vars PATH     Override the auto-detected UEFI variables template

Repeatable devices and drives:
  --usb vendor=VVVV,product=PPPP
  --usb bus=N,address=N
  --drive file=PATH[,if=virtio][,format=raw|qcow2][,cache=none|writeback|writethrough|directsync|unsafe][,aio=threads|native][,readonly=on|off]

USB selectors attach in the order provided. Bus/address can change after a device
is unplugged and replugged. Without VNC, up to four USB selections fit; with VNC,
up to two fit because QEMU adds a USB keyboard and tablet.

Extra drives are repeatable virtio disks appended after the managed primary disk.
Relative drive files become absolute external references and must stay readable
and in place. Double each literal comma in a value as ",,". Omitted format is
detected; omitted if means virtio; host and QEMU support still govern aio=native.

--cloud-init-user-data copies one user-data file into a managed read-only
NoCloud ISO labelled CIDATA and generates meta-data using the VM UUID as the
instance-id. The guest image must support the NoCloud datasource. The seed
remains attached across boots; cloud-init normally applies per-instance
configuration only on the first boot.

--share PATH exports one host directory through QEMU's built-in user-network SMB
server. It is create-only, user-network-only, and accepts a single folder: QEMU
exposes exactly one fixed share at //10.0.2.4/qemu. Relative paths are normalized
to absolute. The directory is referenced in place, never copied, and must remain
readable on the host. QEMU's user-network SMB server invokes a helper at
/opt/homebrew/sbin/samba-dot-org-smbd; install it with 'brew install samba' or
create is refused and doctor reports samba_smbd. After create and in
'qemu-manage status NAME', the exact Linux guest mount is printed:

  sudo mkdir -p /mnt/share
  sudo mount -t cifs //10.0.2.4/qemu /mnt/share -o username=guest

Examples:
  # Download, decompress, and import Home Assistant OS directly from GitHub.
  qemu-manage create home-assistant \
    --image "https://github.com/home-assistant/operating-system/releases/download/18.0/haos_generic-aarch64-18.0.qcow2.xz" \
    --cpus 2 --memory 4GiB --disk-size 32GiB \
    --network user --forward tcp:127.0.0.1:2222:22 \
    --guest-agent on --rtc-base utc \
    --restart-policy on-failure

  # Provision a local AArch64 cloud image with NoCloud user-data.
  qemu-manage create cloud-vm \
    --image "$HOME/Images/cloud-aarch64.qcow2" \
    --cloud-init-user-data "$HOME/Images/user-data"

  # Create a blank disk and boot an installer ISO with VNC and a UK keyboard.
  qemu-manage create linux \
    --iso "$HOME/Downloads/linux-arm64.iso" \
    --vnc --vnc-password "$VNC_PASSWORD" \
    --keyboard-layout en-gb

  # Use socket_vmnet with environment-or-discovery filesystem paths.
  export QEMU_MANAGE_SOCKET_VMNET_CLIENT=/opt/socket_vmnet/bin/socket_vmnet_client
  export QEMU_MANAGE_SOCKET_VMNET_SOCKET=/opt/homebrew/var/run/socket_vmnet
  qemu-manage create lab \
    --image "$HOME/Images/lab.qcow2" \
    --network socket_vmnet \
    --socket-vmnet-interface shared

  # Provision a persistent bridged daemon for en0, then create the VM.
  qemu-manage create home-assistant \
    --image "$HOME/Images/haos_generic-aarch64.qcow2" \
    --network socket_vmnet \
    --socket-vmnet-interface en0

NAME must come before options. Next: qemu-manage doctor NAME, then qemu-manage start NAME.
`},
	"set": {text: `Change a managed VM configuration. Omitted options keep their current values.

Usage:
  qemu-manage set NAME OPTION [OPTION ...]

Resources and lifecycle:
  --cpus N                       Positive virtual CPU count
  --memory SIZE                  Whole MiB or GiB, such as 4096MiB or 4GiB
  --restart-policy VALUE         Valid values: never, on-failure
  --shutdown-timeout DURATION    Positive whole-second duration, such as 180s
  --rtc-base VALUE               Valid values: utc, localtime
  --metrics-port VALUE            Loopback monitoring port, 1024-65535, or off

Networking:
  --network VALUE                Valid values: user, socket_vmnet
  --forward SPEC                 Repeatable proto:IPv4:host-port:guest-port
  --clear-forwards               Remove existing user-network forwards
  --socket-vmnet-interface NAME  Interface description, such as shared or vlan0

Selecting --network socket_vmnet resolves QEMU_MANAGE_SOCKET_VMNET_CLIENT and
QEMU_MANAGE_SOCKET_VMNET_SOCKET first, then independent discovery. The resolved
absolute paths are persisted in the VM configuration for later starts and launchd.
On an already-socket_vmnet VM, changing only --socket-vmnet-interface preserves
the current persisted client and socket paths; repeat --network socket_vmnet to
refresh them from environment or discovery.

Display and guest integration:
  --guest-agent VALUE            Valid values: on, off
  --vnc VALUE                    Valid values: on, off
  --vnc-password VALUE           Update the stored VNC password
  --vnc-bind IPV4                Update the VNC bind IPv4 address
  --vnc-port PORT                Update the minimum VNC TCP port
  --vnc-port-to PORT             Update the maximum VNC TCP port
  --keyboard-layout LAYOUT       Requires existing VNC or --vnc on

Examples:
  # Built-in user networking with Home Assistant available on localhost:8123.
  qemu-manage set home-assistant --network user \
    --forward tcp:127.0.0.1:8123:8123 --guest-agent on

  # Use socket_vmnet with discovered defaults or exported overrides.
  qemu-manage set home-assistant --network socket_vmnet

  # If doctor warns that the Homebrew client is user-writable, make a root-owned copy.
  sudo install -d -o root -g wheel -m 0755 /opt/socket_vmnet/bin
  sudo install -o root -g wheel -m 0755 \
    "$(brew --prefix socket_vmnet)/bin/socket_vmnet_client" \
    /opt/socket_vmnet/bin/socket_vmnet_client
  export QEMU_MANAGE_SOCKET_VMNET_CLIENT=/opt/socket_vmnet/bin/socket_vmnet_client
  export QEMU_MANAGE_SOCKET_VMNET_SOCKET=/opt/homebrew/var/run/socket_vmnet
  qemu-manage set home-assistant \
    --network socket_vmnet \
    --socket-vmnet-interface shared

  # Update RTC base and the VNC keyboard layout together.
  qemu-manage set linux --rtc-base localtime --keyboard-layout en-gb

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

The rendered command comes only from the durable VM configuration. One-shot
start overrides such as --boot-menu are intentionally not included.

Examples:
  qemu-manage showcmd home-assistant
`},
	"start": {text: `Check prerequisites and start a VM under its authenticated supervisor.

Usage:
  qemu-manage start NAME [--foreground] [--boot-menu]

Options:
  --foreground  Keep the supervisor attached to this terminal (diagnostics)
  --boot-menu   Request the firmware boot menu for this start only; requires
                firmware support plus an interactive console path such as serial
                console or VNC. The value is not persisted and does not appear
                in showcmd output.

Examples:
  qemu-manage doctor home-assistant
  qemu-manage start home-assistant
  qemu-manage --debug start home-assistant --foreground
  qemu-manage start linux --boot-menu
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
	"log": {text: `Print the active bounded serial log verbatim to stdout.

Usage:
  qemu-manage log NAME

The command works while the VM is running or stopped, and rotated backups are
not included.

Examples:
  qemu-manage log home-assistant | less
`},
	"console": {text: `Connect the terminal to a running or paused VM's serial console.

Usage:
  qemu-manage console NAME

Press Ctrl-] to disconnect without stopping the VM.

Examples:
  qemu-manage console home-assistant
`},
	"monitor": {text: `Connect the terminal to a running or paused VM's QEMU human monitor.

Usage:
  qemu-manage monitor NAME
  qemu-manage monitor NAME "COMMAND"

Interactive mode forwards raw terminal I/O to QEMU's HMP socket. Press Ctrl-] to
disconnect without stopping the VM.

With COMMAND, qemu-manage sends one HMP command through QMP. Stdout is only the
returned HMP text, so the one-shot form is safe to pipe.

VMs already running when qemu-manage was upgraded must be restarted once before
monitor can use the new monitor sockets.

Examples:
  qemu-manage monitor home-assistant
  qemu-manage monitor home-assistant "info status"
`},
	"guest-agent": {text: `Send one strict JSON request to a running or paused VM's guest agent.

Usage:
  qemu-manage guest-agent NAME REQUEST

REQUEST must be one JSON object with "execute" and optional "arguments". Enable
the guest agent before starting the VM:
  qemu-manage set NAME --guest-agent on

Stdout is only the compact JSON return value, so this command is safe to pipe.

A VM that was already running before qemu-manage was upgraded can still use
guest-agent if it was started with the guest agent enabled.

Examples:
  qemu-manage guest-agent home-assistant '{"execute":"guest-info"}'
  qemu-manage guest-agent home-assistant '{"execute":"guest-ping"}'
`},
	"vnc": {text: `Copy the configured VNC password and open the live endpoint in Screen Sharing.

Usage:
  qemu-manage vnc NAME

Requirements:
  - macOS with Screen Sharing available through /usr/bin/open
  - VNC enabled in the VM configuration
  - The VM is currently running or paused
  - The authenticated supervisor reports a live VNC endpoint that matches the
    running configuration

Behavior:
  - Copies the configured VNC password to the clipboard with pbcopy
  - Opens Screen Sharing at the authenticated live vnc://HOST:PORT endpoint
  - Does not print the password or clear the clipboard afterward

Examples:
  qemu-manage vnc home-assistant
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
  export QEMU_MANAGE_SOCKET_VMNET_CLIENT=/opt/socket_vmnet/bin/socket_vmnet_client
  export QEMU_MANAGE_SOCKET_VMNET_SOCKET=/opt/homebrew/var/run/socket_vmnet

Then select it with:
  qemu-manage create NAME --network socket_vmnet --socket-vmnet-interface shared
  qemu-manage set NAME --network socket_vmnet --socket-vmnet-interface shared

Environment overrides take precedence over discovery during create and explicit
socket_vmnet selection. The resolved absolute paths persist in the VM config, so
later starts and launchd do not need those variables.

A named check also verifies copied firmware, disks, configured machine support,
configured socket_vmnet paths, and whether the helper socket is connectable.
`},
	"autostart": {text: `Manage automatic VM startup through launchd.

Usage:
  qemu-manage autostart SUBCOMMAND NAME [OPTIONS]

Subcommands:
  enable NAME [--scope VALUE]  Install an autostart job without starting the VM
  disable NAME                 Remove an autostart job for a stopped VM
  status NAME                  Compare configured and installed launchd state

Valid scopes:
  login  Start when this user logs in
  boot   Start at system boot as the configured non-root user

Enabling installs the launchd job without starting the VM. Boot-scope changes
require sudo for the system LaunchDaemon install, but qemu-manage itself still
runs as a normal user.

Examples:
  qemu-manage autostart enable home-assistant --scope login
  qemu-manage autostart status home-assistant

Run 'qemu-manage autostart SUBCOMMAND --help' for details.
`},
	"autostart enable": {text: `Check prerequisites, install a launchd job, and leave the VM stopped.

Usage:
  qemu-manage autostart enable NAME [--scope VALUE]

Options:
  --scope VALUE  Valid values: boot, login (default: boot)

Examples:
  qemu-manage autostart enable home-assistant --scope login
  qemu-manage autostart enable home-assistant --scope boot

Boot scope installs a system LaunchDaemon with sudo. QEMU itself continues to
run as the configured non-root user.
`},
	"autostart disable": {text: `Remove a launchd job for a stopped VM.

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

func rootHelpText(lookupEnv func(string) (string, bool)) string {
	var builder strings.Builder
	builder.WriteString(`qemu-manage manages headless QEMU virtual machines on Apple Silicon.

Usage:
  qemu-manage [-d|--debug] COMMAND [ARGUMENTS]
  qemu-manage [-d|--debug] COMMAND --help
  qemu-manage help [COMMAND [SUBCOMMAND]]
  qemu-manage --version

Options:
  -d, --debug  Emit redacted diagnostic records to stderr for this invocation
  -h, --help   Show progressive help for the current command
  --version    Show version and build information

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
  log          Print the active serial log
  monitor      Connect to the QEMU human monitor or run one HMP command
  guest-agent  Send one JSON request to the guest agent and print its return
  vnc          Copy the VNC password and open Screen Sharing (macOS)
  status       Show one VM or all VM runtime states
  list         List all managed VMs
  doctor       Check QEMU, firmware, VM files, and networking prerequisites
  autostart    Manage login or system-boot startup with launchd

Network choices:
  user          Built into QEMU; simplest setup, with optional port forwards
  socket_vmnet  Host/shared/bridged networking; requires socket_vmnet

Environment:
`)
	for _, variable := range rootHelpEnvVars {
		current := "unset"
		if lookupEnv != nil {
			if value, ok := lookupEnv(variable.name); ok && value != "" {
				current = strconv.Quote(value)
			}
		}
		fmt.Fprintf(&builder, "  %-33s %s Current: %s\n", variable.name, variable.description, current)
	}
	builder.WriteString(`
Examples:
  qemu-manage --help
  qemu-manage --version
  qemu-manage create --help
  qemu-manage --debug doctor
  qemu-manage help guest-agent

Run 'qemu-manage COMMAND --help' for options, examples, and next steps.
`)
	return builder.String()
}

func writeHelp(output io.Writer, topic string, lookupEnv ...func(string) (string, bool)) error {
	help, ok := helpTopics[topic]
	if !ok {
		return fmt.Errorf("unknown help topic %q", topic)
	}
	text := help.text
	if topic == "" {
		var resolver func(string) (string, bool)
		if len(lookupEnv) != 0 {
			resolver = lookupEnv[0]
		}
		text = rootHelpText(resolver)
	}
	_, err := io.WriteString(output, text)
	return err
}
