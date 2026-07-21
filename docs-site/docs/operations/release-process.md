---
title: Release Process
---

Releases are created from strict semantic-version tags and published by the repository's Release GitHub Actions workflow. The Homebrew tap update remains a separate manual step after the GitHub release exists.

## 1. Prepare the repository

Before tagging:

1. Update `CHANGELOG.md`.
2. Bump every versioned installation example in `README.md`.
3. Run the normal project checks. The release workflow itself runs `go test -race ./...` and `go vet ./...` before packaging.

## 2. Push a release tag

Use a tag in exactly this form:

```text
vMAJOR.MINOR.PATCH
```

For example:

```sh
git tag v0.7.0
git push origin v0.7.0
```

The workflow rejects prerelease suffixes, build metadata, leading-zero components, or any tag that does not match `vMAJOR.MINOR.PATCH`.

## 3. GitHub Actions builds and publishes

A matching tag triggers `.github/workflows/release.yml`. On an Ubuntu runner, the workflow:

1. checks out the repository and installs the Go version declared by `go.mod`;
2. runs the race-enabled test suite and `go vet`;
3. cross-compiles a static `darwin/arm64` binary with `CGO_ENABLED=0`, embedding the tag's version without the leading `v`;
4. packages `qemu-manage`, `LICENSE`, and `README.md` as `qemu-manage_<version>_darwin_arm64.tar.gz`;
5. generates `checksums.txt` containing the archive's SHA-256 checksum; and
6. creates the GitHub release from the verified tag, uploads the archive and checksum file, and generates release notes.

Wait until both the workflow and the published GitHub release succeed. Do not update downstream packaging to reference an unpublished archive, and do not invent or precompute the release checksum.

## 4. Update the Homebrew tap manually

After the release assets are public, update:

```text
bradsjm/homebrew-tap/Formula/qemu-manage.rb
```

Change the formula's:

- `version` to the semantic version without `v`;
- release `url`, preserving the leading `v` in the GitHub release directory but omitting it from the archive filename; and
- `sha256`, copied verbatim from the published `checksums.txt`.

Validate the tap checkout before committing:

```sh
brew style bradsjm/tap/qemu-manage
brew audit --strict --online bradsjm/tap/qemu-manage
brew reinstall --build-from-source --verbose bradsjm/tap/qemu-manage
brew test bradsjm/tap/qemu-manage
qemu-manage --version
```

Commit and push the formula update only after those checks pass. The release is not complete until the tap is updated.

The tap update is intentionally manual. Do not add cross-repository credentials or modify `bradsjm/homebrew-tap` from the release workflow.
