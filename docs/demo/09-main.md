# 09. main.go — wiring everything

The entry point pulls together every component built in phases 1–8:

1. Parse the `--config` flag
2. Load HCL → typed `*config.File`
3. Build the logger, metrics, tracer
4. Build K8s clients + register the real Backend
5. Resolve `Instance`s from the registry
6. Construct the idempotency tracker + dispatcher
7. Build the public + admin HTTP servers
8. Run them under a signal-aware context with graceful shutdown

The pattern is "fat `main()` is fine" — a single function, top-to-bottom.
We split the body into a `realMain() int` so deferred cleanup runs even
on `os.Exit`.

## File

### `cmd/webhookd-demo/main.go`

```go
// Package main is the webhookd-demo entry point.
package main

import (
    "context"
    "errors"
    "flag"
    "log/slog"
    "net/http"
    "os"
    "os/signal"
    "sync"
    "syscall"
    "time"

    "github.com/example/webhookd-demo/internal/config"
    "github.com/example/webhookd-demo/internal/httpx"
    "github.com/example/webhookd-demo/internal/integrations/k8sbackend"
    "github.com/example/webhookd-demo/internal/k8s"
    "github.com/example/webhookd-demo/internal/observability"
    "github.com/example/webhookd-demo/internal/webhook"

    // Anonymous imports drive init()-based registration (ADR-0010).
    _ "github.com/example/webhookd-demo/internal/integrations/jsm"
    _ "github.com/example/webhookd-demo/internal/integrations/k8sbackend"
)

// Set at build time via -ldflags. Not load-bearing for the demo.
var (
    version = "dev"
    commit  = "unknown"
)

func main() { os.Exit(realMain()) }

func realMain() int {
    var configPath string
    flag.StringVar(&configPath, "config", "webhookd.hcl", "path to HCL config")
    flag.Parse()

    log := observability.NewLogger(observability.WithLevel(envOr("LOG_LEVEL", "info")))

    cfg, diags := config.Load(configPath)
    if diags.HasErrors() {
        log.Error("load config", "err", diags.Error())
        return 1
    }

    m := observability.NewMetrics(version)
    log.Info("startup",
        "version", version,
        "commit", commit,
        "instances", len(cfg.Instances),
    )

    rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    // Tracing.
    tp, err := observability.NewTracerProvider(rootCtx, observability.TracingOptions{
        Enabled:  cfg.Runtime.Tracing != nil && cfg.Runtime.Tracing.Enabled,
        Endpoint: tracingEndpoint(cfg),
        Service:  tracingService(cfg, "webhookd-demo"),
        Version:  version,
    })
    if err != nil {
        log.Error("init tracing", "err", err)
        return 1
    }
    defer shutdownTracer(tp, log)

    // K8s clients + Backend setup. The k8sbackend package init() puts a
    // placeholder in the registry; Setup swaps in the real backend.
    kubeconfig := pickKubeconfig(cfg)
    clients, err := k8s.NewClients(kubeconfig)
    if err != nil {
        log.Error("build k8s clients", "err", err)
        return 1
    }
    if err := k8sbackend.Setup(clients); err != nil {
        log.Error("k8s backend setup", "err", err)
        return 1
    }

    // Resolve instances. Each Instance binds Provider+Backend with their
    // typed configs, ready for the dispatcher.
    instances, err := webhook.Resolve(cfg, webhook.Default)
    if err != nil {
        log.Error("resolve instances", "err", err)
        return 1
    }

    tracker := webhook.NewIdempotencyTracker(idempotencyTTL(cfg), 10_000)

    disp, err := webhook.NewDispatcher(webhook.DispatcherConfig{
        Instances:    instances,
        Tracker:      tracker,
        Metrics:      m,
        MaxBodyBytes: maxBodyBytes(cfg),
        Logger:       log,
    })
    if err != nil {
        log.Error("build dispatcher", "err", err)
        return 1
    }

    // Build the middleware chain and the two HTTP servers.
    chain := func(h http.Handler) http.Handler {
        h = httpx.MaxBodyBytes(maxBodyBytes(cfg))(h)
        h = httpx.NewRateLimiter(rateLimit(cfg), m.RateLimitDrops)(h)
        h = httpx.WithInFlight(m.HTTPInFlight)(h)
        h = httpx.WithRequestID(h)
        return h
    }

    publicServer := httpx.NewServer(
        httpx.DefaultServerConfig(cfg.Runtime.Addr),
        chain(disp.Handler()),
    )
    adminServer := httpx.NewAdminServer(httpx.AdminConfig{
        Addr:     cfg.Runtime.AdminAddr,
        Registry: m.Registry,
        Ready:    func(_ context.Context) error { return nil },
    })

    // Run.
    var wg sync.WaitGroup
    serve := func(s *http.Server, name string) {
        wg.Add(1)
        defer wg.Done()
        log.Info("listening", "name", name, "addr", s.Addr)
        if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            log.Error("server died", "name", name, "err", err)
        }
    }
    go serve(publicServer, "public")
    go serve(adminServer, "admin")

    <-rootCtx.Done()
    log.Info("shutdown signal received")

    drainServers(publicServer, adminServer, log, shutdownTimeout(cfg))
    wg.Wait()

    log.Info("shutdown complete")
    return 0
}

// drainServers calls Shutdown on each *http.Server with a detached
// timeout so the parent ctx cancellation (which signaled shutdown in
// the first place) doesn't immediately abort the drain.
func drainServers(public, admin *http.Server, log *slog.Logger, timeout time.Duration) {
    ctx, cancel := context.WithTimeout(context.Background(), timeout) //nolint:contextcheck
    defer cancel()

    var wg sync.WaitGroup
    drain := func(s *http.Server, name string) {
        wg.Add(1)
        go func() {
            defer wg.Done()
            if err := s.Shutdown(ctx); err != nil {
                log.Error("shutdown", "name", name, "err", err)
            }
        }()
    }
    drain(public, "public")
    drain(admin, "admin")
    wg.Wait()
}

func shutdownTracer(tp *observability.TracerProvider, log *slog.Logger) {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) //nolint:contextcheck
    defer cancel()
    if err := tp.Shutdown(ctx); err != nil {
        log.Warn("tracer shutdown", "err", err)
    }
}

// helpers — pull config values with sensible fallbacks.

func envOr(k, fallback string) string {
    if v := os.Getenv(k); v != "" {
        return v
    }
    return fallback
}

func tracingEndpoint(cfg *config.File) string {
    if cfg.Runtime.Tracing == nil {
        return ""
    }
    return cfg.Runtime.Tracing.Endpoint
}

func tracingService(cfg *config.File, fallback string) string {
    if cfg.Runtime.Tracing == nil || cfg.Runtime.Tracing.Service == "" {
        return fallback
    }
    return cfg.Runtime.Tracing.Service
}

func pickKubeconfig(cfg *config.File) string {
    // The K8s backend's per-instance config supplies KubeconfigEnv.
    // For the demo (single backend instance), grab it off the first
    // instance — production code would build per-instance clients.
    for _, inst := range cfg.Instances {
        if inst.Backend.Type == "k8s" {
            // The HCL body hasn't been decoded yet at this point —
            // we get the value via os.LookupEnv KUBECONFIG fallback.
            return os.Getenv("KUBECONFIG")
        }
    }
    return os.Getenv("KUBECONFIG")
}

func idempotencyTTL(cfg *config.File) time.Duration {
    if cfg.Defaults == nil || cfg.Defaults.IdempotencyTTL == "" {
        return 5 * time.Minute
    }
    d, err := time.ParseDuration(cfg.Defaults.IdempotencyTTL)
    if err != nil {
        return 5 * time.Minute
    }
    return d
}

func maxBodyBytes(cfg *config.File) int64 {
    if cfg.Defaults == nil || cfg.Defaults.MaxBodyBytes == 0 {
        return 1 << 20 // 1 MiB
    }
    return cfg.Defaults.MaxBodyBytes
}

func rateLimit(cfg *config.File) httpx.RateLimiterConfig {
    if cfg.Runtime.RateLimit == nil {
        return httpx.RateLimiterConfig{RPS: 50, Burst: 100}
    }
    return httpx.RateLimiterConfig{
        RPS:   cfg.Runtime.RateLimit.RPS,
        Burst: cfg.Runtime.RateLimit.Burst,
    }
}

func shutdownTimeout(cfg *config.File) time.Duration {
    if cfg.Runtime.ShutdownTimeout == "" {
        return 30 * time.Second
    }
    d, err := time.ParseDuration(cfg.Runtime.ShutdownTimeout)
    if err != nil {
        return 30 * time.Second
    }
    return d
}
```

## Why all the tiny helpers?

`tracingEndpoint`, `pickKubeconfig`, `idempotencyTTL` and friends are
defensive against partial config. The HCL2 decoder enforces required
fields (everything tagged without `,optional`), but the *optional*
fields can still be nil/empty — better to centralize the defaults at
the consumer than scatter `if cfg.Runtime.Tracing == nil` checks
across the codebase.

## Why the `,nolint:contextcheck` annotations?

Production webhookd's CLAUDE.md flags `contextcheck` as a recurring
gotcha: shutdown paths intentionally use `context.WithTimeout(context.Background(), …)`
because the parent ctx is signal-cancelled. The cleanest fix is to
extract the helper (`drainServers`, `shutdownTracer`) and `nolint` at
the call site with a comment explaining the reason. The demo follows
the same pattern.

## Build + run

```bash
go build -o build/webhookd-demo ./cmd/webhookd-demo
WEBHOOK_DEMO_SECRET=topsecret \
KUBECONFIG=$HOME/.kube/config \
./build/webhookd-demo --config webhookd.hcl
# {"time":"...","level":"INFO","msg":"startup","version":"dev","commit":"unknown","instances":1}
# {"time":"...","level":"INFO","msg":"listening","name":"public","addr":":8080"}
# {"time":"...","level":"INFO","msg":"listening","name":"admin","addr":":9090"}
```

`Ctrl-C` triggers graceful shutdown:

```
{"time":"...","level":"INFO","msg":"shutdown signal received"}
{"time":"...","level":"INFO","msg":"shutdown complete"}
```

## What we proved

- [x] Single `realMain()` function with linear top-to-bottom flow
- [x] Anonymous integration imports drive `init()`-based registration
- [x] K8s backend's two-step (placeholder + Setup) pattern works
- [x] Two HTTP servers under one cancellation context
- [x] Graceful shutdown with detached drain timeouts

Next: [10-local-stack.md](10-local-stack.md) — bring up the
observability stack via `docker compose up`.
