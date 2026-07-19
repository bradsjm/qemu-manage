# Changelog

All notable changes to qemu-manage are documented in this file.

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

[0.3.0]: https://github.com/bradsjm/qemu-manage/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/bradsjm/qemu-manage/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/bradsjm/qemu-manage/releases/tag/v0.1.0
