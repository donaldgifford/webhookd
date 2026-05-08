# 07. HTTP Framework

Three concerns:

- A **public server** on `:8080` that runs the dispatcher routes.
- An **admin server** on `:9090` that exposes `/metrics`, `/healthz`,
  `/readyz`, and (optionally) `/debug/pprof`.
- Common middleware: per-request context, request-ID, body limit,
  rate limiting, in-flight gauge.

stdlib only — `net/http` + Go 1.22+ `ServeMux` is enough. No router
library.

## Files in this phase

```
internal/httpx/
├── admin.go          # admin mux + handlers
├── server.go         # public server constructor
├── middleware.go     # context, request-id, body limit, in-flight
└── ratelimit.go      # per-provider token bucket
```

## Admin server

The admin server is a separate listener so probes and metrics scrape
isolation never compete with webhook traffic. `/debug/pprof` is gated
behind an env var (`WEBHOOK_PPROF_ENABLED=true`) — production webhookd's
ADR-0002 footnote.

### `internal/httpx/admin.go`

```go
// Package httpx wires up the public + admin HTTP servers, common
// middleware, and the per-provider rate limiter.
package httpx

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/pprof"
    "os"
    "strconv"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

// AdminConfig governs the admin listener.
type AdminConfig struct {
    Addr     string
    Registry *prometheus.Registry
    // Ready is called by /readyz; should return nil when the process
    // is willing to take traffic.
    Ready func(ctx context.Context) error
}

// NewAdminServer builds an *http.Server that serves /metrics, /healthz,
// /readyz, and (when WEBHOOK_PPROF_ENABLED=true) /debug/pprof.
func NewAdminServer(cfg AdminConfig) *http.Server {
    mux := http.NewServeMux()

    mux.Handle("GET /metrics", promhttp.HandlerFor(cfg.Registry, promhttp.HandlerOpts{}))
    mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
        writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
    })
    mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
        if cfg.Ready != nil {
            if err := cfg.Ready(r.Context()); err != nil {
                writeJSON(w, http.StatusServiceUnavailable,
                    map[string]string{"status": "not ready", "reason": err.Error()})
                return
            }
        }
        writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
    })

    if pprofEnabled() {
        // Standard pprof endpoints — Go's http/pprof package wires
        // these to the DefaultServeMux; we have to re-register.
        mux.HandleFunc("/debug/pprof/", pprof.Index)
        mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
        mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
        mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
        mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
    }

    return &http.Server{
        Addr:              cfg.Addr,
        Handler:           mux,
        ReadHeaderTimeout: 10 * time.Second,
    }
}

func pprofEnabled() bool {
    v, _ := strconv.ParseBool(os.Getenv("WEBHOOK_PPROF_ENABLED"))
    return v
}

func writeJSON(w http.ResponseWriter, status int, body any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    if err := json.NewEncoder(w).Encode(body); err != nil {
        // Best-effort — the client may have hung up.
        _ = err
    }
}
```

## Public server

The public server takes a `http.Handler` (the dispatcher mux from
phase 8) and a small `Config` for listen + timeout knobs.

### `internal/httpx/server.go`

```go
package httpx

import (
    "net/http"
    "time"
)

// ServerConfig governs the public listener.
type ServerConfig struct {
    Addr              string
    ReadHeaderTimeout time.Duration
    ReadTimeout       time.Duration
    WriteTimeout      time.Duration
    IdleTimeout       time.Duration
}

// DefaultServerConfig returns conservative timeouts that are sane
// defaults for webhookd-style workloads.
func DefaultServerConfig(addr string) ServerConfig {
    return ServerConfig{
        Addr:              addr,
        ReadHeaderTimeout: 10 * time.Second,
        ReadTimeout:       30 * time.Second,
        WriteTimeout:      45 * time.Second,
        IdleTimeout:       120 * time.Second,
    }
}

// NewServer constructs the public *http.Server with the given handler.
// Wrap the handler with WithMiddleware before passing it in.
func NewServer(cfg ServerConfig, h http.Handler) *http.Server {
    return &http.Server{
        Addr:              cfg.Addr,
        Handler:           h,
        ReadHeaderTimeout: cfg.ReadHeaderTimeout,
        ReadTimeout:       cfg.ReadTimeout,
        WriteTimeout:      cfg.WriteTimeout,
        IdleTimeout:       cfg.IdleTimeout,
    }
}
```

## Middleware

A small, composable middleware stack. Each function takes an
`http.Handler` and returns one — the conventional Go pattern.

### `internal/httpx/middleware.go`

```go
package httpx

import (
    "context"
    "crypto/rand"
    "encoding/hex"
    "io"
    "net/http"
    "strings"
    "sync/atomic"
    "time"

    "github.com/prometheus/client_golang/prometheus"
)

type ctxKey int

const (
    ctxRequestID ctxKey = iota + 1
    ctxStartTime
)

// RequestID returns the request ID stamped by WithRequestID, or "".
func RequestID(ctx context.Context) string {
    v, _ := ctx.Value(ctxRequestID).(string)
    return v
}

// StartTime returns the wall-clock instant at which the request entered
// the middleware chain, or zero.
func StartTime(ctx context.Context) time.Time {
    v, _ := ctx.Value(ctxStartTime).(time.Time)
    return v
}

// WithRequestID assigns each request a 16-char hex ID and stamps it on
// both the response header (X-Request-ID) and the context.
func WithRequestID(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        id := r.Header.Get("X-Request-ID")
        if id == "" {
            id = newRequestID()
        }
        w.Header().Set("X-Request-ID", id)
        ctx := context.WithValue(r.Context(), ctxRequestID, id)
        ctx = context.WithValue(ctx, ctxStartTime, time.Now())
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

func newRequestID() string {
    var b [8]byte
    _, _ = rand.Read(b[:])
    return hex.EncodeToString(b[:])
}

// MaxBodyBytes wraps r.Body in a http.MaxBytesReader.
func MaxBodyBytes(limit int64) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            r.Body = http.MaxBytesReader(w, r.Body, limit)
            next.ServeHTTP(w, r)
        })
    }
}

// WithInFlight increments/decrements a Prometheus gauge for the
// duration of each request.
func WithInFlight(g prometheus.Gauge) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            g.Inc()
            defer g.Dec()
            next.ServeHTTP(w, r)
        })
    }
}

// ReadAll reads up to limit bytes from r and returns the byte slice.
// Returns an error wrapping http.MaxBytesError when the limit is hit.
//
// Exists as a helper because handlers that need both the body bytes
// (for HMAC) and a re-readable Request body have to drain it themselves.
func ReadAll(r io.Reader) ([]byte, error) {
    return io.ReadAll(r)
}

// providerFromPath extracts the leading path segment (the provider
// type) from request paths shaped like /<provider>/<webhook_id>.
//
// Useful inside middleware that runs *before* the mux populates
// r.PathValue("provider") — Go 1.22+ ServeMux only fills path values
// after routing, so the rate-limit middleware (which runs first)
// has to parse the URL itself.
func providerFromPath(p string) string {
    p = strings.TrimPrefix(p, "/")
    if i := strings.Index(p, "/"); i > 0 {
        return p[:i]
    }
    return p
}

// hits is a tiny atomic counter we use in some tests to confirm a
// middleware fired (kept in source for the curious — production would
// drop it).
type hits struct{ n atomic.Int64 }

func (h *hits) inc() { h.n.Add(1) }
```

## Per-provider rate limiter

Token bucket per provider type, lazy-initialized via `sync.Map.LoadOrStore`.
Provider set is bounded by config so no GC needed.

### `internal/httpx/ratelimit.go`

```go
package httpx

import (
    "net/http"
    "sync"

    "github.com/prometheus/client_golang/prometheus"
    "golang.org/x/time/rate"
)

// RateLimiterConfig is the per-process token-bucket spec, applied
// per-provider via LoadOrStore.
type RateLimiterConfig struct {
    RPS   int
    Burst int
}

// NewRateLimiter returns middleware that enforces RateLimiterConfig
// per provider type (extracted from the URL path). Drops are recorded
// against the provided counter.
func NewRateLimiter(cfg RateLimiterConfig, drops *prometheus.CounterVec) func(http.Handler) http.Handler {
    var buckets sync.Map // map[string]*rate.Limiter

    get := func(provider string) *rate.Limiter {
        if v, ok := buckets.Load(provider); ok {
            return v.(*rate.Limiter)
        }
        l := rate.NewLimiter(rate.Limit(cfg.RPS), cfg.Burst)
        actual, _ := buckets.LoadOrStore(provider, l)
        return actual.(*rate.Limiter)
    }

    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            provider := providerFromPath(r.URL.Path)
            if provider == "" {
                next.ServeHTTP(w, r)
                return
            }
            if !get(provider).Allow() {
                if drops != nil {
                    drops.WithLabelValues(provider).Inc()
                }
                http.Error(w, "rate limited", http.StatusTooManyRequests)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

> **Why not `rate.Limiter.Wait`?** `Wait` blocks the request until the
> next token is available — fine for low RPS, but during a burst it
> stacks goroutines and degrades latency for everyone. `Allow()` +
> immediate 429 is more honest: the caller learns about backpressure
> and can decide what to do.

## Building the middleware chain

`main.go` will compose them in order — outermost first, innermost last:

```go
chain := func(h http.Handler) http.Handler {
    h = httpx.MaxBodyBytes(maxBody)(h)
    h = httpx.NewRateLimiter(rlCfg, m.RateLimitDrops)(h)
    h = httpx.WithInFlight(m.HTTPInFlight)(h)
    h = httpx.WithRequestID(h)
    return h
}
```

The order matters:

1. `WithRequestID` runs first so every downstream layer sees the ID
2. `WithInFlight` increments the gauge once we've decided to handle
3. Rate limit happens before body read so we don't spin up bytes for
   requests we'll drop
4. Body limit is innermost — only enforced once the request makes it
   to the dispatcher

## What we proved

- [x] Two listeners (public, admin) on independent addresses
- [x] `/metrics`, `/healthz`, `/readyz` plus optional pprof
- [x] Middleware composes into a clean stack
- [x] Per-provider token bucket without GC complexity
- [x] Path-based provider extraction works *before* `ServeMux` populates
      `r.PathValue("provider")`

Next: [08-dispatcher.md](08-dispatcher.md) — what runs *inside* this
HTTP shell.
