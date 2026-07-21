---
title: Starting and Inspecting VMs
---

## Start a VM

```sh
qemu-manage start NAME
```

`start` checks prerequisites and runs the VM under its authenticated supervisor. Add `--foreground` to keep the supervisor attached to the terminal for diagnostics:

```sh
qemu-manage --debug start home-assistant --foreground
```

`--boot-menu` requests the firmware boot menu for this start only. It requires firmware support and an interactive path such as the serial console or VNC. The option is not persisted.

```sh
qemu-manage start linux --boot-menu
```

With configured login or boot autostart, `start` uses the VM's launchd job. Otherwise it launches a detached supervisor. `--boot-menu` always uses a one-off detached instance.

## Stop and restart

A normal stop requests a graceful guest shutdown through the guest agent or QMP:

```sh
qemu-manage stop NAME
qemu-manage stop NAME --timeout 5m
```

`--timeout DURATION` overrides the VM's configured whole-second shutdown timeout. `--force` kills QEMU without guest cooperation and can corrupt guest filesystems or data.

`restart` stops the VM, waits until it is fully stopped, and then starts it under its supervisor. It accepts stop- and start-phase options together:

```sh
qemu-manage restart NAME
qemu-manage restart NAME --timeout 5m
qemu-manage restart NAME --force
qemu-manage restart NAME --boot-menu
qemu-manage restart NAME --foreground
```

If the VM is already stopped, the start still proceeds. A stop failure aborts before the start is attempted.

## Inspect runtime state

Use `list` for a Docker-`ps`-style overview of every managed VM:

```sh
qemu-manage list
qemu-manage list --json
```

The human table includes `NAME`, `STATE`, `CPUS`, `MEMORY`, `NETWORK`, `AUTOSTART`, `VNC`, `RESTART`, and `ERROR`. Invalid configurations retain their name, failed state, and error while unavailable configuration or live values appear as `-`. The JSON form preserves the existing status fields and adds the durable resource and lifecycle settings.

`status` retains its compact four-column human output and can inspect all VMs or one named VM:

```sh
qemu-manage status
qemu-manage status NAME
qemu-manage status NAME --json
```

For a VNC-enabled running VM, status JSON includes the authenticated live VNC endpoint.

## Inspect rich VM information

```sh
qemu-manage info NAME
qemu-manage info NAME --json
```

A stopped VM reports only that it is not running. For a running or paused VM, `info` combines authenticated supervisor state and durable configuration with the VM's loopback `/info` and `/status` monitoring responses. It validates the VM identity, backend PID, and run start time before displaying monitoring data, so a restart racing the request cannot expose information from the wrong run.

If monitoring is disabled or unavailable, the command still succeeds with authenticated supervisor/configuration information and explains the fallback. Enable monitoring with:

```sh
qemu-manage set NAME --metrics-port PORT
```

Restart the VM after changing its metrics configuration.

## Inspect the QEMU command

```sh
qemu-manage showcmd NAME
```

`showcmd` prints the exact, safely quoted QEMU command derived from durable VM configuration without starting the VM. One-shot overrides such as `--boot-menu` do not appear.

## Connect to the serial console

```sh
qemu-manage console NAME
```

The VM must be running or paused. Press `Ctrl-]` to disconnect without stopping it.

## Open VNC

```sh
qemu-manage vnc NAME
```

On macOS, for a running or paused VNC-enabled VM, this copies the configured password with `pbcopy` and opens the authenticated live `vnc://HOST:PORT` endpoint in Screen Sharing via `/usr/bin/open`. The password is not printed and is not cleared from the clipboard afterward.
