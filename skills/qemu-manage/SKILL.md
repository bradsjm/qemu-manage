---
name: qemu-manage
description: Operate qemu-manage. Use to create, configure, start, stop, restart, inspect, access, or delete QEMU VMs on macOS; manage VM autostart; troubleshoot executable-visible behavior; retrieve VM health, status, guest addresses, process statistics, block statistics, or Prometheus metrics from /metrics, /health, /status, /ping, or /info.
---

# qemu-manage

## Scope

Operate headless native AArch64 QEMU virtual machines on Apple Silicon macOS through `qemu-manage`.

Do not edit managed files directly, invoke the internal `supervise` command, or invent unsupported QEMU arguments. Prefer commands that expose stable JSON for automation.

## Workflow

1. Check whether the executable exists with `command -v qemu-manage`.
2. If absent, install it with Homebrew:

   ```sh
   brew install bradsjm/tap/qemu-manage
   ```

   The formula installs QEMU. Install `socket_vmnet` or `samba` separately only when the requested VM needs those optional features.

3. Verify the host before changes:

   ```sh
   qemu-manage --version
   qemu-manage doctor
   ```

4. Select the task:
   - For creating, configuring, lifecycle, access, autostart, deletion, or troubleshooting, read [references/cli.md](references/cli.md).
   - For statistics, health, guest addresses, Prometheus, or HTTP behavior, read [references/monitoring-api.md](references/monitoring-api.md).
   - Read both when creating a VM that must expose monitoring.
5. Run `qemu-manage COMMAND --help` before using options if the installed release may differ from the reference.
6. After creation or configuration, run `qemu-manage doctor NAME`.
7. After lifecycle operations, verify with `qemu-manage status NAME`; add `--json` when consuming output programmatically.

## Operational rules

- Put the VM `NAME` before command options.
- Use only AArch64 guest images on Apple Silicon macOS 13+.
- Start with user-mode networking unless shared/bridged host networking is explicitly required.
- Enable monitoring with `--metrics-port PORT`, then restart after changing the port.
- Treat `stop --force` and `restart --force` as data-corruption risks.
- Before deletion, stop the VM and disable autostart. Explain exactly which managed artifacts deletion removes.
- Use `/health` for cached health policy, `/metrics` for Prometheus, `/status` for detailed cached statistics and guest addresses, `/ping` for an explicit live guest-agent probe, and `/info` for static identity/capabilities.
- Never expose the unauthenticated loopback monitoring listener through a tunnel or port forward without external authentication and transport security.
- Distinguish transport success from VM health: `/status` can return HTTP `200` while collectors report failed or stale data.
- Call out destructive or restart-requiring effects.
- When diagnosing, request or consume executable output such as `doctor --json`, `status --json`, `log`, or endpoint JSON rather than assuming internal state.
