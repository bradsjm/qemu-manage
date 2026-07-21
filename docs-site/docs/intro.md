---
title: Introduction
sidebar_position: 1
description: What qemu-manage is and where to start.
---

# Introduction

qemu-manage is a single-binary CLI for creating and operating headless AArch64 QEMU virtual machines on Apple Silicon.

It manages the complete VM lifecycle while keeping each machine independently supervised. There is no central daemon, no database, and no shell execution.

## Key features

- Create, start, stop, restart, inspect, and delete virtual machines.
- Import local or HTTP(S) disk images, including compressed images, and provision compatible guests with cloud-init.
- Connect to a guest's serial console and inspect its bounded serial log.
- Work with the QEMU monitor through QMP and communicate with QEMU Guest Agent (QGA).
- Expose per-VM Prometheus metrics and a REST API on localhost.
- Enable password-protected VNC and open it through macOS Screen Sharing.
- Choose built-in QEMU user networking or optional shared and bridged `socket_vmnet` networking.
- Configure per-VM launchd autostart at login or system boot.
- Rely on secure-by-design storage and control primitives: atomic writes, peer-authenticated Unix sockets, and immutable-ID lifetime locks.
- Run AArch64 guests with macOS Hypervisor.framework (HVF) acceleration.

## Where to go next

- [Getting Started](./getting-started.md) — install qemu-manage and run your first VM workflow.
- [CLI Reference](./reference/cli.md) — find commands, options, and environment variables.
- [Architecture](./architecture/overview.md) — understand supervisors, storage, sockets, and VM processes.
