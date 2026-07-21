---
title: Troubleshooting
description: Common issues and their resolutions.
---

# Troubleshooting

## Doctor reports missing prerequisites

```sh
brew install qemu
```

For `socket_vmnet` networking:

```sh
brew install socket_vmnet
sudo "$(brew --prefix)/bin/brew" services start socket_vmnet
```

## Doctor reports a user-writable socket_vmnet client

The socket_vmnet client must be root-owned for security. Make a root-owned copy:

```sh
sudo install -d -o root -g wheel -m 0755 /opt/socket_vmnet/bin
sudo install -o root -g wheel -m 0755 \
  "$(brew --prefix socket_vmnet)/bin/socket_vmnet_client" \
  /opt/socket_vmnet/bin/socket_vmnet_client
export QEMU_MANAGE_SOCKET_VMNET_CLIENT=/opt/socket_vmnet/bin/socket_vmnet_client
export QEMU_MANAGE_SOCKET_VMNET_SOCKET=/opt/homebrew/var/run/socket_vmnet
```

Repeat the `install` step after upgrading `socket_vmnet` with Homebrew.

## Config change says restart required

Some settings (metrics port, network mode) take effect only after restart. Run:

```sh
qemu-manage restart NAME
```

## Cannot connect to console or monitor

VMs that were already running when `qemu-manage` was upgraded must be restarted once before console or monitor can use the new sockets.

## `--share` command fails with missing samba_smbd

QEMU's user-network SMB server invokes a helper at `/opt/homebrew/sbin/samba-dot-org-smbd`. Install it:

```sh
brew install samba
```

## Serial log storage failure

If serial-log storage fails during a run, the supervisor reports a warning and continues forwarding the live serial stream. Durable serial logging is disabled for that run rather than terminating the VM.

## Guest agent requests time out or fail

- Ensure the agent is enabled before starting the VM: `qemu-manage set NAME --guest-agent on`
- The guest must have `qemu-guest-agent` installed and running
- The guest agent is optional; QMP-based monitoring works without it

## Boot menu does not show

`--boot-menu` requires firmware support and an interactive console path such as serial console or VNC. Not all firmware configurations support boot menus.

## Force-stop causes filesystem corruption

`--force` kills QEMU without guest cooperation. Use it only when the guest is unresponsive to ACPI shutdown requests. Prefer graceful stop with a generous `--timeout`.

## VM autostart does not survive Homebrew upgrade

The plist records the pathname used to run `qemu-manage`, not a resolved Homebrew Cellar path. If a plist has already drifted, re-run `autostart enable` to rewrite it in place. Run `qemu-manage doctor NAME` to confirm.
