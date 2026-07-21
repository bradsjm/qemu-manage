---
title: Getting Started
sidebar_position: 2
description: Installation and quick start guide.
---

# Getting Started

## Requirements

- An Apple Silicon Mac running macOS 13 or newer.
- QEMU with `qemu-system-aarch64` and `qemu-img`:

  ```sh
  brew install qemu
  ```

- An AArch64 guest. qemu-manage does not support cross-architecture emulation.
- Go 1.25 or newer only when installing with Go or building from source.
- Optional [`socket_vmnet`](https://github.com/lima-vm/socket_vmnet) for shared or bridged networking:

  ```sh
  brew install socket_vmnet
  ```

- Optional Samba for exporting a host folder over SMB with `--share`:

  ```sh
  brew install samba
  ```

## Install qemu-manage

Choose one installation method.

### Homebrew

Install the prebuilt Apple Silicon binary from the project tap:

```sh
brew install bradsjm/tap/qemu-manage
```

The formula installs QEMU as a dependency. Install `socket_vmnet` separately if you need shared or bridged networking.

### GitHub release

Download a release archive and its checksums. Replace `0.6.1` with the version you want:

```sh
VERSION=0.6.1
curl -fLO "https://github.com/bradsjm/qemu-manage/releases/download/v${VERSION}/qemu-manage_${VERSION}_darwin_arm64.tar.gz"
curl -fLO "https://github.com/bradsjm/qemu-manage/releases/download/v${VERSION}/checksums.txt"
shasum -a 256 -c checksums.txt
tar -xzf "qemu-manage_${VERSION}_darwin_arm64.tar.gz"
mkdir -p "$HOME/.local/bin"
install -m 0755 qemu-manage "$HOME/.local/bin/qemu-manage"
```

Add `$HOME/.local/bin` to your `PATH` if it is not already present. Release archives are unsigned and target Apple Silicon; macOS may require approval in Privacy & Security.

### Install with Go

```sh
# Latest release
go install github.com/bradsjm/qemu-manage/cmd/qemu-manage@latest

# Specific release
go install github.com/bradsjm/qemu-manage/cmd/qemu-manage@v0.6.1
```

This method requires Go 1.25 or newer.

### Build from source

From the repository root:

```sh
go build -o qemu-manage ./cmd/qemu-manage
./qemu-manage --help
./qemu-manage --version
```

Copy the resulting binary to a directory on your `PATH` if you want to invoke it globally.

## Quick start

First, check whether QEMU and matching AArch64 firmware are discoverable:

```sh
qemu-manage doctor
```

Create a VM with two virtual CPUs, 4 GiB of memory, user-mode networking, an SSH port forward, and QEMU Guest Agent support:

```sh
qemu-manage create my-vm \
  --cpus 2 \
  --memory 4GiB \
  --network user \
  --forward tcp:127.0.0.1:2222:22 \
  --guest-agent on
```

This creates a blank managed disk. For an immediately bootable guest, add `--image` with a local or HTTP(S) AArch64 image, or use `--iso` with an AArch64 installer ISO. Before starting, inspect the exact safely quoted QEMU command:

```sh
qemu-manage showcmd my-vm
```

Start the VM with its supervisor attached to the terminal so diagnostics remain visible:

```sh
qemu-manage start my-vm --foreground
```

Leave that process running. In a second terminal, inspect the VM and its serial log:

```sh
qemu-manage status my-vm
qemu-manage log my-vm
```

Connect to the interactive serial console:

```sh
qemu-manage console my-vm
```

Press <kbd>Ctrl</kbd>+<kbd>]</kbd> to disconnect from the console without stopping the VM. Then stop it gracefully:

```sh
qemu-manage stop my-vm
```

Run `qemu-manage COMMAND --help` for command-specific options and examples.

## Doctor checks

`qemu-manage doctor` checks host prerequisites without changing anything, including QEMU executables and matching firmware discovery. Pass a VM name to also validate its copied firmware, disks, configured QEMU machine, configured `socket_vmnet` paths, and helper-socket connectivity:

```sh
qemu-manage doctor my-vm
```

Use `--json` when you need machine-readable results:

```sh
qemu-manage doctor --json
```

## AI Agent Integration

Install the repository's skill definitions to give compatible AI coding agents project-specific command, configuration, architecture, and troubleshooting context:

```sh
npx skills add bradsjm/qemu-manage/skills
```
