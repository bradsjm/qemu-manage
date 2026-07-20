# Changelog

All notable changes to qemu-manage are documented in this file.


## [Unreleased]

### Added
- Optional per-VM loopback monitoring server with Prometheus metrics, cached
- Optional per-VM loopback monitoring server with Prometheus metrics, cached health/status/info JSON, live guest-agent ping, validated guest IP reporting, and an API reference

### Changed
- CLI progress now uses “complete” instead of “done,” and `stop` reports the
- CLI progress now uses "complete" instead of "done," and `stop` reports the authenticated shutdown acknowledgment, wait duration, and graceful or forced completion path
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

[0.4.0]: https://github.com/bradsjm/qemu-manage/compare/v0.3.0...v0.4.0

[0.3.0]: https://github.com/bradsjm/qemu-manage/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/bradsjm/qemu-manage/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/bradsjm/qemu-manage/releases/tag/v0.1.0
