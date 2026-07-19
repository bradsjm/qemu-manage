# qemu-manage

[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Coding Harness](https://img.shields.io/badge/coding_harness-oh--my--pi/gpt--5.6-orange)](https://opencode.ai/)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/bradsjm/qemu-manage)

![qemu-manage logo](./artifacts/banner.png)

**A single-binary CLI for managing headless AArch64 QEMU virtual machines on Apple Silicon.** No persistent daemon, no database, no shell execution — just declarative JSON configs, per-VM supervisors, and authenticated control sockets.

## Features

- **VM lifecycle** — Create, start, stop, and inspect VMs with straightforward commands.
- **Disk import** — Pull images from HTTP(S) URLs (auto-decompresses `.xz`/`.gz`), copy local qcow2/raw images, or boot installer ISOs.
- **Serial console** — Connect to any guest with `Ctrl-]` disconnect handling.
- **Monitor & guest agent** — Use the interactive QEMU human monitor, run one-shot HMP commands, and send strict JSON guest-agent requests with pipe-safe stdout.
- **VNC passthrough** — Optional VNC with password auth; `qemu-manage vnc NAME` opens it in Screen Sharing with the password on your clipboard.
- **Networking** — User-mode NAT out of the box; optional `socket_vmnet` for shared or bridged mode without running QEMU as root.
- **Autostart** — Per-VM launchd jobs at login or boot scope; QEMU stays unprivileged.
- **Secure by design** — Atomic writes, owner-only file modes, peer-authenticated Unix sockets, immutable-ID lifetime locks, and no central service to attack.
- **Accelerated hardware** — All AArch64 guests use QEMU's HVF (Hypervisor.framework) native hardware virtualization accelerator for MacOS.

## Requirements

- Apple Silicon Mac running macOS 13 or newer
- Go 1.25+ to build from source (or use a prebuilt release)
- QEMU for AArch64 guests ([`qemu-system-aarch64`](https://formulae.brew.sh/formula/qemu))
- Only AArch64 guests are supported; no cross-architecture emulation
- _Optional [`socket_vmnet`](https://github.com/lima-vm/socket_vmnet) for shared or bridged networking_

  ```sh
  brew install qemu
  brew install socket_vmnet
  ```

  `qemu-manage` resolves `QEMU_MANAGE_SOCKET_VMNET_CLIENT` and
  `QEMU_MANAGE_SOCKET_VMNET_SOCKET` first, then falls back to independent
  discovery from common Homebrew or MacPorts locations.

## Installation

### GitHub release

Download the latest archive from [GitHub Releases](https://github.com/bradsjm/qemu-manage/releases/latest). Archives are unsigned and target Apple Silicon only. macOS may ask you to approve the binary in Privacy & Security.

Replace `0.1.0` with the version you want to install:

```sh
VERSION=0.1.0
curl -fLO "https://github.com/bradsjm/qemu-manage/releases/download/v${VERSION}/qemu-manage_${VERSION}_darwin_arm64.tar.gz"
curl -fLO "https://github.com/bradsjm/qemu-manage/releases/download/v${VERSION}/checksums.txt"
shasum -a 256 -c checksums.txt
tar -xzf "qemu-manage_${VERSION}_darwin_arm64.tar.gz"
mkdir -p "$HOME/.local/bin"
install -m 0755 qemu-manage "$HOME/.local/bin/qemu-manage"
```

Make sure `$HOME/.local/bin` is on your `PATH`.

### Install with Go

```sh
# Latest release
go install github.com/bradsjm/qemu-manage/cmd/qemu-manage@latest

# Specific version
go install github.com/bradsjm/qemu-manage/cmd/qemu-manage@v0.1.0
```

Requires Go 1.25+ and builds locally — no unsigned binary needed.

### Build from source

```sh
go build -o qemu-manage ./cmd/qemu-manage
./qemu-manage --help
```

Copy the resulting binary to a directory on your `PATH` to make it globally available.

## Quick start

Check that QEMU and firmware are discoverable:

```sh
qemu-manage doctor
```

Create a VM, inspect the rendered command, start it with diagnostics, connect to
its serial console, then shut it down:

```sh
qemu-manage create my-vm \
  --cpus 2 \
  --memory 4GiB \
  --network user \
  --forward tcp:127.0.0.1:2222:22 \
  --guest-agent on \
  --rtc-base utc

qemu-manage doctor my-vm
qemu-manage showcmd my-vm
qemu-manage --debug start my-vm
qemu-manage status my-vm

# Ctrl-] to disconnect
qemu-manage console my-vm

qemu-manage stop my-vm
```

Run `qemu-manage COMMAND --help` for command-specific options, examples, and
environment details.

## Creating a VM

You can create a VM from an HTTP(S) disk image, a local image file, an ARM64
installer ISO, or leave it blank. New VMs default to user networking, guest
agent off, RTC base `utc`, and a concrete Arm machine pinned from the selected
QEMU binary as `virt-N.N`. When VNC is enabled, the default keyboard layout is
`en-us`.

### Import from an HTTP(S) URL

Pass a qcow2 or raw URL to `--image`. URLs ending in `.xz` or `.gz` are
decompressed while downloading and converted to a managed qcow2 disk:

```sh
qemu-manage create home-assistant \
  --image "https://github.com/home-assistant/operating-system/releases/download/18.0/haos_generic-aarch64-18.0.qcow2.xz" \
  --cpus 2 \
  --memory 4GiB \
  --disk-size 32GiB \
  --network user \
  --forward tcp:127.0.0.1:2222:22 \
  --guest-agent on \
  --rtc-base utc \
  --restart-policy on-failure
```

### Import a local image

Pass a local qcow2 or raw path to `--image`. The source is copied and converted
— the original file is never touched:

```sh
qemu-manage create appliance \
  --image "$HOME/Downloads/appliance-aarch64.qcow2" \
  --cpus 2 \
  --memory 4GiB \
  --disk-size 32GiB
```

### Install from an ARM64 ISO

Pass a local ISO to `--iso`. The tool creates the qcow2 disk, copies the ISO
into managed storage, and boots ISO-first. VNC is useful for graphical
installers:

```sh
qemu-manage create linux \
  --iso "$HOME/Downloads/linux-arm64.iso" \
  --cpus 4 \
  --memory 4GiB \
  --disk-size 64GiB \
  --vnc \
  --vnc-password "$VNC_PASSWORD" \
  --keyboard-layout en-gb
```

VNC passwords must be 1–8 UTF-8 bytes; set `VNC_PASSWORD` in your environment
before running.

### Blank disk

Omit both `--image` and `--iso` and you get a blank 32 GiB qcow2 disk by
default.

### Repeatable extra drives

Repeat `--drive` to append extra virtio disks after the managed primary disk:

```text
--drive file=PATH[,if=virtio][,format=raw|qcow2][,cache=none|writeback|writethrough|directsync|unsafe][,aio=threads|native][,readonly=on|off]
```

Relative paths are resolved to absolute external references in the stored
config. Double each literal comma in a value as `,,`. `qemu-manage` never
copies, resizes, converts, chmods, or deletes those extra drive files, so they
must remain readable and in place. Omitted `format` is detected from the file
header; omitted `if` means `virtio`. `aio=native` still depends on host and
QEMU support.

```sh
qemu-manage create lab \
  --image "$HOME/Images/lab.qcow2" \
  --drive "file=disk.img,if=virtio,cache=none,aio=native" \
  --drive "file=archive.qcow2,format=qcow2,readonly=on"
```

### Repeatable USB passthrough

Repeat `--usb` with either exact selector form:

```text
--usb vendor=VVVV,product=PPPP
--usb bus=N,address=N
```

Vendor/product is usually the stable choice across replugging. Bus/address can
change after the device is unplugged and replugged. Without VNC, up to four USB
selections fit; with VNC, up to two fit because QEMU adds a USB keyboard and
tablet.

```sh
qemu-manage create lab \
  --image "$HOME/Images/lab.qcow2" \
  --usb vendor=1d6b,product=0002 \
  --usb bus=1,address=2
```

## Networking

VMs use QEMU user-mode networking by default. Port forwards bind explicitly to
an IPv4 address:

```sh
qemu-manage set home-assistant \
  --network user \
  --forward tcp:127.0.0.1:8123:8123
```

`socket_vmnet` provides host/shared/bridged networking without running QEMU as
root. During `create` and explicit `set --network socket_vmnet`, `qemu-manage`
resolves `QEMU_MANAGE_SOCKET_VMNET_CLIENT` and
`QEMU_MANAGE_SOCKET_VMNET_SOCKET` first, then falls back to independent
discovery. The resolved absolute paths are persisted in the VM config, so later
starts and launchd do not need those shell variables.

If `doctor` warns that the Homebrew client is user-writable, make a root-owned
copy first:

```sh
sudo install -d -o root -g wheel -m 0755 /opt/socket_vmnet/bin
sudo install -o root -g wheel -m 0755 \
  "$(brew --prefix socket_vmnet)/bin/socket_vmnet_client" \
  /opt/socket_vmnet/bin/socket_vmnet_client
```

Then export the desired paths before `create` or explicit reselection:

```sh
export QEMU_MANAGE_SOCKET_VMNET_CLIENT=/opt/socket_vmnet/bin/socket_vmnet_client
export QEMU_MANAGE_SOCKET_VMNET_SOCKET=/opt/homebrew/var/run/socket_vmnet

qemu-manage create lab \
  --image "$HOME/Images/lab.qcow2" \
  --network socket_vmnet \
  --socket-vmnet-interface shared

qemu-manage set home-assistant \
  --network socket_vmnet \
  --socket-vmnet-interface shared
```

On an already-`socket_vmnet` VM, changing only `--socket-vmnet-interface`
preserves the currently persisted client and socket paths. Repeat
`--network socket_vmnet` when you want environment or discovery changes to
refresh the stored paths.

## Starting and inspecting a VM

Use `showcmd` to inspect the durable QEMU argv without launching anything:

```sh
qemu-manage showcmd home-assistant
```

One-shot start overrides such as `--boot-menu` are intentionally absent from
`showcmd` because they are not persisted.

Start-time diagnostics are enabled only by a leading global flag:

```sh
qemu-manage --debug start home-assistant --foreground
```

Use `--boot-menu` when firmware and your console path support interacting with a
one-shot boot chooser:

```sh
qemu-manage start linux --boot-menu
qemu-manage status linux --json    # shows live VNC endpoint when enabled
qemu-manage vnc linux              # opens in Screen Sharing, password on clipboard
```

## Monitor and guest agent

Use `qemu-manage monitor --help` and `qemu-manage guest-agent --help` for the complete command contracts.

### Monitor

Interactive monitor mode connects your terminal directly to QEMU's human monitor:

```sh
qemu-manage monitor home-assistant
```

Press `Ctrl-]` to disconnect without stopping the VM. `qemu-manage` does not add its own prompt; you interact with QEMU's HMP prompt directly.

You can also run one HMP command through QMP:

```sh
qemu-manage monitor home-assistant "info status"
```

In that one-shot form, stdout is only the returned HMP text, so it is safe to pipe into other tools or scripts.

VMs that were already running when `qemu-manage` was upgraded must be restarted once before either monitor mode can use the new monitor sockets.

### Guest agent

Enable the guest agent before starting the VM:

```sh
qemu-manage set home-assistant --guest-agent on
```

Then send one strict JSON request object:

```sh
qemu-manage guest-agent home-assistant '{"execute":"guest-info"}'
qemu-manage guest-agent home-assistant '{"execute":"guest-ping"}'
```

Stdout is only the compact JSON `return` value, so this command is safe to pipe. A VM that was already running before `qemu-manage` was upgraded can still use `guest-agent` without a restart if that VM was started with the guest agent enabled.

## Autostart

Autostart renders per-VM launchd jobs. Manual starts and autostart use the same supervisor path:

```sh
# Start after this user logs in.
qemu-manage autostart enable home-assistant --scope login

# Or start at system boot under the VM owner's account.
qemu-manage autostart enable home-assistant --scope boot

qemu-manage autostart status home-assistant
qemu-manage autostart disable home-assistant
```

Boot-scope changes require `sudo` for the narrow LaunchDaemon installation and `launchctl` operations. QEMU itself still runs as the non-root VM owner.

## Storage & security

Managed state lives in standard macOS user directories:

- VM configs and managed images — `~/Library/Application Support/qemu-manage/vms`
- Logs — `~/Library/Logs/qemu-manage`
- Control sockets and runtime metadata (ephemeral) — `/tmp/qemu-manage-<uid>`

Supported environment variables:

- `QEMU_MANAGE_DATA_ROOT` — Override the VM data root with an absolute,
  owner-controlled directory.
- `QEMU_MANAGE_RUNTIME_ROOT` — Override the runtime root with an absolute,
  owner-controlled directory. Keep it short enough for macOS Unix-socket path
  limits.
- `QEMU_MANAGE_LOG_ROOT` — Override the log root with an absolute,
  owner-controlled directory.
- `QEMU_MANAGE_SOCKET_VMNET_CLIENT` — Override the `socket_vmnet_client`
  executable path used during explicit `socket_vmnet` resolution.
- `QEMU_MANAGE_SOCKET_VMNET_SOCKET` — Override the `socket_vmnet` daemon socket
  path used during explicit `socket_vmnet` resolution.

Autostart jobs preserve explicit roots and persisted `socket_vmnet` paths
because launchd does not inherit your shell environment.
Configuration files are strict, versioned JSON with owner-only mode `0600`. Use `qemu-manage config show`, `config validate`, and `config apply` for full configuration management.

> **VNC security note:** An enabled VNC password is stored in plaintext in the config file, and `qemu-manage config show NAME` prints it. VNC transport is not encrypted; binding to an address other than loopback exposes it to the network.

## How it works

`qemu-manage` is a single binary with no central daemon. The `start` command re-execs itself in a hidden `supervise` mode — one supervisor per running VM, each owning its QEMU child process, its immutable-ID lifetime lock, runtime metadata, and an authenticated Unix control socket:

```mermaid
flowchart LR
    User["User"] --> Binary["cmd/qemu-manage"]
    Launchd["launchd job"] --> Binary
    Binary --> CLI["internal/cli<br/>dispatch and wiring"]

    CLI --> Config["internal/model + internal/store<br/>desired config, secure files, locks"]
    CLI --> Lifecycle["internal/lifecycle + internal/console<br/>status, stop, serial console"]
    CLI -->|"start re-execs as supervise"| Supervisor["internal/supervisor<br/>one per running VM"]
    Lifecycle -->|"authenticated Unix socket"| Supervisor
    Launchd -->|"same supervisor path"| Supervisor

    Supervisor --> Backend["internal/backend + internal/qemu<br/>argv, QMP, optional QGA"]
    Backend --> QEMU["unprivileged QEMU child"]
    Supervisor --> Runtime["runtime metadata<br/>control socket + lifetime lock"]
```

**Key design decisions:**

- **Desired vs. live state** — Durable JSON configs store only what you want. Live state comes from the supervisor and QEMU control protocols (QMP/QGA).
- **No central service** — Every VM supervisor is an independent process. There is no shared daemon, database, or background agent — just the binary, its config files, and per-VM locks.
- **Minimal privilege** — QEMU runs unprivileged. The only `sudo` operations are narrow LaunchDaemon installs for boot-scope autostart.

## Development

Built with code assistance from the [Oh My Pi](https://omp.sh/) harness using GPT 5.6.

See [CONTRIBUTING.md](CONTRIBUTING.md) for local checks and contribution expectations. Security reports are handled according to [SECURITY.md](SECURITY.md).

## License

Licensed under the [Apache License 2.0](LICENSE).
