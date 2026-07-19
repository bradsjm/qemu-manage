# qemu-manage

`qemu-manage` is a small command-line manager for headless QEMU virtual machines on Apple Silicon Macs. It manages VM configuration, lifecycle, serial consoles, networking, and launchd autostart without a persistent central daemon.

## Requirements

- Apple Silicon Mac running macOS 13 or newer
- Go 1.25 or newer to build from source
- QEMU for AArch64 guests:

  ```sh
  brew install qemu
  ```

- Optional [`socket_vmnet`](https://github.com/lima-vm/socket_vmnet) installation for shared or bridged networking

The first release supports native AArch64 guests using QEMU's HVF accelerator. It does not silently fall back to cross-architecture emulation.

## Build

```sh
go build -o qemu-manage ./cmd/qemu-manage
./qemu-manage --help
```

To make the command available globally, copy the resulting executable to a directory on `PATH`.

## Getting started

Inspect QEMU and firmware discovery before creating a VM:

```sh
qemu-manage doctor
```

Create a VM from an existing ARM64 qcow2 image:

```sh
qemu-manage create home-assistant \
  --image "$HOME/Downloads/haos_generic-aarch64.qcow2" \
  --cpus 2 \
  --memory 4GiB \
  --disk-size 32GiB \
  --restart-policy on-failure

qemu-manage doctor home-assistant
qemu-manage showcmd home-assistant
qemu-manage start home-assistant
qemu-manage status home-assistant
```

Connect to the guest's serial console and press `Ctrl-]` to disconnect:

```sh
qemu-manage console home-assistant
```

For installation or diagnostics, opt into password-protected VNC:

```sh
qemu-manage create linux --iso "$HOME/Downloads/linux-arm64.iso" \
  --vnc --vnc-password "$VNC_PASSWORD"
qemu-manage start linux
qemu-manage status linux --json
qemu-manage vnc linux
```

VNC is disabled by default and binds to `127.0.0.1` when enabled. QEMU selects a free port in the configured range; JSON status reports the authenticated supervisor's live `vnc` endpoint. On macOS, `qemu-manage vnc NAME` copies the configured password to the clipboard and opens that live endpoint in Screen Sharing. The VM must be running or paused with its current configuration.

Request a graceful shutdown:

```sh
qemu-manage stop home-assistant
```

Run `qemu-manage COMMAND --help` for command-specific options and examples.

## Networking

VMs use QEMU user-mode networking by default. Host forwards bind explicitly to an IPv4 address:

```sh
qemu-manage set home-assistant \
  --network user \
  --forward tcp:127.0.0.1:8123:8123
```

`socket_vmnet` mode provides host/shared/bridged networking without running QEMU as root. It requires a separately installed and running helper service. Use `qemu-manage set NAME --help` and `qemu-manage doctor NAME` to configure and validate it.

## Autostart

Autostart uses a per-VM launchd job:

```sh
# Start after this user logs in.
qemu-manage autostart enable home-assistant --scope login

# Or start at system boot under the VM owner's account.
qemu-manage autostart enable home-assistant --scope boot

qemu-manage autostart status home-assistant
qemu-manage autostart disable home-assistant
```

Boot-scope changes require `sudo` for the narrow LaunchDaemon installation and launchctl operations. QEMU itself still runs as the non-root VM owner.

## Storage

Managed state is stored in macOS user directories:

- VM configuration and managed images: `~/Library/Application Support/qemu-manage/vms`
- Logs: `~/Library/Logs/qemu-manage`
- Ephemeral control sockets and runtime metadata: `/tmp/qemu-manage-<uid>`

Configuration files are strict, versioned JSON. Use `qemu-manage config show`, `config validate`, and `config apply` for complete configuration changes.

Set `QEMU_MANAGE_DATA_ROOT`, `QEMU_MANAGE_RUNTIME_ROOT`, or `QEMU_MANAGE_LOG_ROOT` to replace the corresponding default. Each override must be an absolute, owner-controlled directory; unset or empty variables retain the default. The runtime root must also remain short enough for macOS Unix-socket path limits. Autostart jobs preserve the selected roots explicitly because launchd does not inherit the shell environment.

VM configuration files are owner-only mode `0600`. An enabled VNC password is stored there in plaintext and `qemu-manage config show NAME` prints it. VNC password authentication accepts only 1–8 UTF-8 bytes. VNC transport is not encrypted; binding to an address other than loopback exposes it to that network.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for local checks and contribution expectations. Security reports are handled according to [SECURITY.md](SECURITY.md).

## License

Licensed under the [Apache License 2.0](LICENSE).
