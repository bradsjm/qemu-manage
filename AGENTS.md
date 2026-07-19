# Repository Guidelines

## Project Overview

`qemu-manage` is a small Go CLI for creating and operating headless AArch64 QEMU VMs on Apple Silicon macOS. It manages strict JSON configuration, VM lifecycle, serial consoles, networking, and per-VM launchd autostart. Keep it a single binary without a central daemon, database, shell execution, or unnecessary dependencies.

The primary target is macOS 13+ with QEMU/HVF. User-mode networking is the default; `socket_vmnet` is the optional shared/bridged path. QEMU must remain unprivileged even when boot-scope launchd installation requires narrow `sudo` operations.

## Architecture & Data Flow

The dependency flow is deliberately one-way:

```text
cmd/qemu-manage -> internal/cli
internal/cli -> model, store, backend/qemu, supervisor, lifecycle,
                launchd, console
```

- `cmd/qemu-manage/main.go` creates `cli.NewApp()`, passes process I/O and arguments to `App.Run`, and exits with its status code.
- `internal/cli` parses commands and wires collaborators. `App` exposes dependency-injection seams for external commands, HTTP, terminal checks, firmware/network discovery, runtime, lifecycle, and launchd.
- Durable VM configuration is strict, versioned JSON under `~/Library/Application Support/qemu-manage/vms`. `internal/model` owns validation and canonical hashing; `internal/store` owns secure paths, atomic writes, and flock-based locks.
- `start` re-execs the same binary in hidden `supervise` mode. One supervisor owns one QEMU child, holds the immutable-ID lifetime lock, writes runtime metadata, and serves an authenticated Unix control socket. There is no shared service process.
- CLI lifecycle requests flow through `internal/lifecycle` to `internal/supervisor`. If control is unavailable, status is derived conservatively from the lifetime lock plus `runtime.json` and `last_exit.json`.
- `internal/backend` defines backend/instance interfaces; `internal/qemu` is the concrete backend. QMP provides readiness, status, and power control; QGA is optional.
- Configuration mutations take a stable per-name lock. Running-state ownership uses immutable-ID lifetime locks. Preserve this distinction to prevent delete/recreate races.
- Autostart renders per-VM LaunchAgent/LaunchDaemon plists and uses the same foreground supervisor path as manual starts.

## Key Directories

- `cmd/qemu-manage/` — sole executable entry point.
- `internal/cli/` — command parsing, application wiring, create/config/runtime/autostart workflows, and user-facing help.
- `internal/model/` — durable schema, validation, IDs, network/restart/autostart enums, and canonical configuration hashing.
- `internal/store/` — managed filesystem layout, atomic saves, artifact creation/deletion, name locks, and lifetime locks.
- `internal/backend/` — backend-neutral contracts and registry.
- `internal/qemu/` — deterministic argv rendering, QMP/QGA clients, process ownership, readiness, and host diagnostics.
- `internal/supervisor/` — per-VM state machine, metadata, peer-authenticated control protocol, signals, and detached process startup.
- `internal/lifecycle/` — status derivation and stop coordination.
- `internal/launchd/` — plist rendering and transactional login/boot job management.
- `internal/console/` — raw-terminal serial-console proxy and `Ctrl-]` disconnect handling.

## Development Commands

Use standard Go tooling; the repository has no Makefile, task runner, scripts directory, or CI workflow.

```sh
# Format source
go fmt ./...

# Run the race-enabled suite
go test -race ./...

# Static analysis
go vet ./...

# Build the only executable
go build -o qemu-manage ./cmd/qemu-manage

# Inspect host prerequisites on the target Mac
brew install qemu
./qemu-manage doctor
```

Use `go mod tidy` only when dependency imports change, and keep `go.mod` and `go.sum` synchronized. For command discovery, run `./qemu-manage --help` or `./qemu-manage COMMAND --help`.

## Code Conventions & Common Patterns

- Follow standard Go formatting and naming. Export protocol/model contracts; keep implementation helpers unexported.
- Prefer the standard library. Current direct dependencies are limited to `golang.org/x/sys` and `golang.org/x/term`; justify additions against existing platform capabilities.
- Wrap errors with actionable package context using `%w` where callers need the cause. CLI failures use stable prefixes such as `config:`, `qemu:`, `runtime:`, `launchd:`, and `socket_vmnet:`. Usage errors exit 2; operational/validation failures exit 1.
- Pass `context.Context` through blocking process, socket, and lifecycle operations. Do not add detached goroutines without explicit ownership and cleanup.
- Concurrency is localized: supervisors serialize lifecycle transitions with mutexes/channels; QEMU instances own child reaping and force-stop idempotence; QMP serializes requests so cancellation cannot corrupt the stream.
- Use interfaces and injected function fields instead of global state or mocking frameworks. Existing seams include `backend.Backend`/`Instance`, `launchd.Runner`, `cli.RuntimeService`, clocks, external command runners, HTTP clients, and discovery functions.
- Persist desired configuration only. Runtime state belongs to authenticated supervisor observations and ephemeral metadata, not `config.json`.
- Use atomic writes, owner-only modes, `O_NOFOLLOW`, ownership checks, and stable locks for managed files. Never weaken socket permissions, peer-UID checks, symlink rejection, or process ownership validation.
- Render external commands as argv slices; never invoke a shell. Keep manager-owned QEMU lifecycle, device, and control arguments protected from user passthrough.
- Keep Darwin behavior in build-tagged files with unsupported-platform companions. Do not make another platform compile by silently weakening macOS security behavior.
- Update `internal/cli/help.go` and `README.md` together when commands, defaults, prerequisites, storage paths, or security behavior change.

## Important Files

- `go.mod` — module `qemu-manage`, Go `1.25.0`, and dependency source of truth.
- `cmd/qemu-manage/main.go` — executable entry point.
- `internal/cli/app.go` — command dispatch, dependency wiring, root refusal, and exit-code mapping.
- `internal/cli/help.go` — canonical user-facing command documentation.
- `internal/model/config.go` — durable configuration contract and validation boundary.
- `internal/store/store.go`, `internal/store/lock.go` — secure storage and lock ownership.
- `internal/backend/backend.go` — backend and running-instance interfaces.
- `internal/qemu/command.go` — deterministic QEMU/socket_vmnet command boundary.
- `internal/qemu/instance.go`, `qmp.go`, `qga.go` — child lifecycle and control protocols.
- `internal/supervisor/supervisor.go`, `protocol.go`, `metadata.go` — per-VM runtime owner and wire/state contracts.
- `internal/launchd/plist.go`, `manage.go` — plist contract and launchd integration.
- `README.md`, `CONTRIBUTING.md`, `SECURITY.md` — supported environment, contribution checks, and non-negotiable security boundaries.

## Runtime/Tooling Preferences

- Required development runtime: Go 1.25 or newer; `go.mod` declares `go 1.25.0`.
- Production target: Apple Silicon, macOS 13+, native AArch64 guests, Homebrew QEMU, and HVF acceleration. Do not silently fall back to TCG or cross-architecture emulation.
- Optional networking dependency: separately installed `socket_vmnet`. Never install it implicitly, run QEMU as root, or silently replace it with user NAT.
- The module uses Go modules directly; there is no vendored tree or alternate package manager.
- Build tags isolate Darwin, Unix/Linux, and unsupported implementations. Preserve paired files such as `runner_darwin.go`/`runner_unsupported.go` and `peercred_darwin.go`/`peercred_unsupported.go`.
- Ignored outputs include `/qemu-manage`, `/bin`, `/dist`, coverage/profile files, and local `go.work` files. Do not ignore `go.sum`.
- The repository is Apache-2.0 licensed; contributions are accepted under section 5 of that license.

## Testing & QA

Tests use only the standard `testing` package and remain in-package to exercise internal contracts. Common patterns are table-driven cases with `t.Run`, `test*`/`require*`/`assert*` helpers calling `t.Helper`, and lightweight fakes implementing injected interfaces.

- Canonical suite: `go test -race ./...`; follow with `go vet ./...` and a full build.
- Keep tests deterministic. Inject clocks and process dependencies; use channels for synchronization instead of `time.Sleep` or real timers.
- Use `t.TempDir` for ordinary files. For Unix sockets, prefer short `os.MkdirTemp(os.TempDir(), "qm-...")` paths to stay under macOS socket-path limits, and register cleanup.
- Assert observable contracts: strict JSON, state transitions, locking, file modes/ownership, symlink rejection, protocol framing, QEMU argv arrays, privilege boundaries, and error behavior.
- Add bug regressions at the lowest shared owner. Do not weaken valid security assertions to make tests pass.
- Platform tests generally run everywhere and skip only where OS semantics genuinely differ. Production platform code belongs behind build tags with compilable unsupported stubs.
- Automated tests fake QEMU, launchctl, and sudo. Changes to real process execution, QEMU rendering/lifecycle, launchd, console handling, or `socket_vmnet` also require a disposable AArch64 VM smoke test on Apple Silicon; never use a production VM for destructive checks.
- Use `https://github.com/home-assistant/operating-system/releases/download/18.0/haos_generic-aarch64-18.0.qcow2.xz` as the smoke testing image.
