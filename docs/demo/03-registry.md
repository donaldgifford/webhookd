# 03. Registry & Interfaces

The contracts every integration plugs into. `Provider` and `Backend`
are the two interfaces; `BackendRequest` is the open-typed object that
flows from one to the other; `ExecResult` is what comes back. The
`Registry` holds the type→implementation map and is populated at
package-init time by integration packages (ADR-0010).

## The interfaces

### `internal/webhook/types.go`

```go
// Package webhook holds the dispatcher contracts: Provider, Backend,
// BackendRequest, ExecResult, and the Registry that maps config strings
// to concrete implementations.
package webhook

import (
    "context"
    "net/http"

    "github.com/hashicorp/hcl/v2"
)

// ProviderConfig is the typed config struct an integration's
// DecodeConfig returns. The dispatcher carries it opaquely; only the
// owning Provider type-asserts it back.
type ProviderConfig any

// BackendConfig mirrors ProviderConfig on the Backend side.
type BackendConfig any

// BackendRequest is the open contract between a Provider and a Backend.
// Every concrete request type implements it and the receiving Backend
// type-asserts to the type it knows how to handle.
//
// Example: jsm.Provider produces a *jsm.SAMLGroupMappingRequest; the
// k8sbackend.Backend asserts to that type and applies the embedded
// SAMLGroupMapping CR.
type BackendRequest interface {
    // Kind returns a human-readable string for logging/metrics
    // (e.g. "wiz.SAMLGroupMapping"). Not used for routing.
    Kind() string
}

// Provider is one webhook source — JSM, GitHub, Slack, etc.
//
// Provider methods MUST be pure functions: byte-in, value-out, no I/O.
// Side effects belong in the Backend. This split is what keeps
// providers trivially unit-testable (no envtest, no HTTP fixtures).
type Provider interface {
    // Type returns the config block label, e.g. "jsm".
    Type() string

    // DecodeConfig partial-decodes the provider's HCL block body into
    // the integration's typed Config struct.
    DecodeConfig(body hcl.Body, ctx *hcl.EvalContext) (ProviderConfig, hcl.Diagnostics)

    // VerifySignature authenticates the request. Returns nil on success,
    // a typed error on failure (the dispatcher maps to 401/4xx).
    VerifySignature(r *http.Request, body []byte, cfg ProviderConfig) error

    // IdempotencyKey extracts a stable key from the request. The
    // dispatcher uses this to dedupe retries within the TTL window.
    // Returning ("", nil) opts out of idempotency for this request.
    IdempotencyKey(r *http.Request, body []byte) (string, error)

    // Handle parses bytes into a typed BackendRequest.
    // Pure: no I/O, no logging, no metrics. Errors are mapped to 4xx.
    Handle(ctx context.Context, body []byte, cfg ProviderConfig) (BackendRequest, error)

    // BuildResponse shapes the synchronous HTTP response body from the
    // Backend's ExecResult. Different providers may want different
    // shapes — JSM returns {"status": "..."}; GitHub returns 204; etc.
    BuildResponse(res ExecResult, traceID, requestID string) any
}

// Backend is one execution target — Kubernetes, AWS EventBridge, HTTP, …
//
// Backend.Execute is allowed to do I/O (it must, by definition).
// It returns a typed ExecResult; the dispatcher never inspects the
// underlying error directly — only the kind/HTTP status.
type Backend interface {
    Type() string
    DecodeConfig(body hcl.Body, ctx *hcl.EvalContext) (BackendConfig, hcl.Diagnostics)
    Execute(ctx context.Context, req BackendRequest, cfg BackendConfig) ExecResult
}

// ResultKind classifies the outcome of a Backend.Execute call.
type ResultKind int

const (
    // ResultUnknown is the zero value; never return it intentionally.
    ResultUnknown ResultKind = iota
    // ResultSuccess: the side effect completed and was observed.
    ResultSuccess
    // ResultNoOp: the request was valid but produced no work
    // (e.g. JSM trigger_status didn't match — provider returns this
    // before Backend.Execute is called).
    ResultNoOp
    // ResultClientError: the request was malformed or referenced
    // missing prerequisites (4xx).
    ResultClientError
    // ResultServerError: the Backend hit an internal failure (5xx).
    ResultServerError
    // ResultTimeout: the synchronous wait exceeded the configured
    // budget. Dispatcher maps to 504.
    ResultTimeout
)

// ExecResult is what every Backend.Execute returns.
type ExecResult struct {
    Kind       ResultKind
    HTTPStatus int    // suggested HTTP status code (200, 422, 504, etc.)
    Reason     string // short machine-readable code (e.g. "TimedOut")
    Detail     string // human-friendly detail for logs/responses
    Err        error  // optional underlying error for log enrichment
}
```

A few design choices worth flagging:

- **`BackendRequest` is an interface, not a sentinel union.** Each
  Backend type-asserts to its expected concrete type. New
  Provider × Backend pairings need no changes to the dispatcher.
- **`ProviderConfig` and `BackendConfig` are `any`.** The integration's
  Config struct is the schema; the dispatcher never inspects it.
- **`ExecResult.HTTPStatus` is a *suggestion*.** The dispatcher
  ultimately writes the response; providers can override via
  `BuildResponse` (e.g. JSM returns 200 even on a no-op so Jira
  retries don't fire).

## The registry

### `internal/webhook/registry.go`

```go
package webhook

import (
    "fmt"
    "sync"
)

// Registry maps type-name strings (e.g. "jsm", "k8s") to concrete
// Provider/Backend implementations. The default global registry is
// populated at package-init time by integration packages.
type Registry struct {
    mu        sync.RWMutex
    providers map[string]Provider
    backends  map[string]Backend
}

// NewRegistry constructs an isolated registry. Tests use this to
// avoid the global state populated by init() functions.
func NewRegistry() *Registry {
    return &Registry{
        providers: make(map[string]Provider),
        backends:  make(map[string]Backend),
    }
}

// RegisterProvider adds p to the registry, panicking on duplicate
// Type(). A duplicate is a programming error — preferable to silently
// routing to whichever package was imported last.
func (r *Registry) RegisterProvider(p Provider) {
    r.mu.Lock()
    defer r.mu.Unlock()

    if _, dup := r.providers[p.Type()]; dup {
        panic(fmt.Sprintf("webhook: duplicate provider type %q", p.Type()))
    }
    r.providers[p.Type()] = p
}

// RegisterBackend adds b to the registry. Same panic-on-duplicate rule.
func (r *Registry) RegisterBackend(b Backend) {
    r.mu.Lock()
    defer r.mu.Unlock()

    if _, dup := r.backends[b.Type()]; dup {
        panic(fmt.Sprintf("webhook: duplicate backend type %q", b.Type()))
    }
    r.backends[b.Type()] = b
}

// Provider returns the Provider registered under name and a found bool.
func (r *Registry) Provider(name string) (Provider, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    p, ok := r.providers[name]
    return p, ok
}

// Backend returns the Backend registered under name and a found bool.
func (r *Registry) Backend(name string) (Backend, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    b, ok := r.backends[name]
    return b, ok
}

// Default is the package-global registry that integration init()
// functions populate. Production main.go reads from Default;
// tests construct fresh registries via NewRegistry().
var Default = NewRegistry()

// RegisterProvider and RegisterBackend are package-level shortcuts to
// Default — what integration init() functions actually call.
func RegisterProvider(p Provider) { Default.RegisterProvider(p) }
func RegisterBackend(b Backend)   { Default.RegisterBackend(b) }
```

## Why `init()`-based registration?

This is ADR-0010. The trade-off:

- **`init()`** — every integration registers itself when its package is
  imported. Adding an integration is one anonymous import in `main.go`:
  ```go
  import _ "github.com/example/webhookd-demo/internal/integrations/jsm"
  ```
  Cheap, idiomatic Go (mirrors `database/sql` driver registration).
  Downside: package side effects on import.

- **Explicit `Register(reg)` from `main`** — slightly cleaner, more
  testable. The demo supports both: `init()` populates `Default`;
  tests call `webhook.NewRegistry()` and register only what they need.

Either works. The demo defaults to `init()` for production wiring.

## Wire format for `webhook.Verify` (preview)

The HMAC-SHA256 signature primitive lives in `internal/webhook/signature.go`.
It's a one-function helper that providers reuse:

### `internal/webhook/signature.go`

```go
package webhook

import (
    "crypto/hmac"
    "crypto/sha256"
    "crypto/subtle"
    "encoding/hex"
    "errors"
    "fmt"
    "strings"
    "time"
)

var (
    // ErrInvalidSignature indicates the signature header was missing,
    // malformed, or did not match the computed HMAC.
    ErrInvalidSignature = errors.New("invalid signature")
    // ErrStaleTimestamp indicates the request timestamp fell outside
    // the configured skew window.
    ErrStaleTimestamp = errors.New("stale timestamp")
)

// Verify validates an HMAC-SHA256 signature with optional replay
// protection via a timestamp + skew window.
//
// Headers ("sha256=" prefix is supported and stripped):
//   X-Hub-Signature-256: sha256=<hex>
//   X-Webhook-Timestamp: <RFC3339> (optional)
//
// If timestamp is empty, replay protection is skipped.
func Verify(body, secret []byte, signature, timestamp string, skew time.Duration, now time.Time) error {
    if signature == "" {
        return fmt.Errorf("%w: missing signature", ErrInvalidSignature)
    }
    sig := strings.TrimPrefix(signature, "sha256=")
    sigBytes, err := hex.DecodeString(sig)
    if err != nil {
        return fmt.Errorf("%w: malformed hex: %v", ErrInvalidSignature, err)
    }

    mac := hmac.New(sha256.New, secret)
    mac.Write(body)
    expected := mac.Sum(nil)

    if subtle.ConstantTimeCompare(sigBytes, expected) != 1 {
        return ErrInvalidSignature
    }

    if timestamp == "" {
        return nil
    }
    ts, err := time.Parse(time.RFC3339, timestamp)
    if err != nil {
        return fmt.Errorf("%w: malformed timestamp: %v", ErrStaleTimestamp, err)
    }
    if d := now.Sub(ts); d < -skew || d > skew {
        return fmt.Errorf("%w: %s outside %s window", ErrStaleTimestamp, ts, skew)
    }
    return nil
}
```

The provider's `VerifySignature` is a one-liner around this:

```go
// preview from internal/integrations/jsm/provider.go
func (p *Provider) VerifySignature(r *http.Request, body []byte, cfg webhook.ProviderConfig) error {
    c := cfg.(Config)
    secret := os.Getenv(c.Signing.SecretEnv)
    return webhook.Verify(
        body,
        []byte(secret),
        r.Header.Get(c.Signing.SignatureHeader),
        r.Header.Get(c.Signing.TimestampHeader),
        c.Signing.SkewDuration(),
        time.Now(),
    )
}
```

## What we proved

- [x] The contract surface is small: Provider, Backend, BackendRequest,
      ExecResult, Registry.
- [x] Adding an integration means implementing Provider or Backend and
      registering in `init()`. No dispatcher edits needed.
- [x] Test isolation works via `webhook.NewRegistry()`.
- [x] HMAC verification is one shared helper, not duplicated per provider.

Next: [04-jsm-provider.md](04-jsm-provider.md) — concrete Provider impl.
