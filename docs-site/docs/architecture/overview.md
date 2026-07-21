---
title: Architecture Overview
---

`qemu-manage` is a single Go binary that creates and operates headless AArch64 QEMU virtual machines on Apple Silicon Macs. The executable is both the user-facing CLI and, when re-executed in an internal mode, the runtime supervisor. There is no central daemon, database, or shared service process.

## Dependency direction

Dependencies flow one way from the executable entry point into the CLI and then into focused internal packages:

```text
cmd/qemu-manage -> internal/cli
internal/cli -> model, store, backend/qemu, supervisor, lifecycle,
                launchd, console
internal/supervisor -> internal/monitoring
```

This keeps command parsing and application wiring at the edge while model, persistence, VM control, and operating-system integration remain separated.

## Package responsibilities

| Package | Responsibility |
| --- | --- |
| `cmd/qemu-manage` | The sole executable entry point. It creates `cli.NewApp()`, passes process I/O and arguments to `App.Run`, and exits with the returned status. |
| `internal/cli` | Parses commands and wires collaborators for configuration, runtime, autostart, diagnostics, and user-facing help. |
| `internal/model` | Defines the durable, versioned configuration schema, validates it, creates VM IDs, and computes canonical configuration hashes. |
| `internal/store` | Owns the managed filesystem layout, secure paths, atomic saves, artifact operations, stable per-name locks, and immutable-ID lifetime locks. |
| `internal/backend` | Defines backend-neutral `Backend` and running `Instance` contracts and the backend registry. |
| `internal/qemu` | Implements the QEMU backend: deterministic argument rendering, child-process ownership, QMP/QGA clients, readiness, and host diagnostics. |
| `internal/supervisor` | Runs the per-VM state machine, owns one QEMU child, writes runtime metadata, handles signals, and serves the authenticated control protocol. |
| `internal/lifecycle` | Coordinates stop requests and derives status from supervisor control or conservative runtime evidence. |
| `internal/monitoring` | Caches QMP, optional QGA, and process observations and exposes them through a per-VM loopback HTTP surface owned by the supervisor. |
| `internal/launchd` | Renders per-VM LaunchAgent or LaunchDaemon plists and manages login- and boot-scope jobs transactionally. |
| `internal/console` | Proxies the serial console through a raw terminal and handles `Ctrl-]` disconnects. |

## One supervisor per VM

`qemu-manage start NAME` re-executes the same binary in the hidden `supervise` mode. That new process is dedicated to one VM and owns exactly one QEMU child. It holds a lifetime lock keyed by the VM's immutable ID, records runtime metadata, and serves a Unix control socket whose peer is authenticated.

The immutable-ID lock is deliberately different from the stable per-name lock used for configuration changes. Separating them prevents a deleted-and-recreated VM with the same name from being confused with the lifetime of the old instance.

Lifecycle commands use `internal/lifecycle` to communicate with the supervisor. If authenticated control is unavailable, status is derived conservatively from the lifetime lock, `runtime.json`, and `last_exit.json` rather than assuming that stale metadata proves a VM is running.

## Backend and control protocols

`internal/backend` keeps lifecycle orchestration independent of a specific hypervisor implementation; `internal/qemu` supplies the concrete implementation. QEMU Machine Protocol (QMP) provides readiness, status, and power control. The QEMU Guest Agent (QGA) is optional and contributes guest observations when configured.

Manual starts and launchd autostart use the same foreground supervisor path. launchd changes who owns the supervisor process lifecycle, not the VM execution architecture.
