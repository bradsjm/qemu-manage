---
title: Autostart with launchd
---

qemu-manage installs one launchd job per VM. Whether started manually or by launchd, the VM follows the same per-VM supervisor path.

## Choose a scope

| Scope | launchd job | Starts | Privilege |
| --- | --- | --- | --- |
| `login` | LaunchAgent | When the VM owner logs in | Installed in the user's LaunchAgents directory. |
| `boot` | LaunchDaemon | At system boot, under the VM owner's account | Uses `sudo` for the narrow system LaunchDaemon installation and management steps. The supervisor and QEMU still run as the configured non-root user. |

`boot` is the default for `autostart enable`; use `--scope login` when the VM should start only after the user logs in.

## Enable autostart

Install a login-scope job without starting the VM:

```sh
qemu-manage autostart enable home-assistant --scope login
```

Install a boot-scope job:

```sh
qemu-manage autostart enable home-assistant --scope boot
```

By default, `enable` installs the job and leaves the VM stopped. Add `--start` to install and start it immediately through launchd:

```sh
qemu-manage autostart enable home-assistant --scope boot --start
```

When autostart is configured, a later `qemu-manage start NAME` starts the VM through its launchd job so launchd owns the supervisor lifecycle. No manual `launchctl` command is needed.

## Inspect status

```sh
qemu-manage autostart status home-assistant
```

`status` compares the configured autostart setting with the installed and loaded launchd job and reports whether the installed plist matches.

## Disable autostart

Stop a running VM before disabling autostart:

```sh
qemu-manage stop home-assistant
qemu-manage autostart disable home-assistant
```

`disable` refuses while the VM is running. For a stopped VM, it boots out any loaded job and removes its plist. Boot-scope removal uses `sudo` for the privileged launchd steps. The optional `--scope` flag is accepted for symmetry, but a VM has only one configured autostart scope, so disable removes that job:

```sh
qemu-manage autostart disable home-assistant --scope boot
```

## Homebrew upgrades

The generated plist records the pathname used to invoke `qemu-manage`, such as `/opt/homebrew/bin/qemu-manage`. It deliberately does not resolve that path to a versioned Homebrew Cellar target. When Homebrew repoints its stable path during an upgrade, existing launchd jobs continue to find the current binary instead of being orphaned.

If an older plist has drifted, run `enable` again with the intended scope. qemu-manage rewrites the plist in place and reports `Reconciled`; then use `doctor` to check the VM:

```sh
brew upgrade qemu-manage
qemu-manage autostart enable home-assistant --scope boot
qemu-manage doctor home-assistant
```
