---
title: Log Management
---

Each VM has a private log directory at:

```text
~/Library/Logs/qemu-manage/NAME/
```

The directory and managed log files use owner-only access. The `QEMU_MANAGE_LOG_ROOT` environment variable can override the default log root.

## Log files

| File | Contents and behavior |
| --- | --- |
| `serial.log` | Active guest serial output; printed by `qemu-manage log NAME` while the VM is running or stopped |
| `serial.log.0` through `serial.log.2` | Up to three older serial-log backups |
| `qemu.log` | QEMU process output, appended across starts; not printed by `log` and not rotated with the serial log |
| `supervisor.stdout.log` | Standard output from the per-VM supervisor |
| `supervisor.stderr.log` | Standard error from the per-VM supervisor |

The active serial log rotates when it reaches 2 MiB. Each active or backup file is kept at or below 2 MiB, up to three backups are retained, and the oldest is discarded on the next rotation. If an active log already exceeds the limit when a supervisor starts, it is truncated before new output is collected.

If serial-log storage fails during a run, the supervisor warns and continues forwarding the live serial stream. Durable serial logging is disabled for that run rather than terminating the VM.

## View the current serial log

`log` prints the active serial log verbatim to stdout, so it can be paged or redirected:

```sh
qemu-manage log home-assistant | less
qemu-manage log home-assistant > serial.log.txt
```

The command deliberately excludes `serial.log.0`, `.1`, and `.2`. Read backup files directly when older serial output is needed.

## Delete logs with the VM

```sh
qemu-manage delete NAME
```

Deleting a VM removes its managed logs together with its configuration, copied firmware, disks, and runtime files. Original source images passed to `--image` or `--iso` are not removed.
