---
title: CLI Reference
---

`qemu-manage` manages headless AArch64 QEMU virtual machines on Apple Silicon macOS.

## Global options

| Option | Description |
|---|---|
| `--debug` | Enable debug logging. Place it before the command, for example `qemu-manage --debug start NAME --foreground`. |
| `-h`, `-help`, `--help` | Show root or command-specific help. You can also use `qemu-manage help COMMAND`. |
| `--version` | Print the program version. |

## Commands

| Command | Usage | Description |
|---|---|---|
| `create` | `qemu-manage create NAME [OPTIONS]` | Create a managed AArch64 VM. Source images and firmware are copied; extra drive files remain external references. |
| `set` | `qemu-manage set NAME OPTION [OPTION ...]` | Change selected settings while preserving omitted values. |
| `config` | `qemu-manage config SUBCOMMAND [ARGUMENTS]` | Inspect, validate, or replace a complete strict JSON VM configuration. |
| `config show` | `qemu-manage config show NAME` | Print a VM's complete canonical JSON configuration. |
| `config validate` | `qemu-manage config validate FILE` | Strictly validate a configuration without changing a VM. |
| `config apply` | `qemu-manage config apply NAME FILE` | Validate and atomically replace a VM's editable settings. Identity, backend, architecture, and autostart scope must match. |
| `showcmd` | `qemu-manage showcmd NAME` | Print the exact safely quoted QEMU command derived from durable configuration without starting the VM. |
| `start` | `qemu-manage start NAME [--foreground] [--boot-menu]` | Check prerequisites and start a VM under its authenticated supervisor. `--boot-menu` is a one-start override. |
| `stop` | `qemu-manage stop NAME [--timeout DURATION] [--force]` | Request graceful shutdown through QGA or QMP. `--force` kills QEMU without guest cooperation. |
| `restart` | `qemu-manage restart NAME [OPTIONS]` | Stop and then start a VM. Accepts `--timeout`, `--force`, `--boot-menu`, and `--foreground`. |
| `console` | `qemu-manage console NAME` | Attach to the serial console of a running or paused VM. Press `Ctrl-]` to disconnect. |
| `log` | `qemu-manage log NAME` | Print the active bounded serial log verbatim; rotated backups are not included. |
| `monitor` | `qemu-manage monitor NAME` or `qemu-manage monitor NAME "COMMAND"` | Attach interactively to QEMU's human monitor, or issue one HMP command through QMP. |
| `guest-agent` | `qemu-manage guest-agent NAME REQUEST` | Send one strict JSON request object containing `execute` and optional `arguments` to QGA. |
| `vnc` | `qemu-manage vnc NAME` | Copy the configured VNC password to the clipboard and open the authenticated live endpoint in macOS Screen Sharing. |
| `status` | `qemu-manage status [NAME] [--json]` | Show runtime state and whether changed configuration requires a restart. |
| `list` | `qemu-manage list [--json]` | List all managed VMs and their runtime states. |
| `doctor` | `qemu-manage doctor [NAME] [--json]` | Check host or named-VM prerequisites without changing anything. |
| `autostart` | `qemu-manage autostart SUBCOMMAND NAME [OPTIONS]` | Manage VM startup through launchd. |
| `autostart enable` | `qemu-manage autostart enable NAME [--scope VALUE] [--start]` | Install a `boot` or `login` launchd job; optionally start through it immediately. Default scope is `boot`. |
| `autostart disable` | `qemu-manage autostart disable NAME [--scope VALUE]` | Remove the stopped VM's launchd job. The optional scope is accepted for symmetry. |
| `autostart status` | `qemu-manage autostart status NAME` | Show configured, installed, matching, and loaded launchd state. |
| `delete` | `qemu-manage delete NAME [--force]` | Permanently delete a stopped VM and its managed files. A running VM or one with autostart is refused. |
| `supervise` | `qemu-manage supervise NAME --ready-fd FD --expected-id ID` | Internal supervisor entry point. Users should run `start` instead. |

Use `qemu-manage COMMAND --help` (or `qemu-manage config SUBCOMMAND --help` / `qemu-manage autostart SUBCOMMAND --help`) for the complete option descriptions and examples.

## Environment variables

| Variable | Description |
|---|---|
| `QEMU_MANAGE_LOG_ROOT` | Absolute owner-controlled log directory. Default: `~/Library/Logs/qemu-manage`. |
| `QEMU_MANAGE_SOCKET_VMNET_CLIENT` | Absolute path to a root-owned `socket_vmnet_client`; otherwise discovered from Homebrew or MacPorts. |
| `QEMU_MANAGE_SOCKET_VMNET_SOCKET` | Absolute `socket_vmnet` daemon socket path; otherwise discovered from Homebrew or MacPorts. |
