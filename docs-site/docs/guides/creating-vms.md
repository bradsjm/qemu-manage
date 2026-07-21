---
title: Creating VMs
---

Create a managed AArch64 VM with:

```sh
qemu-manage create NAME [OPTIONS]
```

`NAME` must appear before the options. With neither `--image` nor `--iso`, the command creates a blank 32 GiB qcow2 primary disk. New VMs default to two CPUs, 2 GiB of memory, user-mode networking, guest agent off, RTC base `utc`, restart policy `never`, and a generated locally administered unicast MAC.

## Choose an installation source

### Import an HTTP(S) image

Pass a qcow2 or raw URL to `--image`. URL paths ending in `.xz` or `.gz` are decompressed while downloading and converted to the managed qcow2 disk.

```sh
qemu-manage create home-assistant \
  --image "https://github.com/home-assistant/operating-system/releases/download/18.0/haos_generic-aarch64-18.0.qcow2.xz" \
  --cpus 2 --memory 4GiB --disk-size 32GiB \
  --network user --forward tcp:127.0.0.1:2222:22 \
  --guest-agent on --rtc-base utc \
  --restart-policy on-failure
```

### Import a local image

Pass a local qcow2 or raw path to `--image`. qemu-manage copies and converts the source; it never modifies the original.

```sh
qemu-manage create appliance \
  --image "$HOME/Downloads/appliance-aarch64.qcow2" \
  --mac 02:12:34:56:78:9a \
  --cpus 2 --memory 4GiB --disk-size 32GiB
```

### Provision with cloud-init

`--cloud-init-user-data PATH` requires a cloud-init-compatible image with the NoCloud datasource. qemu-manage uses `/usr/bin/hdiutil` to create a managed, read-only ISO labelled `CIDATA`, copies `user-data` into it, and generates `meta-data` whose `instance-id` is the VM UUID. The seed stays attached across boots and is removed with the VM; the source user-data file need not remain after creation.

```sh
qemu-manage create cloud-vm \
  --image "$HOME/Downloads/cloud-aarch64.qcow2" \
  --cloud-init-user-data "$HOME/Downloads/user-data" \
  --cpus 2 --memory 4GiB
```

Cloud-init normally applies per-instance configuration only on first boot. Because guest root can read secrets on the attached seed, protect both the source file and guest.

### Install from an ISO

`--iso PATH` copies a local ARM64 installer ISO into managed storage, creates the qcow2 disk, and boots ISO-first. VNC is useful for graphical installers.

```sh
qemu-manage create linux \
  --iso "$HOME/Downloads/linux-arm64.iso" \
  --cpus 4 --memory 4GiB --disk-size 64GiB \
  --vnc --vnc-password "$VNC_PASSWORD" \
  --keyboard-layout en-gb
```

### Create a blank disk

Omit both `--image` and `--iso`. The default primary disk is a blank 32 GiB qcow2 image; change its size with `--disk-size`.

## Create options

| Area | Options |
| --- | --- |
| Resources | `--cpus N`; `--memory SIZE` using whole MiB or GiB; `--disk-size SIZE` |
| Lifecycle | `--restart-policy never\|on-failure`; `--shutdown-timeout DURATION` (default `180s`); `--rtc-base utc\|localtime`; `--metrics-port PORT` (loopback port 1024–65535, default off) |
| Networking | `--network user\|socket_vmnet`; `--mac MAC`; repeatable `--forward proto:IPv4:host-port:guest-port`; `--share PATH`; `--socket-vmnet-interface shared\|INTERFACE` |
| Display | `--vnc`; `--vnc-password VALUE` (required with VNC, 1–8 UTF-8 bytes); `--vnc-bind IPV4` (default `127.0.0.1`); `--vnc-port PORT` (default 5900); `--vnc-port-to PORT` (default 5999); `--keyboard-layout LAYOUT` (VNC only, default `en-us`) |
| Guest integration | `--guest-agent on\|off` |
| Executables and firmware | `--qemu PATH`; `--qemu-img PATH`; `--firmware-code PATH` and `--firmware-vars PATH` (provide both to override discovery) |
| Devices | Repeatable `--usb`; repeatable `--drive` |

QEMU and qemu-img otherwise resolve from `PATH`; matching AArch64 UEFI code and variables images are auto-detected.

## Extra drives

Append repeatable external disks after the managed primary disk:

```text
--drive file=PATH[,if=virtio][,format=raw|qcow2][,cache=none|writeback|writethrough|directsync|unsafe][,readonly=on|off]
```

Omitted `if` defaults to `virtio`; omitted format is detected from the file header. All disks use QEMU threaded I/O. Relative paths become absolute references. Extra-drive files are referenced in place: qemu-manage does not copy, resize, convert, chmod, or delete them.

```sh
qemu-manage create lab \
  --image "$HOME/Images/lab.qcow2" \
  --drive "file=disk.img,if=virtio,cache=none" \
  --drive "file=archive.qcow2,format=qcow2,readonly=on"
```

## USB passthrough

Repeat either exact selector form:

```text
--usb vendor=VVVV,product=PPPP
--usb bus=N,address=N
```

Vendor/product is generally stable across replugging; bus/address can change. Up to four selections fit without VNC, or two with VNC because QEMU adds a USB keyboard and tablet.

## Share one host folder over SMB

`--share PATH` is create-only and user-network-only. It references one host directory in place and exposes the single fixed share `//10.0.2.4/qemu`. Install the required helper first:

```sh
brew install samba
```

Without `/opt/homebrew/sbin/samba-dot-org-smbd`, creation is refused. Mount the share in a Linux guest with:

```sh
sudo mkdir -p /mnt/share
sudo mount -t cifs //10.0.2.4/qemu /mnt/share -o username=guest
```

`socket_vmnet` VMs and additional SMB folders are not supported.
