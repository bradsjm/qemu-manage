---
title: Networking
---

qemu-manage supports QEMU user networking and `socket_vmnet` in shared or bridged mode.

## User mode

User mode is the default. It provides outbound connectivity without a privileged networking service, but the guest is not directly reachable from the host unless you configure forwards. It is also the only mode that supports `--share`.

Port forwards are repeatable and bind an explicit IPv4 address:

```sh
qemu-manage create web \
  --network user \
  --forward tcp:127.0.0.1:2222:22 \
  --forward tcp:127.0.0.1:8080:80
```

The format is `proto:IPv4:host-port:guest-port`. You can also update forwards later:

```sh
qemu-manage set web \
  --network user \
  --forward tcp:127.0.0.1:8123:8123
```

## `socket_vmnet` shared

Shared mode allows the host or other VMs to reach the guest without maintaining QEMU port forwards. The guest does not appear directly on the physical LAN, and QEMU still runs unprivileged.

```sh
brew install socket_vmnet
sudo "$(brew --prefix)/bin/brew" services start socket_vmnet

qemu-manage create lab \
  --image "$HOME/Images/lab.qcow2" \
  --network socket_vmnet \
  --socket-vmnet-interface shared
```

When selecting `socket_vmnet`, qemu-manage checks these environment variables before trying independent Homebrew or MacPorts discovery:

| Variable | Purpose |
| --- | --- |
| `QEMU_MANAGE_SOCKET_VMNET_CLIENT` | Absolute path to the root-owned `socket_vmnet_client` executable |
| `QEMU_MANAGE_SOCKET_VMNET_SOCKET` | Absolute path to the `socket_vmnet` daemon socket |

For example:

```sh
export QEMU_MANAGE_SOCKET_VMNET_CLIENT=/opt/socket_vmnet/bin/socket_vmnet_client
export QEMU_MANAGE_SOCKET_VMNET_SOCKET=/opt/homebrew/var/run/socket_vmnet
qemu-manage create lab \
  --network socket_vmnet \
  --socket-vmnet-interface shared
```

Resolved absolute client and socket paths are persisted in the VM configuration for later manual starts and launchd autostart.

## `socket_vmnet` bridged

Bridged mode makes the guest a separate machine on the physical LAN, including receiving an address from that network and participating in LAN discovery. Install the Homebrew package, then name a host interface such as `en0`:

```sh
brew install socket_vmnet

qemu-manage create home-assistant \
  --image "$HOME/Images/haos_generic-aarch64.qcow2" \
  --network socket_vmnet \
  --socket-vmnet-interface en0
```

For a non-`shared` interface, qemu-manage requests `sudo`, copies the Homebrew daemon and client into root-owned `/opt/socket_vmnet/bin`, installs and starts a persistent bridged LaunchDaemon for that interface, waits for its Unix socket, and stores the paths in the VM configuration.

Later manual starts and login- or boot-scope autostart use the persisted client and `/var/run/socket_vmnet.bridged.en0` socket without `sudo` or environment variables. QEMU and the VM supervisor run as the VM owner; only the separate networking daemon runs as root.

One bridged daemon is shared by all qemu-manage VMs using the same host interface. It remains installed when an individual VM is deleted. A bridged VM's launchd job waits for the daemon socket during boot.
