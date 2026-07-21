---
title: Security
---

`qemu-manage` is designed around per-user VM ownership and narrow trust boundaries. Security-sensitive behavior includes managed storage, process ownership, local control channels, QEMU argument construction, and launchd integration.

## Managed storage

Durable VM state is stored beneath the user's qemu-manage data directory. `internal/store` protects that state with:

- atomic writes, so a failed or interrupted update does not expose a partially written configuration;
- owner-only modes for managed files and directories;
- `O_NOFOLLOW`, ownership checks, and explicit symlink rejection, preventing hostile links from redirecting writes outside managed storage;
- stable per-name locks for configuration mutations; and
- immutable-ID lifetime locks for running-state ownership.

The immutable ID ties a supervisor to one VM incarnation. Reusing a deleted VM's name therefore cannot make a new configuration appear to own the old process.

## Local control surfaces

Each supervisor exposes a Unix control socket for its VM. Socket permissions and peer-UID authentication restrict lifecycle requests to the intended local owner. QMP, QGA, and serial sockets are also treated as private per-VM resources.

The separate per-VM HTTP monitoring endpoint binds only to loopback. It is intended for local callers and is not exposed as a remote management service. Because each VM has an independent supervisor and there is no central daemon, there is no shared privileged service or global control endpoint to compromise.

## Process and argument boundaries

External programs are invoked directly with argument vectors; qemu-manage does not execute a shell. QEMU command lines are rendered deterministically, and manager-owned lifecycle, device, and control arguments are protected from user passthrough. This prevents configuration input from replacing the control sockets, lifecycle behavior, or other arguments qemu-manage relies on to own the child safely.

Supervisors validate process ownership before signaling a child. One supervisor owns one QEMU process and is responsible for reaping and stopping it.

## Privilege boundary

QEMU and the per-VM supervisor run as the configured, unprivileged user. Login-scope LaunchAgents need no system-wide installation. Boot-scope autostart uses `sudo` only for the narrow LaunchDaemon installation and management steps; the generated job still runs the supervisor and QEMU as the non-root VM owner.

Optional bridged `socket_vmnet` provisioning has its own explicit privileged setup. It must not make QEMU run as root or silently fall back to a less restrictive networking mode.

## Reporting vulnerabilities

Do not publish suspected vulnerabilities or sensitive reproduction data in a public issue. Use the repository Security page's **Report a vulnerability** action. If private reporting is unavailable, open a public issue without vulnerability details and ask the maintainer to establish a private channel. Never attach VM images, guest credentials, private keys, tokens, or sensitive logs.
