# 10. Local Stack (docker-compose)

Bring up an OTel collector + Prometheus + Jaeger locally via
docker-compose. The webhookd-demo binary runs natively on your host
and exports/scrapes against these.

The split is deliberate:

- **Native binary** for fast iteration (rebuild cycle = `go run`)
- **Containerized observability** so you don't install Prometheus/Jaeger
  on your laptop

## Files

```
docs/demo/
├── docker-compose.yaml          # otel-collector + prometheus + jaeger
├── otel-collector.yaml          # collector pipeline config
└── prometheus.yaml              # scrape config
```

These are real files in `docs/demo/` — copy this directory, run
`docker compose up`, and you have the whole stack.

## docker-compose.yaml

```yaml
# Observability stack for webhookd-demo. The binary runs natively on
# your host and pushes traces to localhost:4317 + serves /metrics on
# localhost:9090 (which Prometheus scrapes).
services:
  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.110.0
    command: ["--config=/etc/otelcol/config.yaml"]
    volumes:
      - ./otel-collector.yaml:/etc/otelcol/config.yaml:ro
    ports:
      - "4317:4317"   # OTLP/gRPC
      - "4318:4318"   # OTLP/HTTP
    depends_on:
      - jaeger

  jaeger:
    image: jaegertracing/all-in-one:1.61.0
    environment:
      COLLECTOR_OTLP_ENABLED: "true"
    ports:
      - "16686:16686" # UI
      - "14250:14250" # collector gRPC

  prometheus:
    image: prom/prometheus:v2.55.1
    command:
      - "--config.file=/etc/prometheus/prometheus.yaml"
      - "--storage.tsdb.retention.time=2h"
    volumes:
      - ./prometheus.yaml:/etc/prometheus/prometheus.yaml:ro
    ports:
      - "9091:9090"   # UI on localhost:9091 to avoid clashing with
                       # webhookd-demo's admin listener on :9090
```

> **Port note.** webhookd-demo's *admin* listener defaults to `:9090`,
> which is also Prometheus's default UI port. The compose file maps
> Prometheus to host `:9091` instead. Adjust if you change either.

## otel-collector.yaml

```yaml
# OTel Collector pipeline: receive OTLP traces, send to Jaeger.
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 1s
    send_batch_size: 1024

exporters:
  otlp/jaeger:
    endpoint: jaeger:4317
    tls:
      insecure: true

  # Useful when debugging. Comment out in steady state.
  debug:
    verbosity: basic
    sampling_initial: 1
    sampling_thereafter: 100

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/jaeger]
  telemetry:
    logs:
      level: info
```

## prometheus.yaml

```yaml
# Prometheus scrapes webhookd-demo's admin listener directly.
# When running webhookd-demo natively on the host, use host.docker.internal
# (macOS/Windows) or your bridge IP (Linux).
global:
  scrape_interval: 5s
  evaluation_interval: 5s

scrape_configs:
  - job_name: webhookd-demo
    static_configs:
      - targets:
          - host.docker.internal:9090
    metrics_path: /metrics

  - job_name: prometheus
    static_configs:
      - targets: ["localhost:9090"]
```

> **Linux users:** `host.docker.internal` doesn't resolve by default on
> Linux. Either add `extra_hosts: ["host.docker.internal:host-gateway"]`
> to the Prometheus service in compose, or replace the target with your
> Docker bridge IP (commonly `172.17.0.1`).

## Bring it up

```bash
cd docs/demo
docker compose up -d
```

Verify each piece:

```bash
docker compose ps
# NAME                            IMAGE                                                STATUS    PORTS
# docs-demo-jaeger-1              jaegertracing/all-in-one:1.61.0                      Up        0.0.0.0:14250->14250/tcp, ...
# docs-demo-otel-collector-1      otel/opentelemetry-collector-contrib:0.110.0         Up        0.0.0.0:4317->4317/tcp, ...
# docs-demo-prometheus-1          prom/prometheus:v2.55.1                              Up        0.0.0.0:9091->9090/tcp
```

Open the UIs:

- Prometheus: <http://localhost:9091>
- Jaeger: <http://localhost:16686>

If you've already run the binary, you should see:

- `webhookd_*` metrics under "Status → Targets" in Prometheus
- A `webhookd-demo` service in Jaeger's service dropdown

## Tear it down

```bash
docker compose down
```

Add `-v` to also drop the named volumes (none in this compose, so
it's a no-op — but the habit is good).

## What we proved

- [x] One-command observability stack
- [x] OTel traces routed to Jaeger
- [x] Prometheus scrapes the host-running binary
- [x] No hidden state — everything is in two YAML files

Next: [11-image-build.md](11-image-build.md) — production-shaped
image builds.
