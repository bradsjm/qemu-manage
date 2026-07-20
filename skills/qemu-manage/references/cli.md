# qemu-manage CLI reference

Use only documented executable behavior. Run `qemu-manage COMMAND --help` when the installed version may differ from this reference.

## Platform and installation

`qemu-manage` manages headless native AArch64 QEMU guests on Apple Silicon Macs running macOS 13 or newer. It uses QEMU's HVF acceleration; it does not provide x86 emulation or a non-macOS fallback.

Check installation:

```sh
command -v qemu-manage
qemu-manage --version
```

If missing, install the prebuilt executable and QEMU dependency:

```sh
brew install bradsjm/tap/qemu-manage
qemu-manage --version
qemu-manage doctor
```

Optional components:

```sh
brew install socket_vmnet  # shared or bridged networking only
brew install samba         # --share host-folder export only
```

For shared `socket_vmnet`, start its service:

```sh
sudo "$(brew --prefix)/bin/brew" services start socket_vmnet
```

Use `qemu-manage doctor [NAME] [--json]` to inspect prerequisites without changing anything.

## Global orientation

```sh
qemu-manage --help
qemu-manage COMMAND --help
qemu-manage list [--json]
qemu-manage status [NAME] [--json]
```

Put `NAME` before command options. Prefer `--json` for automation. Default managed VM data is under `~/Library/Application Support/qemu-manage/vms`, but operate through the executable rather than editing managed files.

## Create a VM

General form:

```sh
qemu-manage create NAME [OPTIONS]
```

Choose one source pattern:

```sh
# Blank 32 GiB qcow2 disk
qemu-manage create demo

# Import a local qcow2/raw AArch64 image
qemu-manage create demo --image "$HOME/Images/demo-aarch64.qcow2"

# Download and decompress an .xz or .gz image
qemu-manage create home-assistant \
  --image "https://github.com/home-assistant/operating-system/releases/download/18.0/haos_generic-aarch64-18.0.qcow2.xz" \
  --cpus 2 --memory 4GiB --disk-size 32GiB

# Boot an AArch64 installer with VNC
qemu-manage create linux --iso "$HOME/Downloads/linux-arm64.iso" \
  --vnc --vnc-password "$VNC_PASSWORD" --keyboard-layout en-us

# Provision a cloud image with a persistent NoCloud seed
qemu-manage create cloud-vm --image "$HOME/Images/cloud-aarch64.qcow2" \
  --cloud-init-user-data "$HOME/Images/user-data"
```

Important options:

- Storage: `--image SOURCE`, `--iso PATH`, `--cloud-init-user-data PATH`, `--disk-size SIZE`, repeatable `--drive file=PATH[,if=virtio][,format=raw|qcow2][,cache=...][,aio=threads|native][,readonly=on|off]`.
- Resources: `--cpus N` (default 2), `--memory SIZE` (default 2GiB).
- Lifecycle: `--restart-policy never|on-failure`, `--shutdown-timeout D`, `--rtc-base utc|localtime`.
- Monitoring: `--metrics-port PORT` (1024–65535; off by default).
- User networking: `--network user`, repeatable `--forward proto:IPv4:host-port:guest-port`, and optional `--share PATH`.
- socket_vmnet: `--network socket_vmnet --socket-vmnet-interface shared|INTERFACE`.
- Guest integration: `--guest-agent on|off`.
- Display: `--vnc`, required `--vnc-password VALUE` (1–8 UTF-8 bytes), `--vnc-bind IPV4`, `--vnc-port PORT`, `--vnc-port-to PORT`, `--keyboard-layout LAYOUT`.
- Devices: repeatable `--usb vendor=VVVV,product=PPPP` or `--usb bus=N,address=N`.
- Overrides: `--qemu PATH`, `--qemu-img PATH`, and paired `--firmware-code PATH --firmware-vars PATH`.

Source images and ISOs are copied into managed storage; extra drive files and `--share` directories are referenced in place and must remain readable. A locally administered MAC is generated unless `--mac` is supplied. User networking is the default.

After creation:

```sh
qemu-manage doctor NAME
qemu-manage showcmd NAME
qemu-manage start NAME
qemu-manage status NAME
```

## Configure a VM

Change selected editable values while preserving omitted values:

```sh
qemu-manage set NAME OPTION [OPTION ...]
qemu-manage set demo --cpus 4 --memory 4GiB
qemu-manage set demo --restart-policy on-failure --shutdown-timeout 5m
qemu-manage set demo --network user \
  --clear-forwards --forward tcp:127.0.0.1:2222:22
qemu-manage set demo --guest-agent on --metrics-port 9101
qemu-manage set demo --metrics-port off
```

Supported `set` areas include CPUs, memory, restart policy, shutdown timeout, RTC base, monitoring port, network mode and forwards, socket_vmnet interface, guest agent, VNC settings, and keyboard layout. Restart a running VM when `status` reports `restart_required`.

For complete strict JSON configuration:

```sh
qemu-manage config show NAME > NAME.json
qemu-manage config validate NAME.json
qemu-manage config apply NAME NAME.json
```

`config apply` atomically applies editable settings. Identity, backend, architecture, and autostart scope must still match. Unknown fields, trailing JSON, unsupported schema versions, and invalid values fail validation.

## Lifecycle and access

```sh
qemu-manage start NAME
qemu-manage start NAME --boot-menu
qemu-manage status NAME
qemu-manage stop NAME
qemu-manage stop NAME --timeout 5m
qemu-manage restart NAME
```

Use `--foreground` on `start` or `restart` for attached diagnostics. Use `--force` on `stop` or `restart` only when graceful shutdown fails; it kills QEMU and can corrupt guest data.

Inspect and connect:

```sh
qemu-manage log NAME
qemu-manage console NAME                 # Ctrl-] disconnects
qemu-manage monitor NAME                 # Ctrl-] disconnects
qemu-manage monitor NAME "info status"   # pipe-safe one-shot HMP
qemu-manage vnc NAME
```

`vnc` copies the configured password to the macOS clipboard and opens the authenticated live endpoint in Screen Sharing. It does not clear the clipboard.

If the guest agent was enabled before startup:

```sh
qemu-manage guest-agent NAME '{"execute":"guest-info"}'
qemu-manage guest-agent NAME '{"execute":"guest-ping"}'
```

The request must be one JSON object with `execute` and optional `arguments`; stdout is compact JSON.

## Autostart

```sh
qemu-manage autostart enable NAME --scope login
qemu-manage autostart enable NAME --scope boot
qemu-manage autostart status NAME
qemu-manage autostart disable NAME
```

`login` starts at user login. `boot` installs a system LaunchDaemon with narrow sudo operations, while QEMU still runs as the configured non-root user. Enabling autostart does not immediately start the VM. Disable autostart only while the VM is stopped.

## Delete safely

Deletion is permanent and is refused while the VM is running or autostart remains enabled:

```sh
qemu-manage stop NAME
qemu-manage autostart disable NAME  # if enabled
qemu-manage delete NAME
```

For noninteractive automation:

```sh
qemu-manage delete NAME --force
```

Here `--force` skips confirmation; it does not override running/autostart safety checks. Deletion removes managed configuration, copied firmware, installer, managed disks, logs, and runtime files. Original source images/ISOs are not removed; external drives and shares referenced in place are not managed copies.

## Automation and troubleshooting

- Use `qemu-manage status NAME --json`, `qemu-manage list --json`, and `qemu-manage doctor NAME --json` instead of parsing human output.
- Use `qemu-manage showcmd NAME` to inspect the safely quoted persistent QEMU command; one-shot `--boot-menu` is intentionally absent.
- Run `doctor NAME` after creating or changing network, firmware, disk, or executable settings.
- Read `qemu-manage log NAME` when startup or guest boot fails.
- Do not invoke the internal `supervise` command directly.
