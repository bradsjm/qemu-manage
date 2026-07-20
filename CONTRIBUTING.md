# Contributing

Contributions should keep `qemu-manage` small, predictable, and focused on headless QEMU virtual machines on Apple Silicon macOS.

## Before changing code

1. Check existing issues and command help for the current contract.
2. Keep changes narrowly scoped; avoid adding a daemon, database, shell command execution, or dependencies when the standard library is sufficient.
3. Preserve the security boundaries around unprivileged QEMU execution, private runtime sockets, launchd installation, and manager-owned QEMU arguments.

For significant behavior or command-line changes, open an issue describing the user-visible problem and proposed contract before implementation.

## Local workflow

Use Go 1.25 or newer. Format and verify changes with:

```sh
go fmt ./...
go test -race ./...
go vet ./...
go build -o qemu-manage ./cmd/qemu-manage
```

The final runtime checks require an Apple Silicon Mac with Homebrew QEMU installed:

```sh
brew install qemu
./qemu-manage doctor
```

Changes to QEMU rendering, lifecycle handling, `socket_vmnet`, or launchd integration should also exercise the affected path with a disposable AArch64 VM. Do not perform destructive VM or reboot tests against production workloads.

## Tests

- Test observable behavior and stable contracts rather than implementation details.
- Keep tests deterministic; inject clocks and process dependencies instead of sleeping.
- Add regression coverage for bug fixes at the lowest shared owner of the behavior.
- Keep Darwin-specific behavior behind platform files while preserving compilation of unsupported-platform stubs.

## Documentation

Update command help and the README when user-visible commands, prerequisites, defaults, storage paths, or security behavior change. Update `API.md` when monitoring behavior changes, `schema.json` when durable configuration shape or constraints change, and `SECURITY.md` when trust boundaries change. Examples must use supported syntax and should be executable as written once their named input files exist.

## Licensing contributions

By submitting a contribution, you agree that it is licensed under the repository's [Apache License 2.0](LICENSE), as described by section 5 of that license.
