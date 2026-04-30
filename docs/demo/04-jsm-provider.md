# 04. JSM Provider

The JSM provider takes a Jira Service Management webhook payload, verifies
its HMAC signature, parses the relevant fields, and emits a typed
`BackendRequest` for the K8s backend to apply. Pure functions —
no I/O — except for `VerifySignature`, which reads a secret from
the environment.

## Files in this phase

```
internal/integrations/jsm/
├── config.go      # HCL2 Config struct
├── decode.go      # JSON payload → typed model
├── request.go     # MappingRequest (the BackendRequest)
├── provider.go    # Provider interface impl
├── response.go    # BuildResponse shape
├── init.go        # Registers with the global registry
└── provider_test.go  # Example table-driven tests
```

## Config struct

The integration owns its schema. `gohcl.DecodeBody` validates and decodes
in one step; the Go struct + `hcl:""` tags is the wire format.

### `internal/integrations/jsm/config.go`

```go
// Package jsm implements the JSM Provider — Jira Service Management
// webhook payloads in, BackendRequest out.
package jsm

import (
    "fmt"
    "time"

    "github.com/hashicorp/hcl/v2"
    "github.com/hashicorp/hcl/v2/gohcl"

    "github.com/example/webhookd-demo/internal/webhook"
)

// Config is the typed shape of the `provider "jsm" { ... }` HCL block.
type Config struct {
    TriggerStatus string  `hcl:"trigger_status"`
    Fields        Fields  `hcl:"fields,block"`
    Signing       Signing `hcl:"signing,block"`
}

// Fields maps the tenant's Jira custom-field IDs to the demo's
// internal field names. Different tenants use different custom-field
// IDs for the same logical field, which is why this is per-instance.
type Fields struct {
    IdentityProviderID string `hcl:"identity_provider_id"`
    Role               string `hcl:"role"`
    Project            string `hcl:"project"`
}

// Signing configures HMAC verification.
type Signing struct {
    SecretEnv       string `hcl:"secret_env"`
    SignatureHeader string `hcl:"signature_header,optional"`
    TimestampHeader string `hcl:"timestamp_header,optional"`
    Skew            string `hcl:"skew,optional"`
}

// SkewDuration returns Signing.Skew parsed as a duration, defaulting
// to 5m when unset or unparseable.
func (s Signing) SkewDuration() time.Duration {
    if s.Skew == "" {
        return 5 * time.Minute
    }
    d, err := time.ParseDuration(s.Skew)
    if err != nil {
        return 5 * time.Minute
    }
    return d
}

// decodeConfig is the partial decoder the Provider's DecodeConfig
// method delegates to.
func decodeConfig(body hcl.Body, ctx *hcl.EvalContext) (Config, hcl.Diagnostics) {
    var cfg Config
    diags := gohcl.DecodeBody(body, ctx, &cfg)
    if diags.HasErrors() {
        return Config{}, diags
    }
    if err := validateConfig(cfg); err != nil {
        return Config{}, hcl.Diagnostics{{
            Severity: hcl.DiagError,
            Summary:  "jsm config validation",
            Detail:   err.Error(),
        }}
    }
    return cfg, nil
}

func validateConfig(c Config) error {
    if c.TriggerStatus == "" {
        return fmt.Errorf("trigger_status is required")
    }
    if c.Fields.IdentityProviderID == "" {
        return fmt.Errorf("fields.identity_provider_id is required")
    }
    if c.Signing.SecretEnv == "" {
        return fmt.Errorf("signing.secret_env is required")
    }
    return nil
}

// Compile-time check that Config satisfies the dispatcher's expected
// shape (used as a documentation aid, not enforced at runtime).
var _ webhook.ProviderConfig = Config{}
```

## Decoding the JSM payload

JSM webhooks ship a deeply-nested Jira payload. The demo only cares
about a few fields: the issue key, the status, and three custom fields.
We use a thin typed decode rather than reaching into `map[string]any`.

### `internal/integrations/jsm/decode.go`

```go
package jsm

import (
    "encoding/json"
    "fmt"
)

// jsmPayload is the minimal shape of a JSM webhook body the demo cares
// about. Unrelated fields are ignored by the JSON decoder.
type jsmPayload struct {
    Issue struct {
        Key    string `json:"key"`
        Fields struct {
            Status struct {
                Name string `json:"name"`
            } `json:"status"`
            // Custom fields are tenant-configured. We unmarshal into
            // a generic map and pull the configured keys out below.
            Custom map[string]json.RawMessage `json:"-"`
        } `json:"fields"`
    } `json:"issue"`
}

// decodePayload extracts the typed JSM payload plus a flat
// custom-field map keyed by Jira's customfield_XXXXX identifier.
func decodePayload(body []byte) (jsmPayload, map[string]string, error) {
    var p jsmPayload
    if err := json.Unmarshal(body, &p); err != nil {
        return jsmPayload{}, nil, fmt.Errorf("decode payload: %w", err)
    }

    // Re-decode just `issue.fields` into a string-keyed map so we can
    // pull out tenant-configured custom-field IDs without each one
    // being a typed struct field.
    var raw struct {
        Issue struct {
            Fields map[string]json.RawMessage `json:"fields"`
        } `json:"issue"`
    }
    if err := json.Unmarshal(body, &raw); err != nil {
        return jsmPayload{}, nil, fmt.Errorf("decode fields: %w", err)
    }

    custom := make(map[string]string, len(raw.Issue.Fields))
    for k, v := range raw.Issue.Fields {
        var s string
        if err := json.Unmarshal(v, &s); err == nil {
            custom[k] = s
        }
        // Non-string values silently skipped; extractField handles missing.
    }
    return p, custom, nil
}

// extractField returns the string value for the configured custom-field
// ID. Returns ("", false) when missing or empty.
func extractField(custom map[string]string, id string) (string, bool) {
    v, ok := custom[id]
    if !ok || v == "" {
        return "", false
    }
    return v, true
}
```

## The BackendRequest

This is the typed object the Provider hands to the Backend. The K8s
backend will type-assert to `*MappingRequest` and apply the embedded
fields as a CR.

### `internal/integrations/jsm/request.go`

```go
package jsm

// MappingRequest is the BackendRequest that the JSM provider produces.
// Each field maps to a column on the demo CRD (WebhookMapping).
//
// The K8s backend type-asserts to *MappingRequest and uses these
// fields to build a server-side-applied CR.
type MappingRequest struct {
    // IssueKey is the Jira issue key (e.g. "ABC-123"). Used as the
    // CR's metadata.name (after lowercasing) and as a correlation
    // annotation for trace/log linking.
    IssueKey string

    // IdentityProviderID, Role, Project come straight off the JSM
    // custom fields configured on the instance.
    IdentityProviderID string
    Role               string
    Project            string

    // TraceID, if non-empty, is propagated as an annotation on the
    // applied CR (per ADR-0007) so downstream operators can link
    // their reconcile spans.
    TraceID string
}

// Kind implements webhook.BackendRequest.
func (r *MappingRequest) Kind() string { return "k8s.WebhookMapping" }
```

## The Provider implementation

Tying the pieces together. Note `Handle` is *pure* — no logging, no
metrics, no I/O. Errors are typed so the dispatcher can map to status
codes.

### `internal/integrations/jsm/provider.go`

```go
package jsm

import (
    "context"
    "errors"
    "fmt"
    "net/http"
    "os"
    "strings"
    "time"

    "github.com/hashicorp/hcl/v2"
    "go.opentelemetry.io/otel/trace"

    "github.com/example/webhookd-demo/internal/webhook"
)

// ErrTriggerStatusMismatch indicates the JSM payload's status doesn't
// match the configured trigger_status — the dispatcher returns 200
// with a no-op response, not a 4xx (Jira retries on 4xx, which we
// don't want for benign mismatches).
var ErrTriggerStatusMismatch = errors.New("trigger status mismatch")

// ErrMissingField indicates a required custom field was missing from
// the payload. Mapped to 422 by the dispatcher.
var ErrMissingField = errors.New("missing required field")

// Provider implements webhook.Provider.
type Provider struct{}

// New returns a Provider — the constructor exists for symmetry with
// production code; the type carries no state.
func New() *Provider { return &Provider{} }

// Type implements webhook.Provider.
func (p *Provider) Type() string { return "jsm" }

// DecodeConfig implements webhook.Provider.
func (p *Provider) DecodeConfig(body hcl.Body, ctx *hcl.EvalContext) (webhook.ProviderConfig, hcl.Diagnostics) {
    return decodeConfig(body, ctx)
}

// VerifySignature implements webhook.Provider.
func (p *Provider) VerifySignature(r *http.Request, body []byte, cfg webhook.ProviderConfig) error {
    c, ok := cfg.(Config)
    if !ok {
        return fmt.Errorf("jsm: unexpected config type %T", cfg)
    }
    secret := os.Getenv(c.Signing.SecretEnv)
    if secret == "" {
        return fmt.Errorf("jsm: signing secret %q not set", c.Signing.SecretEnv)
    }

    sigHdr := c.Signing.SignatureHeader
    if sigHdr == "" {
        sigHdr = "X-Hub-Signature-256"
    }
    tsHdr := c.Signing.TimestampHeader
    if tsHdr == "" {
        tsHdr = "X-Webhook-Timestamp"
    }

    return webhook.Verify(
        body,
        []byte(secret),
        r.Header.Get(sigHdr),
        r.Header.Get(tsHdr),
        c.Signing.SkewDuration(),
        time.Now(),
    )
}

// IdempotencyKey implements webhook.Provider. JSM doesn't ship a
// dedicated request ID, so we derive one from issue key + status —
// retries of the same status transition on the same issue dedupe.
func (p *Provider) IdempotencyKey(_ *http.Request, body []byte) (string, error) {
    payload, _, err := decodePayload(body)
    if err != nil {
        return "", err
    }
    if payload.Issue.Key == "" {
        return "", nil // opt out — no key, no dedupe
    }
    return fmt.Sprintf("jsm:%s:%s",
        payload.Issue.Key,
        payload.Issue.Fields.Status.Name,
    ), nil
}

// Handle implements webhook.Provider. Pure: bytes in, *MappingRequest
// or typed error out.
func (p *Provider) Handle(ctx context.Context, body []byte, cfg webhook.ProviderConfig) (webhook.BackendRequest, error) {
    c, ok := cfg.(Config)
    if !ok {
        return nil, fmt.Errorf("jsm: unexpected config type %T", cfg)
    }

    payload, custom, err := decodePayload(body)
    if err != nil {
        return nil, err
    }

    if payload.Issue.Fields.Status.Name != c.TriggerStatus {
        return nil, fmt.Errorf("%w: got %q, want %q",
            ErrTriggerStatusMismatch,
            payload.Issue.Fields.Status.Name,
            c.TriggerStatus,
        )
    }

    idp, ok := extractField(custom, c.Fields.IdentityProviderID)
    if !ok {
        return nil, fmt.Errorf("%w: identity_provider_id (%s)", ErrMissingField, c.Fields.IdentityProviderID)
    }
    role, ok := extractField(custom, c.Fields.Role)
    if !ok {
        return nil, fmt.Errorf("%w: role (%s)", ErrMissingField, c.Fields.Role)
    }
    project, ok := extractField(custom, c.Fields.Project)
    if !ok {
        return nil, fmt.Errorf("%w: project (%s)", ErrMissingField, c.Fields.Project)
    }

    // Propagate the active trace ID via the BackendRequest so the
    // K8s backend can stamp it as an annotation (ADR-0007).
    var traceID string
    if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
        traceID = sc.TraceID().String()
    }

    return &MappingRequest{
        IssueKey:           strings.ToLower(payload.Issue.Key),
        IdentityProviderID: idp,
        Role:               role,
        Project:            project,
        TraceID:            traceID,
    }, nil
}

// BuildResponse implements webhook.Provider.
func (p *Provider) BuildResponse(res webhook.ExecResult, traceID, requestID string) any {
    return buildResponse(res, traceID, requestID)
}
```

## Response shape

JSM-specific JSON shape — separate file because we want the same
shape in unit tests without spinning up the rest of the Provider.

### `internal/integrations/jsm/response.go`

```go
package jsm

import "github.com/example/webhookd-demo/internal/webhook"

// Response is the JSON body returned to JSM. The shape is intentionally
// boring: a status string + optional reason + correlation IDs.
type Response struct {
    Status    string `json:"status"`
    Reason    string `json:"reason,omitempty"`
    Detail    string `json:"detail,omitempty"`
    TraceID   string `json:"trace_id,omitempty"`
    RequestID string `json:"request_id,omitempty"`
}

func buildResponse(res webhook.ExecResult, traceID, requestID string) Response {
    out := Response{TraceID: traceID, RequestID: requestID}
    switch res.Kind {
    case webhook.ResultSuccess:
        out.Status = "success"
    case webhook.ResultNoOp:
        out.Status = "noop"
        out.Reason = res.Reason
    case webhook.ResultClientError:
        out.Status = "rejected"
        out.Reason = res.Reason
        out.Detail = res.Detail
    case webhook.ResultTimeout:
        out.Status = "timeout"
        out.Reason = res.Reason
    default:
        out.Status = "error"
        out.Reason = res.Reason
        out.Detail = res.Detail
    }
    return out
}
```

## Registration

The `init()` function adds the Provider to the global registry as soon
as the package is imported.

### `internal/integrations/jsm/init.go`

```go
package jsm

import "github.com/example/webhookd-demo/internal/webhook"

func init() {
    webhook.RegisterProvider(New())
}
```

`main.go` will pull this in via:

```go
import _ "github.com/example/webhookd-demo/internal/integrations/jsm"
```

## Example test

One table-driven test demonstrating the pattern. Apply the same
shape to the rest of your provider tests.

### `internal/integrations/jsm/provider_test.go`

```go
package jsm

import (
    "context"
    "errors"
    "testing"
)

func TestProvider_Handle(t *testing.T) {
    cfg := Config{
        TriggerStatus: "Approved",
        Fields: Fields{
            IdentityProviderID: "customfield_10001",
            Role:               "customfield_10002",
            Project:            "customfield_10003",
        },
    }

    tests := []struct {
        name      string
        body      string
        wantErr   error
        wantKey   string
        wantIDP   string
    }{
        {
            name: "happy path",
            body: `{
                "issue": {
                    "key": "ABC-123",
                    "fields": {
                        "status": {"name": "Approved"},
                        "customfield_10001": "wiz-tenant-a",
                        "customfield_10002": "Editor",
                        "customfield_10003": "demo-project"
                    }
                }
            }`,
            wantKey: "abc-123",
            wantIDP: "wiz-tenant-a",
        },
        {
            name: "trigger status mismatch becomes typed error",
            body: `{
                "issue": {
                    "key": "ABC-123",
                    "fields": {
                        "status": {"name": "InProgress"},
                        "customfield_10001": "x",
                        "customfield_10002": "y",
                        "customfield_10003": "z"
                    }
                }
            }`,
            wantErr: ErrTriggerStatusMismatch,
        },
        {
            name: "missing custom field becomes typed error",
            body: `{
                "issue": {
                    "key": "ABC-123",
                    "fields": {
                        "status": {"name": "Approved"},
                        "customfield_10002": "Editor",
                        "customfield_10003": "demo-project"
                    }
                }
            }`,
            wantErr: ErrMissingField,
        },
    }

    p := New()
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            req, err := p.Handle(context.Background(), []byte(tt.body), cfg)
            if tt.wantErr != nil {
                if !errors.Is(err, tt.wantErr) {
                    t.Fatalf("Handle err = %v, want %v", err, tt.wantErr)
                }
                return
            }
            if err != nil {
                t.Fatalf("Handle err = %v, want nil", err)
            }
            mr, ok := req.(*MappingRequest)
            if !ok {
                t.Fatalf("Handle returned %T, want *MappingRequest", req)
            }
            if mr.IssueKey != tt.wantKey {
                t.Errorf("IssueKey = %q, want %q", mr.IssueKey, tt.wantKey)
            }
            if mr.IdentityProviderID != tt.wantIDP {
                t.Errorf("IdentityProviderID = %q, want %q", mr.IdentityProviderID, tt.wantIDP)
            }
        })
    }
}
```

```bash
go test ./internal/integrations/jsm/...
# PASS
```

## What we proved

- [x] HCL config decodes into a typed `Config` per integration
- [x] `Handle` is pure — easy to unit-test with no fixtures
- [x] Signature verification is delegated to one shared helper
- [x] Provider registers itself via `init()` (ADR-0010)
- [x] Trigger-status mismatch is a typed error so the dispatcher can
      map it to a 200/no-op response (Jira retries on 4xx)

Next: [05-k8s-backend.md](05-k8s-backend.md) — the side-effect side.
