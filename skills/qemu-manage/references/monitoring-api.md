# qemu-manage monitoring API v1

Each running VM may expose its own unauthenticated, IPv4 loopback-only HTTP server. Use it for statistics, cached state, health, and explicit guest-agent diagnostics.

## Enable and locate

```sh
qemu-manage create NAME --metrics-port 9101 [OTHER OPTIONS]
qemu-manage set NAME --metrics-port 9101
qemu-manage restart NAME
curl --fail-with-body http://127.0.0.1:9101/info
```

Use ports 1024–65535. The listener is always `127.0.0.1` and is not configurable. A port change takes effect after restart; `qemu-manage status NAME` reports `restart_required` until then. Disable with `qemu-manage set NAME --metrics-port off` and restart.

## Common contract

Exact routes:

- `/metrics`
- `/health`
- `/status`
- `/ping`
- `/info`

Only `GET` and `HEAD` are accepted. `HEAD` returns the GET status, headers, content type, and `Content-Length` without a body. Other methods return `405` with `Allow: GET, HEAD`; unknown paths and trailing-slash variants return `404`.

Every response sets `Cache-Control: no-store`. JSON is `application/json; charset=utf-8`, ends with a newline, and includes `api_version: 1`. Prometheus output uses `text/plain; version=0.0.4; charset=utf-8`. Timestamps are UTC RFC 3339 with nanosecond precision.

Collector statuses are `ok`, `pending`, `unsupported`, `failed`, and `stale`. Failed or stale collectors retain last-good JSON data, but their value metrics are omitted.

## Choose an endpoint

| Goal | Route | Behavior |
|---|---|---|
| Prometheus scrape | `/metrics` | `200` if representation renders; unavailable value families are omitted |
| Cached health check | `/health` | `200` healthy/degraded, `503` unhealthy |
| Detailed stats and guest addresses | `/status` | Cached snapshot, normally `200` even when collectors failed/stale |
| Live guest-agent probe | `/ping` | `200` success, `409` unconfigured/disabled, `503` live failure |
| Static identity/capabilities | `/info` | `200`, no volatile counters |

Use `/health` for liveness policy, `/metrics` for scraping, `/status` for inspection, and `/ping` only for explicit live QGA diagnosis.

## `/metrics`

```sh
curl --fail-with-body http://127.0.0.1:9101/metrics
```

Always-emitted availability/state families:

- `qemu_manage_qmp_up`
- `qemu_manage_vm_state_info{state=...}`
- `qemu_manage_vm_uptime_seconds`
- `qemu_manage_qemu_process_stats_up`
- `qemu_manage_guest_agent_configured`
- `qemu_manage_guest_agent_up`

When fresh and supported, metrics also cover:

- QMP lifecycle events and structured block I/O errors.
- QEMU process CPU, resident/wired/physical memory, disk bytes, page-ins, wakeups, instructions, cycles, and threads.
- QMP block-device bytes, operations, durations, failures, invalid operations, idle time, and I/O status.
- Guest CPU/load/vCPUs, filesystems, network, disk I/O, clock offset, and filesystem freeze state through QGA.

Counters from QMP and host-process sources reset with the VM supervisor. Guest counters may reset with the guest or device. Unsupported, failed, stale, or absent value families are omitted rather than emitted as zero.

Each listener represents one VM, so metrics intentionally omit VM-name/ID labels. Attach identity in Prometheus configuration:

```yaml
scrape_configs:
  - job_name: qemu-manage
    static_configs:
      - targets: ["127.0.0.1:9101"]
        labels:
          vm: home-assistant
```

Run Prometheus or its local agent on the same Mac.

## `/health`

```sh
curl --include http://127.0.0.1:9101/health
```

This reads cached observations and performs no VM/guest I/O.

- Fresh `running`: `healthy`, HTTP `200`.
- `paused`, `suspended`, or `debug`: `degraded`, HTTP `200`.
- Internal/error/migration/prelaunch/shutdown/watchdog/panic/colo states: `unhealthy`, HTTP `503`.
- Failed or missing QMP: `qmp_unavailable`, HTTP `503`.
- Stale QMP: `qmp_stale`, HTTP `503`.
- On Darwin, failed/stale process stats downgrade otherwise healthy QMP to `degraded`, HTTP `200`.

Key JSON fields are `ok`, `status`, `state`, `observed_at`, optional `code`, and `checks.qmp` / `checks.process`, each with collector `status`, nullable `observed_at`, and optional `code`.

Health-aware shell check:

```sh
if curl --silent --show-error --fail http://127.0.0.1:9101/health >/dev/null; then
  echo healthy-or-degraded
else
  echo unhealthy-or-unreachable
fi
```

## `/status`

```sh
curl --fail-with-body http://127.0.0.1:9101/status
curl --silent http://127.0.0.1:9101/status | jq .
```

This returns one immutable cached snapshot and remains `200` when it can render, including when collector state reports failure or staleness. Inspect:

- `health`: cached status and optional reason code.
- `vm`: ID, name, backend, architecture, raw state, start time, and uptime.
- `qmp`: freshness, state, attempt duration/time, QEMU version, lifecycle event counters, and block I/O error aggregates.
- `process`: exact child PID plus available macOS process CPU, memory, disk, wakeup, instruction/cycle, and thread statistics.
- `block_devices`: sorted cached QMP block statistics.
- `guest_agent`: configured/up, version, and latest live probe details.
- `guest`: available QGA CPU, load, vCPU, clock, freeze, filesystem, network-address/statistics, and disk samples.
- `collectors`: per-collector status, optional code, and last-success age.

Extract guest addresses:

```sh
curl --silent http://127.0.0.1:9101/status |
  jq -r '.guest.network_interfaces[]? as $i | $i.addresses[]? | [$i.name, .address, .family, (.prefix|tostring)] | @tsv'
```

Never infer success solely from HTTP `200`; inspect `health` and relevant collector status.

## `/ping`

```sh
curl --include http://127.0.0.1:9101/ping
```

This is the only live endpoint. It probes QGA with a two-second total deadline.

- `200`: QGA responded; JSON includes `ok: true`, `guest_agent: "available"`, `round_trip_seconds`, and `observed_at`.
- `409`: QGA is not configured or ping capability is disabled.
- `503`: QGA is unavailable, timed out, or had a protocol failure.

Failure JSON includes `ok: false`, stable `code`, fixed `message`, and `observed_at`. Use `curl --include` rather than `--fail` when distinguishing `409` from `503`.

## `/info`

```sh
curl --fail-with-body http://127.0.0.1:9101/info
```

Returns service version/Prometheus format, safe VM identity and configured resources, retained QEMU version and `hvf` accelerator, guest-agent configuration/version/capabilities, and the sorted route list. Guest capability keys may include `ping`, `cpu`, `load`, `vcpus`, `clock`, `filesystem_freeze`, `filesystems`, `network`, and `disk`.

## Freshness and security

QMP, block, and exact-child process data is collected every 10 seconds and becomes stale after 30 seconds. Guest collection runs every 30 seconds and becomes stale after 90 seconds. `/health`, `/status`, `/info`, and `/metrics` are cached; `/ping` is live.

The listener has no authentication because it is fixed to loopback. Do not expose it through a port forward or tunnel without adding external authentication and transport security. `/status` provides normalized guest IP address/family/prefix to local callers, but not guest MAC, hostname, user/OS data, credentials, filesystem paths, argv, environment, raw QGA payloads, or raw errors.
