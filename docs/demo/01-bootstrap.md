# 01. Bootstrap

Initialize the module, pin dependencies, and lay down the directory
skeleton.

## Project init

```bash
mkdir webhookd-demo && cd webhookd-demo
go mod init github.com/example/webhookd-demo
```

## Dependencies

Add them in one shot — `go mod tidy` will sort the indirect deps:

```bash
# HCL2 typed decoding
go get github.com/hashicorp/hcl/v2

# OpenTelemetry
go get go.opentelemetry.io/otel
go get go.opentelemetry.io/otel/sdk
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc
go get go.opentelemetry.io/otel/semconv/v1.26.0

# Prometheus
go get github.com/prometheus/client_golang/prometheus
go get github.com/prometheus/client_golang/prometheus/promhttp

# Rate limiting
go get golang.org/x/time/rate

# Kubernetes client (pin to controller-runtime's go.mod — see CLAUDE.md)
go get sigs.k8s.io/controller-runtime@v0.23.3
go get k8s.io/api@v0.35.0
go get k8s.io/apimachinery@v0.35.0
go get k8s.io/client-go@v0.35.0

go mod tidy
```

> **Why those k8s.io versions?** controller-runtime v0.23.3 is built
> against `k8s.io/* v0.35.0`. `go mod tidy` will jump to v0.36.0 and
> break with `HasSyncedChecker` interface mismatches. After any
> controller-runtime bump you must re-pin the `k8s.io/*` deps to match.
> This bit production webhookd during IMPL-0002.

## Skeleton dirs

Lay them down up front so the next phases can drop files straight in:

```bash
mkdir -p cmd/webhookd-demo cmd/mock-operator
mkdir -p internal/wizapi/v1alpha1
mkdir -p internal/config
mkdir -p internal/observability
mkdir -p internal/httpx
mkdir -p internal/webhook
mkdir -p internal/k8s
mkdir -p internal/integrations/jsm
mkdir -p internal/integrations/k8sbackend
```

## Stub `main.go`

A placeholder so the module compiles cleanly while we build out the
internal packages:

### `cmd/webhookd-demo/main.go`

```go
// Package main is the webhookd-demo entry point.
package main

import "fmt"

func main() {
    fmt.Println("webhookd-demo: coming online in phase 9")
}
```

Verify:

```bash
go build ./...
```

Should compile clean. If you see import errors, re-run `go mod tidy`.

## Versioning

The demo uses `go.mod` for tool requirements only. Tool pinning (mise,
asdf, etc.) is not in scope — see production webhookd's `mise.toml` for
the canonical pattern.

```go
// go.mod  — first few lines should look like this
module github.com/example/webhookd-demo

go 1.26

require (
    github.com/hashicorp/hcl/v2 v2.x.x
    github.com/prometheus/client_golang v1.x.x
    go.opentelemetry.io/otel v1.x.x
    // ... etc
)
```

## What we proved

- [x] Module compiles
- [x] Deps resolved with the right K8s pinning
- [x] Directory skeleton matches the layout from [00-overview.md](00-overview.md)

Onward to [02-config.md](02-config.md) — the HCL2 schema is where this
architecture really starts paying off.
