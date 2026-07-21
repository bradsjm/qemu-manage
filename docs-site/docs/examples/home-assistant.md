---
title: Home Assistant OS on a Mac
---

This example installs Home Assistant OS (HAOS) as a headless AArch64 VM on an Apple Silicon Mac. It uses the generic AArch64 HAOS image, QEMU with HVF acceleration, and `socket_vmnet` when the guest must appear as a separate device on the LAN.

The reference deployment for this guide runs on a Mac Mini with:

- 2 virtual CPUs, 5 GiB of memory, and a 32 GiB managed qcow2 disk
- HAOS 18.0 (`generic-aarch64`)
- `socket_vmnet` bridged to `vlan0`
- macOS VLAN `IOT`, tag 20, with parent interface `en0`
- guest agent enabled, restart policy `on-failure`, and monitoring on host loopback port 8120
- boot-scope autostart, while the supervisor and QEMU continue to run as the owning non-root user

Use the same structure with interface names, VLAN tags, and VM sizing appropriate for your Mac and network.

## 1. Prepare the Mac

The Homebrew formula installs QEMU as a dependency. Install qemu-manage and the optional `socket_vmnet` package, then check the host:

```sh
brew install bradsjm/tap/qemu-manage socket_vmnet
qemu-manage doctor
```

Do not start the Homebrew shared-mode `socket_vmnet` service for the bridged examples below. When a VM selects a physical or VLAN interface, qemu-manage installs its own persistent bridged LaunchDaemon and socket for that interface.

The first bridged configuration prompts for `sudo`. qemu-manage uses it to install root-owned `socket_vmnet` binaries and the networking LaunchDaemon. It does **not** run QEMU or the VM supervisor as root.

## 2. Choose the bridge interface

A bridged guest requests an address from the network attached to the selected host interface. Choose exactly one of the following scenarios before creating the VM.

### Scenario A: bridge to wired Ethernet

List the Mac's hardware ports instead of assuming that Ethernet is always `en0`:

```sh
networksetup -listallhardwareports
```

Find the device under the active Ethernet hardware port. If it is `en0`, use:

```sh
BRIDGE_INTERFACE=en0
```

The Ethernet switch port and LAN must provide DHCP unless you plan to configure a static address inside HAOS. The guest receives its own MAC and IP address; it does not share the Mac's address.

### Scenario B: bridge to Wi-Fi

Use `networksetup -listallhardwareports` to find the device under the `Wi-Fi` hardware port. The device might be `en0` or `en1`, depending on the Mac:

```sh
networksetup -listallhardwareports
BRIDGE_INTERFACE=en1  # replace with the Wi-Fi device shown above
```

Bridging depends on the Wi-Fi network and access point accepting the virtual guest. Client isolation, enterprise authentication, captive portals, or network policy may prevent DHCP, LAN access, or multicast discovery. If `homeassistant.local` does not resolve, use the guest IP address. Prefer wired Ethernet for an always-on Home Assistant host when possible.

### Scenario C: bridge to an 802.1Q VLAN

This matches the Mac Mini reference deployment. Its physical Ethernet device is `en0`, the VLAN is named `IOT`, and the VLAN tag is 20.

First confirm that the parent device supports VLANs:

```sh
networksetup -listdevicesthatsupportVLAN
```

Create the VLAN network service:

```sh
sudo networksetup -createVLAN IOT en0 20
networksetup -listVLANs
```

macOS assigns a device such as `vlan0`. Use the device reported by `networksetup -listVLANs`, not the user-defined name `IOT`:

```sh
ifconfig vlan0
BRIDGE_INTERFACE=vlan0
```

The upstream switch port connected to the Mac must carry VLAN 20 as a tagged VLAN, and that VLAN must provide DHCP, routing, and DNS as required by HAOS. Replace `IOT`, `en0`, and `20` with your own network design. Creating or deleting a macOS VLAN changes host networking and requires administrator privileges.

To remove this example VLAN later, first move or stop every VM using it, then run:

```sh
sudo networksetup -deleteVLAN IOT en0 20
```

## 3. Create the HAOS VM

The repository pins the smoke-tested HAOS 18.0 generic AArch64 image. qemu-manage downloads the `.xz` archive, decompresses it, imports it into the managed qcow2 disk, and expands that disk to 32 GiB:

```sh
qemu-manage create home-assistant \
  --image "https://github.com/home-assistant/operating-system/releases/download/18.0/haos_generic-aarch64-18.0.qcow2.xz" \
  --cpus 2 \
  --memory 5GiB \
  --disk-size 32GiB \
  --network socket_vmnet \
  --socket-vmnet-interface "$BRIDGE_INTERFACE" \
  --guest-agent on \
  --restart-policy on-failure \
  --shutdown-timeout 180s \
  --rtc-base utc \
  --metrics-port 8120
```

The selected bridge interface can be the wired device, Wi-Fi device, or VLAN device from the previous section. qemu-manage generates a stable, locally administered MAC address and persists the resolved `socket_vmnet_client` and daemon-socket paths in the VM configuration.

Port 8120 is the qemu-manage monitoring API on the Mac's loopback interface; it is unrelated to Home Assistant's guest web port 8123. Choose another unused host port or omit `--metrics-port` if monitoring is not needed.

Before first boot, inspect the configuration and rendered command:

```sh
qemu-manage config show home-assistant
qemu-manage doctor home-assistant
qemu-manage showcmd home-assistant
```

For a bridged interface, the first create provisions a root-owned daemon socket such as `/var/run/socket_vmnet.bridged.en0` or `/var/run/socket_vmnet.bridged.vlan0`. The daemon remains installed independently of the VM so other VMs can share the same bridge.

## 4. Start HAOS and confirm networking

Start the VM and follow the serial output:

```sh
qemu-manage start home-assistant
qemu-manage status home-assistant
qemu-manage console home-assistant
```

### Inspect the VM with `info`

Use `info` for the detailed configuration and authenticated runtime view:

```sh
qemu-manage info home-assistant
qemu-manage info home-assistant --json
```

For a running or paused VM, `info` also reads the loopback monitoring endpoints configured on port 8120. It validates the VM identity, backend PID, and start time before displaying monitoring data. If monitoring is disabled, unavailable, or belongs to a different run, the command reports that condition and falls back to durable configuration plus authenticated supervisor state.

Use `status` for a compact state check and `info` when diagnosing resources, restart requirements, QEMU details, interfaces, filesystems, or monitoring collectors.

### Read the serial log

`log` prints the active bounded serial log verbatim without opening an interactive session:

```sh
qemu-manage log home-assistant
qemu-manage log home-assistant > home-assistant-serial.log
```

Use it to review HAOS boot messages, interface discovery, shutdown output, and errors after the fact. It is the guest serial log, not the Home Assistant Core application log shown in the Home Assistant UI.

### Connect to the HAOS console

`console` attaches the current terminal to the running VM's serial port:

```sh
qemu-manage console home-assistant
```

Press Enter if the HAOS prompt is not visible. Run Home Assistant CLI commands such as:

```text
ha core info
ha supervisor info
ha network info
```

Press `Ctrl-]` to disconnect from the serial console without shutting down or pausing the VM.

HAOS normally enables its first Ethernet-style interface through DHCP. If the console shows an interface but it remains disabled, enter the HAOS CLI and inspect it:

```text
ha network info
```

Then enable the interface reported by HAOS. In the Mac Mini deployment it is `enp0s4`:

```text
ha network update enp0s4 --ipv4-method auto
```

Use the actual interface name from `ha network info`. The guest interface name is not the same as the macOS bridge interface.

Find the assigned address in the HAOS console or your router/DHCP server. Because the VM has its own MAC address, a DHCP reservation is the preferred way to keep its address stable.

## 5. Complete Home Assistant onboarding

HAOS performs additional preparation on its first boot. From a browser on a network that can reach the guest, open:

```text
http://homeassistant.local:8123/
```

If multicast DNS does not cross the selected VLAN or Wi-Fi network, use the DHCP address instead:

```text
http://GUEST_IP:8123/
```

Wait for preparation to finish, then create the Home Assistant owner account, set the home location and regional preferences, and complete the [Home Assistant onboarding flow](https://www.home-assistant.io/getting-started/onboarding/). Do not expose port 8123 directly to the internet; use Home Assistant's supported remote-access options or a properly secured VPN/reverse proxy.

## 6. Configure autostart

Choose one launchd scope. In both cases, invoke qemu-manage as the VM-owning user, **not** with `sudo`.

### Start when the user logs in

A login-scope LaunchAgent needs no privileged installation, but Home Assistant remains offline until that user logs in:

```sh
qemu-manage autostart enable home-assistant --scope login --start
qemu-manage autostart status home-assistant
```

### Start at system boot

A boot-scope LaunchDaemon is appropriate for an unattended Mac Mini server. qemu-manage requests `sudo` only for the narrow system launchd installation and management operations. The generated job starts the VM under its non-root owner account:

```sh
qemu-manage autostart enable home-assistant --scope boot --start
qemu-manage autostart status home-assistant
```

After enabling either scope, ordinary `qemu-manage start home-assistant` requests go through the installed launchd job. The `on-failure` restart policy allows launchd to restart the supervisor after an unsuccessful exit.

To change scopes, stop the VM, disable the existing job, and enable the new scope:

```sh
qemu-manage stop home-assistant
qemu-manage autostart disable home-assistant
qemu-manage autostart enable home-assistant --scope boot --start
```

Never enable both scopes for the same VM.

## 7. Operate and verify the installation

Useful checks after onboarding or a host reboot are:

```sh
qemu-manage list
qemu-manage info home-assistant
qemu-manage doctor home-assistant
qemu-manage autostart status home-assistant
curl http://127.0.0.1:8120/health
```

Configuration changes that affect QEMU require a restart:

```sh
qemu-manage restart home-assistant
```

Stop HAOS gracefully before host maintenance:

```sh
qemu-manage stop home-assistant
```

The supervisor first requests a guest shutdown and waits up to the configured 180 seconds before its force-stop path is available.

## Troubleshooting

### The guest receives no address

1. Run `qemu-manage doctor home-assistant` and inspect `qemu-manage log home-assistant`.
2. Verify that `$BRIDGE_INTERFACE` is up with `ifconfig`.
3. For Wi-Fi, check access-point client isolation and whether the network permits bridged virtual clients.
4. For a VLAN, confirm that the switch port carries the tag and that the VLAN has DHCP.
5. In the HAOS console, run `ha network info` and enable the detected interface if necessary.
6. Check the router's DHCP leases for the MAC shown by `qemu-manage config show home-assistant`.

### Home Assistant is running but cannot be opened

Try the guest IP instead of `homeassistant.local`, especially across VLANs. Confirm that routing and firewall rules allow the client network to reach guest TCP port 8123.

### Autostart configuration has drifted

Re-enable the intended scope to reconcile the plist, then re-run the checks:

```sh
qemu-manage autostart enable home-assistant --scope boot
qemu-manage autostart status home-assistant
qemu-manage doctor home-assistant
```

Do not run the whole command with `sudo`; qemu-manage performs only the required privileged sub-operations.