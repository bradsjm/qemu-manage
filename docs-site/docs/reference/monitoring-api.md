---
title: Monitoring API
---

The monitoring API is a per-VM, loopback-only HTTP server owned by that VM's supervisor. Enable it with `--metrics-port PORT` during `create`, or change it with `set NAME --metrics-port PORT|off`. The valid range is 1024–65535, and the server always binds to `127.0.0.1`. Configuration changes take effect after restart.

The examples below use port `9101`:

```sh
qemu-manage set home-assistant --metrics-port 9101
```

## Routes

| Route | Result | Purpose |
|---|---|---|
| `/metrics` | `200` when the representation can be rendered | Prometheus 0.0.4 text for QMP, QEMU process, block I/O, and configured guest-agent collectors. Availability gauges are always emitted; unsupported, failed, stale, or absent value families are omitted rather than represented as zero. |
| `/health` | `200` for `healthy` or `degraded`; `503` for `unhealthy` | Cached health without VM or guest I/O. Fresh `running` is healthy; `paused`, `suspended`, and `debug` are degraded. QMP failures/staleness and unhealthy VM states produce `503`. |
| `/status` | `200` while the cached snapshot can be rendered | Full immutable cached snapshot: health, VM identity/state, QMP, process statistics, block devices, guest agent and guest data, plus per-collector status. Collector failures are represented in the body while the route remains `200`. |
| `/ping` | `200`, `409`, or `503` | The only live route. Runs a QGA ping with a two-second deadline. Returns `409` when QGA is not configured or ping is disabled, and `503` for unavailable, timeout, or protocol failure. |
| `/info` | `200` | Service, VM, QEMU, guest-agent capability, and route metadata without volatile counters. |

## Common HTTP contract

- The exact paths are `/metrics`, `/health`, `/status`, `/ping`, and `/info`; trailing-slash variants return `404`.
- Only `GET` and `HEAD` are accepted. Other methods return `405` with `Allow: GET, HEAD`.
- `HEAD` has the same status, content type, and `Content-Length` as `GET`, but no body.
- Every response includes `Cache-Control: no-store`.
- JSON is `application/json; charset=utf-8`, ends with a newline, and includes `"api_version": 1`.
- Prometheus output is `text/plain; version=0.0.4; charset=utf-8`.
- Timestamps are UTC RFC 3339 with nanosecond precision.
- The server writes no access log.

## CLI consumer

`qemu-manage info NAME` reads `/info` followed by `/status` for running or paused VMs. Both requests share one two-second deadline. The CLI accepts monitoring data only when both responses identify the requested VM and `/status` matches the backend PID and start time returned by the authenticated supervisor.

Each response is limited to 128 MiB by the CLI consumer. A redirect, transport failure, non-JSON or non-`200` response, invalid or incomplete payload, response above that limit, or VM/run identity mismatch discards both responses. The command then falls back to authenticated supervisor and durable configuration data instead of showing partial or potentially stale monitoring information.

This client-side validation and size limit do not change the HTTP endpoint schemas, freshness rules, security model, or status codes described below.

## Collector status

The collector status enum is:

| Status | Meaning |
|---|---|
| `ok` | The collector has a fresh successful result. |
| `pending` | Collection has not yet produced a result. |
| `unsupported` | The source is unavailable on this platform or capability. |
| `failed` | The latest collection failed. Last-good JSON data may be retained. |
| `stale` | Retained data is no longer fresh. Last-good JSON data may be retained. |

For `failed` and `stale`, value metrics are omitted even when last-good data remains in JSON. Stable error codes identify QMP, process-statistics, block-query, and guest-agent failures; see the canonical [Monitoring API contract](https://github.com/bradsjm/qemu-manage/blob/main/API.md) for the complete code list and field-level schemas.

## Examples

### Prometheus metrics

```sh
curl --fail-with-body http://127.0.0.1:9101/metrics
```

### Health

```sh
curl --fail-with-body http://127.0.0.1:9101/health
```

A healthy response includes `status: "healthy"`, the cached QEMU state, observation time, and QMP/process checks. An unhealthy response has `ok: false`, `status: "unhealthy"`, and a stable `code`.

### Cached status

```sh
curl --fail-with-body http://127.0.0.1:9101/status
```

The top-level snapshot includes `api_version`, `observed_at`, `health`, `vm`, `qmp`, `process`, `block_devices`, `guest_agent`, optional `guest` data, and `collectors`.

### Live guest-agent probe

```sh
curl --fail-with-body http://127.0.0.1:9101/ping  # 200 success
curl --include http://127.0.0.1:9101/ping         # inspect 409 or 503
```

A success includes `ok`, `guest_agent: "available"`, `round_trip_seconds`, and `observed_at`. A failure includes a stable `code` and fixed diagnostic `message`.

### Service metadata

```sh
curl --fail-with-body http://127.0.0.1:9101/info
```

The response describes the service/version and Prometheus format, durable VM metadata, QEMU version and HVF accelerator, known guest-agent version/capabilities, and the five sorted routes.
