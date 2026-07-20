# Security Policy

## Supported versions

This project is maintained for personal use and does not currently publish multiple supported release branches. Security fixes target the latest source and latest published release, when releases exist.

## Reporting a vulnerability

Do not disclose a suspected vulnerability in a public issue.

Use GitHub's **Report a vulnerability** action on the repository's Security page to submit a private report. If private vulnerability reporting is unavailable, open a public issue containing no vulnerability details and ask the maintainer to establish a private contact channel.

Include, when available:

- The affected version or commit
- The macOS and QEMU versions
- Reproduction steps or a minimal proof of concept
- The expected and observed security boundary
- The potential impact
- Any suggested remediation

Never include VM images, guest credentials, private keys, tokens, or sensitive log contents.

## Security-sensitive areas

Reports are especially useful for behavior that could:

- Run QEMU or user-controlled commands as root
- Replace or load an unintended launchd job
- Expose QMP, QGA, or serial sockets to another user
- Signal a process not owned by the active VM supervisor
- Escape manager-owned QEMU arguments
- Follow hostile symlinks or overwrite files outside managed storage
- Make bridged networking silently fall back to a less restrictive mode
- Expose the unauthenticated per-VM loopback monitoring endpoint beyond intended local callers
- Expose guest IP addresses from the monitoring `/status` route beyond intended local callers

The absence of a response is not permission to disclose secrets or exploit third-party systems.
