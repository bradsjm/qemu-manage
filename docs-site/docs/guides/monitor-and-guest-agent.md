---
title: Monitor and Guest Agent
---

## QEMU human monitor

Interactive mode connects the terminal directly to the QEMU human monitor (HMP) of a running or paused VM:

```sh
qemu-manage monitor home-assistant
```

qemu-manage does not add a prompt. Press `Ctrl-]` to disconnect without stopping the VM.

To run one HMP command through QMP, provide the command as a second argument:

```sh
qemu-manage monitor home-assistant "info status"
```

In one-shot mode, stdout contains only the returned HMP text and is safe to pipe. VMs that were already running when monitor socket support was introduced must be restarted once.

### Useful HMP commands

| Command | Use |
| --- | --- |
| `help` or `help COMMAND` | List commands or show command-specific help |
| `info status` | Show whether the VM is running, paused, or shutting down |
| `info version` | Show the running QEMU version |
| `info cpus` | Show virtual CPU state |
| `info block` | Show attached block devices and backing files |
| `info network` | Show network devices and backends |
| `info pci` | Show the guest-visible PCI topology |
| `info qtree` | Show QEMU's device tree |
| `info snapshots` | List internal disk snapshots |
| `info registers` | Show the current virtual CPU's registers |
| `stop` / `cont` | Pause or resume virtual CPU execution |
| `system_powerdown` | Request an ACPI guest shutdown |
| `system_reset` | Immediately reset the VM |

Commands depend on the QEMU version and machine configuration. Run `help` against the VM to see what it supports. State-changing commands such as `stop`, `system_powerdown`, and `system_reset` affect the running guest.

## QEMU guest agent

Enable the guest agent before starting the VM:

```sh
qemu-manage set home-assistant --guest-agent on
```

Send one strict JSON object containing `execute` and optional `arguments`:

```sh
qemu-manage guest-agent home-assistant '{"execute":"guest-ping"}'
qemu-manage guest-agent home-assistant '{"execute":"guest-info"}'
qemu-manage guest-agent home-assistant '{"execute":"guest-get-osinfo"}'
```

`guest-ping` checks responsiveness. `guest-info` reports the agent's supported commands; support depends on guest-agent version and guest policy. `guest-get-osinfo` inspects the guest operating system.

The command writes only the compact JSON `return` value to stdout, so it is safe to pipe.

### Run a guest process

If the guest permits `guest-exec`, first request execution and capture the returned `pid`:

```sh
qemu-manage guest-agent home-assistant \
  '{"execute":"guest-exec","arguments":{"path":"/usr/bin/uname","arg":["-a"],"capture-output":true}}'
```

Then substitute that PID into `guest-exec-status`:

```sh
qemu-manage guest-agent home-assistant \
  '{"execute":"guest-exec-status","arguments":{"pid":1234}}'
```

With `capture-output` enabled, completed stdout and stderr are returned as Base64.
