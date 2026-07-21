---
title: Configuration Reference
---

Each managed VM has a durable, strict JSON configuration at:

```text
~/Library/Application Support/qemu-manage/vms/NAME/config.json
```

Print the complete canonical configuration without editing the managed file directly:

```sh
qemu-manage config show NAME
```

Use `qemu-manage config validate FILE` before `qemu-manage config apply NAME FILE`. Unknown properties, trailing JSON, invalid values, and unsupported schema versions fail validation. The checked-in [JSON Schema](https://github.com/bradsjm/qemu-manage/blob/main/schema.json) is the machine-readable reference; cross-field and cross-item invariants enforced by `config validate` remain authoritative.

## Top-level fields

The object rejects additional properties. These fields are required:

| Field | Type | Constraint or meaning |
|---|---|---|
| `$schema` | string | Must be `https://raw.githubusercontent.com/bradsjm/qemu-manage/main/schema.json`. |
| `schema_version` | integer | Must be `1`. |
| `id` | string | 32 lowercase hexadecimal characters. |
| `name` | string | 1–63 characters; starts alphanumeric, followed by alphanumeric, `.`, `_`, or `-`. |
| `backend` | string | Must be `qemu`. |
| `architecture` | string | Must be `aarch64`. |
| `uuid` | string | RFC 4122 version-4 UUID shape. |
| `cpus` | integer | 1–64. |
| `memory_mib` | integer | At least 256 MiB. |
| `restart_policy` | string | `never` or `on-failure`. |
| `shutdown_timeout_seconds` | integer | 1–3600 seconds. |
| `firmware` | object | Managed UEFI code and variables paths. |
| `disks` | array or `null` | VM disks. |
| `network` | object | User or `socket_vmnet` networking. |
| `guest_agent` | object | Guest-agent enablement. |
| `qemu` | object | QEMU executable and machine settings. |
| `autostart` | object | launchd scope. |

Optional top-level fields are `installer`, `vnc`, `metrics`, and `usb`.

## Firmware and installer

`firmware` requires non-empty string fields `code` and `variables`.

When present, `installer` requires a non-empty `path` and a non-negative integer `boot_index`.

## Disks

`disks` is either `null` or a unique array. Each disk rejects unknown fields and requires:

| Field | Constraint |
|---|---|
| `path` | Non-empty string. |
| `format` | `qcow2` or `raw`. |
| `serial` | 1–64 ASCII letters, digits, `.`, `_`, or `-`. |
| `boot_index` | Non-negative integer. |
| `read_only` | Boolean. |

The optional `cache` value is one of `none`, `writeback`, `writethrough`, `directsync`, or `unsafe`.

## Network

`network` requires `mode`, `mac`, and `forwards`.

- `mode` is `user` or `socket_vmnet`.
- `mac` is a lowercase locally administered unicast MAC address.
- `forwards` is `null` or a unique array. Each entry requires `protocol` (`tcp` or `udp`), IPv4 `host_address`, and `host_port`/`guest_port` in 1–65535.
- `smb_folder`, when present, is an absolute path and is only valid with user networking.

For `mode: "user"`, `socket_vmnet` must be absent. For `mode: "socket_vmnet"`, `socket_vmnet` is required, `smb_folder` must be absent, and `forwards` must be `null` or empty. The `socket_vmnet` object requires absolute `client_path` and `socket_path` values plus a non-empty `interface`.

## Guest agent

`guest_agent` contains one required boolean field, `enabled`.

## VNC

When present, `vnc` requires:

| Field | Constraint |
|---|---|
| `bind` | IPv4 address. |
| `port` | 5900–65535. |
| `port_to` | 5900–65535. |
| `password` | 1–8 UTF-8 bytes as enforced by `qemu-manage`; the schema's character length is only approximate for non-ASCII text. |

Optional `keyboard_layout` must be one of the layouts enumerated by the schema. When `vnc` is present, at most two USB selections are allowed.

## Metrics

When present, `metrics` contains required integer `port` in the range 1024–65535. The monitoring server binds to loopback.

## USB

When present, `usb` is a unique array of one to four devices (at most two with VNC). Each device uses exactly one selector form:

- `vendor_id` and `product_id`: nonzero, four-digit lowercase hexadecimal strings.
- `host_bus` (1–255) and `host_address` (1–127): integer host location.

## QEMU

`qemu` requires:

| Field | Constraint |
|---|---|
| `binary` | String path/name for QEMU. |
| `image_tool` | String path/name for `qemu-img`. |
| `machine` | Empty string, `virt`, or a versioned `virt-N.N` machine. |
| `extra_args` | `null` or an array of strings. |

Optional `rtc_base` is `utc` or `localtime`.

## Autostart

`autostart` contains the required `scope` field. Its value is `none`, `boot`, or `login`.
