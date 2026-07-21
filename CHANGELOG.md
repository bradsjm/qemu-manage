# Changelog

All notable changes to qemu-manage are documented in this file.


## [Unreleased]

### Added
- New `info` command that inspects one VM's authenticated runtime state and validated loopback monitoring data, falling back to supervisor state on any mismatch or error
- New monitoring fetch pipeline with VM identity (ID + name) and run-binding (PID + started_at) validation against authenticated supervisor metadata
- `StartedAt` field propagated from supervisor through lifecycle status and CLI runtime layers, exposed in status/list JSON output
- `list` output enriched with Docker-ps-style columns: CPUS, MEMORY, NETWORK, AUTOSTART, and VNC
- Comprehensive test coverage for the `info` command, monitoring validation, identity binding, and list/status output contracts

### Changed
- `list` output now displays a richer table with resource and network columns alongside state and error information
## [0.6.1] - 2026-07-21

### Added
- `qemu-manage start NAME` now runs a VM through its launchd job when autostart is configured (login or boot), so the running instance is launchd-owned and survives reboots — no manual `launchctl` is ever required. This mirrors `systemctl start`. With no autostart scope, `start` keeps the existing detached-supervisor behavior.
- `qemu-manage autostart enable NAME --start` installs the job and starts the VM now through launchd, mirroring `systemctl enable --now`.

## [0.6.0] - 2026-07-21

### Added
- `qemu-manage restart NAME` convenience command that stops and starts a VM in sequence, accepting the stop-phase (`--timeout`, `--force`) and start-phase (`--boot-menu`, `--foreground`) options together; a stop failure aborts before the start is attempted.
- Added output abstraction layer with presentation writers, ANSI stripping, and animated spinners

### Changed
- Replaced go-pretty with pterm as the unified terminal output library
- Rewrote progress bars with custom pterm-styled byte-level bars and waiting helpers
- Switched table rendering to pterm with conditional ANSI stripping for redirected output

### Fixed
- Boot- and login-scope autostart no longer break after a `brew upgrade`: launchd plists now record the pathname used to run `qemu-manage` (for example `/opt/homebrew/bin/qemu-manage`) instead of a resolved Homebrew Cellar path that the upgrade removes. Re-running `autostart enable` reconciles already-drifted plists in place and reports `Reconciled`.
- `autostart disable` now removes a boot-scope LaunchDaemon when the VM is stopped instead of refusing whenever the job is loaded (a boot daemon stays loaded for the whole boot session). It invokes `sudo` only for the privileged `bootout` and plist removal.
- `autostart disable NAME [--scope VALUE]` accepts `--scope` for symmetry with `enable` instead of failing with "unexpected arguments".
- `doctor NAME` now checks the installed launchd autostart plist and the executable it references, so a stale job left by a Homebrew upgrade is reported as a failure with a fix command.

## [0.5.0] - 2026-07-20

### Added

- Optional per-VM loopback monitoring server with Prometheus metrics, cached health/status/info JSON, live guest-agent ping, validated guest IP reporting, and an API reference (`API.md`).
- `MetricsConfig` type with port-conflict validation against VNC and port forwards.
- POWERDOWN and SUSPEND_DISK QMP lifecycle events to the monitoring allowlist.
- Bounded rotating serial log capture (2 MiB, three backups) via a supervisor-controlled FIFO pipe with security hardening, plus the `qemu-manage log NAME` command to print the active serial log to stdout.

### Changed

- Extracted supervisor run logic, control server, and monitoring server into separate files (`run.go`, `control_server.go`, `monitoring_server.go`).
- Introduced newline-framed reader (`framedReader`) for protocol decoding and split `Request`/`Response` message types.
- Added `StopProgress` type with non-terminal progress frames (`acknowledged`, `forcing`) to the stop lifecycle; CLI progress now uses "complete" instead of "done", and `stop` reports the authenticated shutdown acknowledgment, wait duration, and graceful or forced completion path.

### Fixed

- Empty-device BLOCK_IO_ERROR events from QEMU are now correctly recorded instead of being silently dropped.
## [0.4.0] - 2026-07-19

### Added

- Cloud-init NoCloud seed ISO provisioning via `--cloud-init-user-data` on the `create` command.
- `--mac` flag for optional MAC address override with validation on the `create` command.
- `$schema` annotation in canonical JSON config output with editor-side JSON Schema validation.
- Formatted CLI output with go-pretty tables for `autostart`, `info`, `list`, and `--version`.
- Progress bar integration for image download, file copy, and VM stop operations.
- `ConnectWithSetup` console API for setup callback before terminal streaming.
- `OnReady` callback to supervisor `StartOptions` and `Debug` flag to `SuperviseOptions`.

### Changed

- Auto-generate MAC via `crypto/rand` when `--mac` is omitted during VM creation.
## [0.3.0] - 2026-07-19

### Added

- SMB host-folder sharing for user-mode networking, including `smbd` discovery, diagnostics, and guest mount guidance.
- Launchd-based `socket_vmnet` bridge provisioning with shared and bridged interface support.
- Detailed autostart status reporting for launchd plist and loaded-state drift.

### Changed

- Defaulted `socket_vmnet` networking to the shared interface.
- Made autostart enable install or reconcile launchd jobs without starting the VM.
- Made autostart disable require the VM to be stopped instead of booting out a loaded job.

### Fixed

- Made launchd plist updates transactional so failed autostart changes roll back instead of leaving partial state.
- Prevented autostart management from unexpectedly starting, stopping, or restarting VMs.

## [0.2.0] - 2026-07-19

### Added

- Image import, installer boot, additional drive, and USB passthrough options for VM creation.
- Interactive serial console and QEMU monitor access, one-shot monitor commands, guest-agent requests, and VNC launching.
- Per-VM supervised startup with authenticated control sockets and runtime metadata.
- `qemu-manage --version` output with release, VCS, toolchain, and repository information.

### Changed

- Expanded command help and README guidance for the complete VM management workflow.
- Hardened configuration, managed storage, runtime path, metadata, process ownership, and supervisor validation.

## [0.1.0] - 2026-07-18

- Initial release.

[Unreleased]: https://github.com/bradsjm/qemu-manage/compare/v0.6.1...HEAD
[0.6.1]: https://github.com/bradsjm/qemu-manage/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/bradsjm/qemu-manage/compare/v0.5.0...v0.6.0

[0.5.0]: https://github.com/bradsjm/qemu-manage/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/bradsjm/qemu-manage/compare/v0.3.0...v0.4.0

[0.3.0]: https://github.com/bradsjm/qemu-manage/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/bradsjm/qemu-manage/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/bradsjm/qemu-manage/releases/tag/v0.1.0
