# Monitoring API

The monitoring API is a per-VM, loopback-only HTTP server owned by that VM's
supervisor. This document is the normative contract for API version 1.

## Enabling the endpoint

Enable monitoring while creating a VM:

```sh
qemu-manage create home-assistant --metrics-port 9101 # plus normal create options
```

Enable, move, or disable it later:

```sh
qemu-manage set home-assistant --metrics-port 9101
qemu-manage set home-assistant --metrics-port off
```

The valid port range is 1024–65535. The server always binds IPv4
`127.0.0.1`; its address is not configurable. A configuration change takes
effect after the VM is restarted and appears as `restart_required` while the
old supervisor is still running.

## Common HTTP contract

The exact routes are `/metrics`, `/health`, `/status`, `/ping`, and `/info`.
Only `GET` and `HEAD` are accepted. `HEAD` renders the same representation as
`GET`, returns the same status, content type, and `Content-Length`, then omits
the body. Other methods return `405` and `Allow: GET, HEAD`; any other path,
including a trailing-slash variant, returns `404`.

Every response has `Cache-Control: no-store`. JSON uses
`application/json; charset=utf-8`, ends in one newline, and contains
`api_version: 1`. Prometheus text uses
`text/plain; version=0.0.4; charset=utf-8`. Timestamps are UTC RFC 3339 with
nanosecond precision. The server writes no access log.

The `qemu-manage info NAME` consumer reads `/info` and `/status` for running
or paused VMs. Before rendering their data, it validates each endpoint's VM
identity and checks `/status` run identity against the authenticated supervisor
run. The CLI limits each response body to 128 MiB; if monitoring is disabled,
unavailable, mismatched, invalid, or exceeds that limit, `info` falls back to
authenticated supervisor/config state. These consumer checks do not change the
endpoint schemas, freshness, security, or status codes described below.

The collector status enum is `ok`, `pending`, `unsupported`, `failed`, or
`stale`. Collector error codes are limited to `qmp_unavailable`,
`qmp_timeout`, `qmp_protocol_error`, `process_stats_unavailable`,
`block_query_failed`, `guest_agent_not_configured`,
`guest_agent_command_disabled`, `guest_agent_unavailable`,
`guest_agent_timeout`, `guest_agent_protocol_error`, and
`guest_agent_command_failed`. A failed or stale collector retains last-good
JSON data but its value metrics are omitted.

## `/metrics`

Returns `200` whenever the representation can be rendered. Each endpoint is
one VM, so no metric has VM name, immutable ID, PID, path, configuration hash,
IP address, or MAC-address labels. Supply target identity in Prometheus
configuration. Counters reset with the supervisor for QMP and host-process
sources; guest counters can reset when the guest or a guest device restarts.
Availability gauges are always emitted; unsupported, failed, stale, or absent
value families are omitted rather than reported as zero.

| Metric | Type | Labels | Unit | Availability and reset |
|---|---|---|---|---|
| `qemu_manage_qmp_up` | gauge | none | boolean | Always; `1` only for fresh QMP |
| `qemu_manage_vm_state_info` | gauge | `state` | info | Always; one raw QEMU state or `unknown` |
| `qemu_manage_vm_uptime_seconds` | gauge | none | seconds | Always; supervisor lifetime |
| `qemu_manage_qmp_events_total` | counter | `event` | events | Fresh QMP; fixed labels `shutdown`, `powerdown`, `reset`, `stop`, `resume`, `suspend`, `suspend_disk`, `wakeup`, `guest_panicked`, `watchdog`; supervisor-lifetime observations; unknown events ignored |
| `qemu_manage_qemu_process_stats_up` | gauge | none | boolean | Always; `0` off Darwin or on failed/stale collection |
| `qemu_manage_qemu_process_cpu_seconds_total` | counter | `mode=user|system` | seconds | Fresh macOS child stats; supervisor reset |
| `qemu_manage_qemu_process_resident_memory_bytes` | gauge | none | bytes | Fresh macOS child stats |
| `qemu_manage_qemu_process_physical_footprint_bytes` | gauge | none | bytes | Fresh macOS child stats |
| `qemu_manage_qemu_process_physical_footprint_peak_bytes` | gauge | none | bytes | Fresh macOS child stats |
| `qemu_manage_qemu_process_wired_memory_bytes` | gauge | none | bytes | Fresh macOS child stats |
| `qemu_manage_qemu_process_threads` | gauge | none | threads | Fresh macOS child stats |
| `qemu_manage_qemu_process_disk_read_bytes_total` | counter | none | bytes | Fresh macOS child stats; supervisor reset |
| `qemu_manage_qemu_process_disk_written_bytes_total` | counter | none | bytes | Fresh macOS child stats; supervisor reset |
| `qemu_manage_qemu_process_pageins_total` | counter | none | page-ins | Fresh macOS child stats; supervisor reset |
| `qemu_manage_qemu_process_idle_wakeups_total` | counter | none | wakeups | Fresh macOS child stats; supervisor reset |
| `qemu_manage_qemu_process_interrupt_wakeups_total` | counter | none | wakeups | Fresh macOS child stats; supervisor reset |
| `qemu_manage_qemu_process_instructions_total` | counter | none | instructions | Fresh macOS child stats; supervisor reset |
| `qemu_manage_qemu_process_cycles_total` | counter | none | cycles | Fresh macOS child stats; supervisor reset |
| `qemu_manage_block_io_bytes_total` | counter | `device`, `direction=read|write|unmap` | bytes | Fresh supplied QMP block sample; supervisor reset |
| `qemu_manage_block_io_operations_total` | counter | `device`, `operation=read|write|flush|unmap` | operations | Fresh supplied QMP sample; supervisor reset |
| `qemu_manage_block_io_seconds_total` | counter | `device`, `operation=read|write|flush|unmap` | seconds | Fresh supplied QMP sample; supervisor reset |
| `qemu_manage_block_failed_operations_total` | counter | `device`, `operation` | operations | Fresh supplied QMP sample; supervisor reset |
| `qemu_manage_block_invalid_operations_total` | counter | `device`, `operation` | operations | Fresh supplied QMP sample; supervisor reset |
| `qemu_manage_block_idle_seconds` | gauge | `device` | seconds | Fresh supplied QMP sample |
| `qemu_manage_block_io_status_info` | gauge | `device`, `status` | info | Fresh supplied QMP status |
| `qemu_manage_block_io_errors_total` | counter | `device`, `operation`, `nospace=true|false` | events | Starts after a structured event; supervisor reset |
| `qemu_manage_guest_agent_configured` | gauge | none | boolean | Always |
| `qemu_manage_guest_agent_up` | gauge | none | boolean | Always; fresh successful guest-info cycle |
| `qemu_manage_guest_agent_probe_duration_seconds` | gauge | none | seconds | After a completed `/ping` probe |
| `qemu_manage_guest_cpu_seconds_total` | counter | `cpu`, `mode=user|nice|system|idle|iowait|irq|softirq|steal|guest|guestnice` | seconds | Fresh supplied QGA CPU sample; guest reset |
| `qemu_manage_guest_load1` | gauge | none | load | Fresh supplied QGA load sample |
| `qemu_manage_guest_load5` | gauge | none | load | Fresh supplied QGA load sample |
| `qemu_manage_guest_load15` | gauge | none | load | Fresh supplied QGA load sample |
| `qemu_manage_guest_vcpus` | gauge | `state=online|offline` | vCPUs | Fresh QGA vCPU sample |
| `qemu_manage_guest_filesystem_size_bytes` | gauge | `mountpoint`, `fstype` | bytes | Fresh supplied QGA filesystem sample |
| `qemu_manage_guest_filesystem_used_bytes` | gauge | `mountpoint`, `fstype` | bytes | Fresh supplied QGA filesystem sample |
| `qemu_manage_guest_network_receive_bytes_total` | counter | `interface` | bytes | Fresh supplied QGA statistic; guest reset |
| `qemu_manage_guest_network_transmit_bytes_total` | counter | `interface` | bytes | Fresh supplied QGA statistic; guest reset |
| `qemu_manage_guest_network_receive_packets_total` | counter | `interface` | packets | Fresh supplied QGA statistic; guest reset |
| `qemu_manage_guest_network_transmit_packets_total` | counter | `interface` | packets | Fresh supplied QGA statistic; guest reset |
| `qemu_manage_guest_network_receive_errors_total` | counter | `interface` | errors | Fresh supplied QGA statistic; guest reset |
| `qemu_manage_guest_network_transmit_errors_total` | counter | `interface` | errors | Fresh supplied QGA statistic; guest reset |
| `qemu_manage_guest_network_receive_dropped_total` | counter | `interface` | packets | Fresh supplied QGA statistic; guest reset |
| `qemu_manage_guest_network_transmit_dropped_total` | counter | `interface` | packets | Fresh supplied QGA statistic; guest reset |
| `qemu_manage_guest_disk_sectors_total` | counter | `device`, `operation=read|write|discard` | sectors | Fresh supplied QGA sample; guest reset |
| `qemu_manage_guest_disk_operations_total` | counter | `device`, `operation=read|write|discard|flush` | operations | Fresh supplied QGA sample; guest reset |
| `qemu_manage_guest_disk_merged_operations_total` | counter | `device`, `operation=read|write|discard` | operations | Fresh supplied QGA sample; guest reset |
| `qemu_manage_guest_disk_io_seconds_total` | counter | `device`, `operation=read|write|discard|flush` | seconds | Fresh supplied QGA sample; guest reset |
| `qemu_manage_guest_disk_weighted_io_seconds_total` | counter | `device` | seconds | Fresh supplied QGA sample; guest reset |
| `qemu_manage_guest_disk_io_in_flight` | gauge | `device` | operations | Fresh supplied QGA sample |
| `qemu_manage_guest_clock_offset_seconds` | gauge | none | seconds | Fresh QGA guest-time sample versus request midpoint |
| `qemu_manage_guest_filesystems_frozen` | gauge | none | boolean | Fresh QGA freeze status; `1` frozen |

Example:

```sh
curl --fail-with-body http://127.0.0.1:9101/metrics
```

## `/health`

Performs no VM or guest I/O. Fresh `running` is `healthy/200`; `paused`,
`suspended`, and `debug` are `degraded/200`. Migration, internal/I/O error,
prelaunch, shutdown, watchdog, panic, and colo states are `unhealthy/503`.
Failed/missing QMP is `qmp_unavailable/503`; stale QMP is `qmp_stale/503`.
An unknown future state is `unsupported_state/503`. On Darwin only, failed or
stale process statistics downgrade otherwise healthy QMP to `degraded/200`.
Unsupported and pending process statistics are neutral.

| JSON field | Type | Required/null | Values, units, and source |
|---|---|---|---|
| `api_version` | integer | required | `1` |
| `ok` | boolean | required | `false` only for unhealthy `503` |
| `status` | string | required | `healthy`, `degraded`, `unhealthy` |
| `state` | string | required | Cached raw QEMU run state |
| `observed_at` | timestamp | required | Snapshot publication time |
| `code` | string | optional | Stable health reason |
| `checks.qmp.status` | string | required | Collector status enum |
| `checks.qmp.observed_at` | timestamp | required, nullable | Last QMP attempt |
| `checks.qmp.code` | string | optional | Stable collector code |
| `checks.process.status` | string | required | Includes `pending`/`unsupported` |
| `checks.process.observed_at` | timestamp | required, nullable | Last process attempt |
| `checks.process.code` | string | optional | Stable collector code |

```json
{"api_version":1,"ok":true,"status":"healthy","state":"running","observed_at":"2026-07-20T12:00:00Z","checks":{"qmp":{"status":"ok","observed_at":"2026-07-20T12:00:00Z"},"process":{"status":"ok","observed_at":"2026-07-20T12:00:00Z"}}}
```

```json
{"api_version":1,"ok":false,"status":"unhealthy","state":"running","observed_at":"2026-07-20T12:01:00Z","code":"qmp_stale","checks":{"qmp":{"status":"stale","observed_at":"2026-07-20T12:00:00Z","code":"qmp_stale"},"process":{"status":"unsupported","observed_at":null}}}
```

```sh
curl --fail-with-body http://127.0.0.1:9101/health
```

## `/status`

Returns `200` while the server can render its immutable cached snapshot.

| JSON field | Type | Required/null | Values, units, freshness |
|---|---|---|---|
| `api_version` | integer | required | `1` |
| `observed_at` | timestamp | required | Snapshot publication |
| `health.status` | string | required | Health enum |
| `health.code` | string | optional | Stable health reason |
| `vm.id`, `vm.name` | string | required | Safe durable identity |
| `vm.backend`, `vm.architecture` | string | required | Configured backend/architecture |
| `vm.state` | string | required | Cached raw QMP state |
| `vm.started_at` | timestamp | required | Supervisor start |
| `vm.uptime_seconds` | number | required | Seconds since supervisor start |
| `qmp.up` | boolean | required | Fresh QMP state |
| `qmp.state` | string | required | Cached raw state |
| `qmp.duration_seconds` | number | required | Latest attempt duration |
| `qmp.observed_at` | timestamp | required, nullable | Latest attempt |
| `qmp.version.{major,minor,micro}` | integer | required | Retained greeting |
| `qmp.version.package` | string | required | Retained greeting |
| `qmp.events` | object | required | Supervisor-lifetime lifecycle counters plus structured block-event aggregates |
| `qmp.events.lifecycle` | object | required | Fixed keys `shutdown`, `powerdown`, `reset`, `stop`, `resume`, `suspend`, `suspend_disk`, `wakeup`, `guest_panicked`, `watchdog`; unknown QMP events ignored |
| `qmp.events.block_io_errors[]` | array | required, nullable | Structured `BLOCK_IO_ERROR` aggregates |
| `qmp.events.block_io_errors[].device` | string | optional | `""` when QEMU reports no associated device |
| `process.stats_up` | boolean | required | Fresh process collector |
| `process.pid` | integer | required | Exact supervised QEMU child PID |
| `process.observed_at` | timestamp | required, nullable | Latest attempt |
| `process.cpu_seconds.{user,system}` | number | optional | Seconds, last good |
| `process.resident_memory_bytes` | integer | optional | Bytes, last good |
| `process.wired_memory_bytes` | integer | optional | Bytes, last good |
| `process.physical_footprint_bytes` | integer | optional | Bytes, last good |
| `process.physical_footprint_peak_bytes` | integer | optional | Bytes, last good |
| `process.disk_read_bytes`, `disk_written_bytes` | integer | optional | Bytes, last good |
| `process.pageins`, `idle_wakeups`, `interrupt_wakeups` | integer | optional | Cumulative, last good |
| `process.instructions`, `cycles`, `threads` | integer | optional | Last good |
| `block_devices[]` | array | required | Sorted cached QMP devices; absent samples omitted |
| `guest_agent.configured`, `guest_agent.up` | boolean | required | Config and fresh guest-info state |
| `guest_agent.version` | string | optional | Last known QGA version |
| `guest_agent.probe_duration_seconds` | number | optional | Latest completed live ping |
| `guest_agent.probe_observed_at` | timestamp | optional | Latest completed live ping |
| `guest.cpus[]` | array | optional | Sorted CPU/mode seconds |
| `guest.load.{load1,load5,load15}` | number | optional | Supplied load values |
| `guest.vcpus[]` | array | optional | Sorted logical ID and online state |
| `guest.clock_offset_seconds` | number | optional | Guest minus host midpoint seconds |
| `guest.filesystems_frozen` | boolean | optional | Last freeze status |
| `guest.filesystems[]` | array | optional | Mountpoint, fstype, optional size/used bytes |
| `guest.network_interfaces[]` | array | optional | Sorted interfaces with supplied stats/addresses |
| `guest.network_interfaces[].name` | string | required | QGA interface name |
| `guest.network_interfaces[].statistics` | object | optional | Only supplied counters |
| `guest.network_interfaces[].addresses` | array | optional | Omitted if QGA omitted source; `[]` if known empty |
| `guest.network_interfaces[].addresses[].address` | string | required | Canonical IP text |
| `guest.network_interfaces[].addresses[].family` | string | required | `ipv4` or `ipv6` |
| `guest.network_interfaces[].addresses[].prefix` | integer | required | `0..32` or `0..128` |
| `guest.disks[]` | array | optional | Sorted supplied disk samples |
| `collectors.<key>.status` | string | required | Collector status enum |
| `collectors.<key>.code` | string | optional | Stable collector code |
| `collectors.<key>.age_seconds` | number/null | required | Age of last success or null |

```json
{"api_version":1,"observed_at":"2026-07-20T12:00:00Z","health":{"status":"healthy"},"vm":{"id":"0123456789abcdef0123456789abcdef","name":"home-assistant","backend":"qemu","architecture":"aarch64","state":"running","started_at":"2026-07-20T11:00:00Z","uptime_seconds":3600},"qmp":{"up":true,"state":"running","duration_seconds":0.002,"observed_at":"2026-07-20T12:00:00Z","version":{"major":11,"minor":0,"micro":1,"package":""},"events":{"lifecycle":{"shutdown":0,"powerdown":0,"reset":0,"stop":0,"resume":0,"suspend":0,"suspend_disk":0,"wakeup":0,"guest_panicked":0,"watchdog":0},"block_io_errors":[]}},"process":{"stats_up":true,"pid":4242,"observed_at":"2026-07-20T12:00:00Z","cpu_seconds":{"user":20.5,"system":3.1},"resident_memory_bytes":1073741824},"block_devices":[],"guest_agent":{"configured":true,"up":true,"observed_at":"2026-07-20T12:00:00Z","version":"9.1"},"guest":{"network_interfaces":[{"name":"eth0","addresses":[{"address":"192.0.2.10","family":"ipv4","prefix":24}]}]},"collectors":{"qmp":{"status":"ok","age_seconds":0},"process":{"status":"ok","age_seconds":0}}}
```

When QEMU reports a structured block I/O error without an associated device, the aggregate keeps `device` as the empty string rather than inventing a placeholder:

```json
{"qmp":{"events":{"block_io_errors":[{"device":"","operation":"read","nospace":false,"count":1}]}}}
```

Failure is represented by collector state while the route remains `200`:

```json
{"api_version":1,"observed_at":"2026-07-20T12:02:00Z","health":{"status":"unhealthy","code":"qmp_stale"},"vm":{"id":"0123456789abcdef0123456789abcdef","name":"home-assistant","backend":"qemu","architecture":"aarch64","state":"running","started_at":"2026-07-20T11:00:00Z","uptime_seconds":3720},"qmp":{"up":false,"state":"running","duration_seconds":5,"observed_at":"2026-07-20T12:01:00Z","version":{"major":11,"minor":0,"micro":1,"package":""},"events":{"lifecycle":{},"block_io_errors":null}},"process":{"stats_up":false,"pid":4242,"observed_at":null},"block_devices":[],"guest_agent":{"configured":false,"up":false,"observed_at":null},"collectors":{"qmp":{"status":"stale","code":"qmp_stale","age_seconds":120},"process":{"status":"unsupported","age_seconds":null}}}
```

```sh
curl --fail-with-body http://127.0.0.1:9101/status
curl --silent http://127.0.0.1:9101/status |
  jq -r '.guest.network_interfaces[]? as $i | $i.addresses[]? | [$i.name, .address, .family, (.prefix|tostring)] | @tsv'
```

## `/ping`

This is the only live route. It has a two-second deadline including the shared
QGA gate wait. Only overlapping requests share an in-flight probe; a later
request starts a new probe. It returns `409` when QGA is unconfigured or cached
capabilities disable ping, `200` on success, and `503` for unavailable, timeout,
or protocol failure.

| JSON field | Type | Required/null | Values, units, source |
|---|---|---|---|
| `api_version` | integer | required | `1` |
| `ok` | boolean | required | Probe result |
| `guest_agent` | string | success only | `available` |
| `round_trip_seconds` | number | success only | Live probe duration |
| `observed_at` | timestamp | required | Probe completion/failure time |
| `code` | string | failure only | Stable ping code |
| `message` | string | failure only | Fixed diagnostic; no transport detail |

```json
{"api_version":1,"ok":true,"guest_agent":"available","round_trip_seconds":0.004,"observed_at":"2026-07-20T12:00:00Z"}
```

```json
{"api_version":1,"ok":false,"code":"guest_agent_not_configured","message":"guest agent is not configured","observed_at":"2026-07-20T12:00:00Z"}
```

```sh
curl --fail-with-body http://127.0.0.1:9101/ping # 200 success
curl --include http://127.0.0.1:9101/ping        # inspect 409 or 503
```

## `/info`

Returns `200` and no volatile counters.

| JSON field | Type | Required/null | Values and source |
|---|---|---|---|
| `api_version` | integer | required | `1` |
| `service.name` | string | required | `qemu-manage` |
| `service.version` | string | required | Running build version |
| `service.metrics_format` | string | required | `prometheus` |
| `service.metrics_format_version` | string | required | `0.0.4` |
| `vm.id`, `vm.name`, `vm.backend`, `vm.architecture` | string | required | Safe copied config |
| `vm.cpus`, `vm.memory_mib` | integer | required | Copied config |
| `qemu.version.{major,minor,micro}` | integer | required | Retained greeting |
| `qemu.version.package` | string | required | Retained greeting |
| `qemu.accelerator` | string | required | `hvf` |
| `guest_agent.configured` | boolean | required | Copied config |
| `guest_agent.version` | string | required | Last known or empty |
| `guest_agent.capabilities` | object | required | Confirmed booleans; unknown entries omitted |
| `routes[]` | string | required | Five sorted exact routes |

Capability keys are `ping`, `cpu`, `load`, `vcpus`, `clock`,
`filesystem_freeze`, `filesystems`, `network`, and `disk`.

```json
{"api_version":1,"service":{"name":"qemu-manage","version":"0.6.1","metrics_format":"prometheus","metrics_format_version":"0.0.4"},"vm":{"id":"0123456789abcdef0123456789abcdef","name":"home-assistant","backend":"qemu","architecture":"aarch64","cpus":4,"memory_mib":4096},"qemu":{"version":{"major":11,"minor":0,"micro":1,"package":""},"accelerator":"hvf"},"guest_agent":{"configured":true,"version":"9.1","capabilities":{"ping":true,"network":true}},"routes":["/health","/info","/metrics","/ping","/status"]}
```

```sh
curl --fail-with-body http://127.0.0.1:9101/info
```

## Prometheus integration

Use a static target label for VM identity instead of adding high-cardinality or
volatile labels to every metric:

```yaml
scrape_configs:
  - job_name: qemu-manage
    static_configs:
      - targets: ["127.0.0.1:9101"]
        labels:
          vm: home-assistant
```

Run Prometheus or a local agent on the same Mac. Guest IP addresses are
intentionally absent from Prometheus; retrieve canonical addresses from
`/status` when needed.

## curl and third-party monitoring

A health-aware shell check can distinguish success from `503`:

```sh
if curl --silent --show-error --fail http://127.0.0.1:9101/health >/dev/null; then
  echo healthy-or-degraded
else
  echo unhealthy-or-unreachable
fi
```

Use `curl --include` to distinguish `/ping` `200` (available), `409`
(unconfigured/disabled), and `503` (live failure). Third-party systems should
scrape `/metrics`, use `/health` for cached liveness policy, and reserve `/ping`
for explicit guest-agent diagnostics.

## Security and freshness

The endpoint is unauthenticated because it is fixed to loopback. `/status`
deliberately exposes only normalized QGA-reported IP address, family, and prefix
to local callers. It never exposes guest MAC address, hostname, OS/user data,
raw QGA payloads, errors, credentials, paths, argv, or environment. The other
routes expose no guest address identity. Do not publish the listener with a
port forward or tunnel without adding external authentication and transport
security.

QMP, block, and exact-child process collection run every 10 seconds and become
stale after 30 seconds. Guest collection starts asynchronously, repeats every
30 seconds, and becomes stale after 90 seconds. `/health`, `/status`, `/info`,
and `/metrics` read one immutable snapshot and perform no protocol I/O. `/ping`
is live and separately bounded. Balloon-derived memory, guest-memory blocks,
arbitrary guest execution, unstable accelerator statistics, virtual-address
size, QoS/energy, and interval-footprint values are not collected.
