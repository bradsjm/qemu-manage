---
title: Basic Prometheus monitoring on macOS
---

This example installs Prometheus on the same Mac that runs qemu-manage and scrapes one VM's loopback-only `/metrics` endpoint. It follows the Homebrew service approach described in [Mastering Prometheus and Grafana on macOS](https://medium.com/@aravind33b/mastering-prometheus-on-macos-seamless-integration-with-grafana-5da2a1c95092), with a qemu-manage scrape job and configuration validation added.

## 1. Enable metrics for a VM

Each VM has its own monitoring port. Enable an unused loopback port, restart the VM so the new supervisor opens it, and confirm the endpoint:

```sh
qemu-manage set home-assistant --metrics-port 9101
qemu-manage restart home-assistant
curl --fail-with-body http://127.0.0.1:9101/metrics
```

You can also select the port when creating a VM with `--metrics-port 9101`. Valid ports are 1024–65535. The server always binds to `127.0.0.1`, starts and stops with the VM, and does not require the guest network to be reachable.

The Home Assistant example uses port 8120 instead. Either value works; the Prometheus target must match the VM configuration.

## 2. Install Prometheus

Install the Homebrew formula:

```sh
brew install prometheus
```

Homebrew installs the configuration at `$(brew --prefix)/etc/prometheus.yml`. Its service wrapper reads arguments from `$(brew --prefix)/etc/prometheus.args`, stores data under the Homebrew `var/prometheus` directory, and binds the Prometheus web UI to `127.0.0.1:9090`.

## 3. Configure the scrape job

Edit the Homebrew configuration:

```sh
$EDITOR "$(brew --prefix)/etc/prometheus.yml"
```

Use this basic configuration:

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: prometheus
    static_configs:
      - targets:
          - 127.0.0.1:9090

  - job_name: qemu-manage
    metrics_path: /metrics
    static_configs:
      - targets:
          - 127.0.0.1:9101
        labels:
          vm: home-assistant
```

The static `vm` label identifies the target in queries. qemu-manage does not need that label internally because one monitoring endpoint belongs to exactly one VM.

Validate the file before starting or restarting Prometheus:

```sh
promtool check config "$(brew --prefix)/etc/prometheus.yml"
```

Fix every reported error before continuing.

## 4. Start Prometheus at login

Run Prometheus now and install its per-user Homebrew LaunchAgent:

```sh
brew services start prometheus
```

After later configuration changes, reload it with:

```sh
brew services restart prometheus
```

Inspect service state and logs with:

```sh
brew services list
cat "$(brew --prefix)/var/log/prometheus.log"
cat "$(brew --prefix)/var/log/prometheus.err.log"
```

Stop and remove the login service when it is no longer needed:

```sh
brew services stop prometheus
```

## 5. Verify the target

Open the local Prometheus target page:

```text
http://127.0.0.1:9090/targets
```

The `qemu-manage` target should report **UP**. In the Prometheus query UI, verify the scrape with:

```promql
up{job="qemu-manage", vm="home-assistant"}
```

A result of `1` means Prometheus completed the latest scrape. Useful qemu-manage queries include:

```promql
qemu_manage_qmp_up{vm="home-assistant"}
```

```promql
qemu_manage_vm_uptime_seconds{vm="home-assistant"}
```

```promql
rate(qemu_manage_qemu_process_cpu_seconds_total{vm="home-assistant"}[5m])
```

Availability gauges are always emitted. Value families that depend on unsupported, failed, or stale collectors are omitted rather than reported as zero, so inspect both target health and the relevant availability gauge.

## Monitor more VMs

Assign a unique monitoring port to each VM:

```sh
qemu-manage set database --metrics-port 9102
qemu-manage restart database
```

Add the target and label to the same job:

```yaml
  - job_name: qemu-manage
    metrics_path: /metrics
    static_configs:
      - targets: [127.0.0.1:9101]
        labels:
          vm: home-assistant
      - targets: [127.0.0.1:9102]
        labels:
          vm: database
```

Validate the configuration and restart Prometheus after every edit.

## Troubleshooting

### Target is down

Check the VM and endpoint directly:

```sh
qemu-manage status home-assistant
qemu-manage info home-assistant
curl --fail-with-body http://127.0.0.1:9101/health
curl --fail-with-body http://127.0.0.1:9101/metrics
```

A stopped VM has no monitoring server, so Prometheus correctly marks its target down. If metrics were enabled with `set`, restart the VM to apply the configuration.

### Prometheus rejects the configuration

Run `promtool check config` again and confirm that indentation uses spaces. Check the Prometheus service error log for startup failures.

### Prometheus is up but no qemu-manage job exists

Confirm that the service reads the file you edited:

```sh
cat "$(brew --prefix)/etc/prometheus.args"
```

The `--config.file` value should point to `$(brew --prefix)/etc/prometheus.yml`. Then restart the service and revisit `/targets`.

For the full route and metric-family contract, see [Monitoring API](../reference/monitoring-api.md).