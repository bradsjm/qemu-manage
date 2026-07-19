# Changelog

All notable changes to qemu-manage are documented in this file.

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

[0.2.0]: https://github.com/bradsjm/qemu-manage/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/bradsjm/qemu-manage/releases/tag/v0.1.0
