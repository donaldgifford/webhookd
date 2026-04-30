# 02. HCL2 Configuration

The whole architecture hinges on the config format being able to express
"this instance pairs Provider X with Backend Y, and here are X's typed
config fields and Y's typed config fields." HCL2 with `gohcl.DecodeBody`
gives us this in one decode step (ADR-0009).

The trick: the top-level loader decodes the *skeleton* (instances,
provider/backend block labels, raw bodies). Each integration's
`DecodeConfig` method later partial-decodes its own typed struct from
the raw body. **One Go struct + `hcl:""` tags per integration *is* the
schema.** No JSON Schema files. No discriminator round-trips.

## The example config

This is what an operator writes. Drop it next to the binary as
`webhookd.hcl`:

### `webhookd.hcl`

```hcl
# Demo config: one JSM-to-K8s instance.

defaults {
  idempotency_ttl = "5m"
  max_body_bytes  = 1048576
}

runtime {
  addr             = ":8080"
  admin_addr       = ":9090"
  shutdown_timeout = "30s"

  rate_limit {
    rps   = 50
    burst = 100
  }

  tracing {
    enabled  = true
    endpoint = "localhost:4317"
    service  = "webhookd-demo"
  }
}

instance "demo-tenant-a" {
  provider "jsm" {
    trigger_status = "Approved"

    fields {
      provider_group_id = "customfield_10001"
      role              = "customfield_10002"
      project           = "customfield_10003"
    }

    signing {
      secret_env       = "WEBHOOK_DEMO_SECRET"
      signature_header = "X-Hub-Signature-256"
      timestamp_header = "X-Webhook-Timestamp"
      skew             = "5m"
    }
  }

  backend "k8s" {
    kubeconfig_env       = "KUBECONFIG"
    namespace            = "wiz-operator"
    identity_provider_id = "saml-idp-abc123"
    sync_timeout         = "20s"
  }
}
```

The `provider.fields` map JSM custom-field IDs to the SAMLGroupMapping
spec fields the K8s backend builds:

| HCL field | Source | Maps to |
|-----------|--------|---------|
| `provider_group_id` | JSM custom field | `spec.providerGroupId` |
| `role`              | JSM custom field | `spec.roleRef.name` |
| `project`           | JSM custom field | `spec.projectRefs[0].name` |

The `backend.identity_provider_id` is **per-instance** — one IDP per
JSM tenant — and populates `spec.identityProviderId` directly. It
doesn't come from the JSM payload.

The block syntax mirrors Terraform: `instance "<id>" { provider "<type>" { … } }`.
The HCL parser uses block labels (`"demo-tenant-a"`, `"jsm"`, `"k8s"`)
to drive the decoder.

## The top-level loader

Two structs — `File` (root) and the four blocks underneath. Provider
and Backend bodies are captured as raw `hcl.Body` for later partial
decode by their owning integrations.

### `internal/config/config.go`

```go
// Package config decodes webhookd-demo's HCL2 config file into typed
// structs. The top-level decoder grabs the skeleton; each integration
// later decodes its provider/backend block via DecodeConfig.
package config

import (
    "fmt"
    "os"

    "github.com/hashicorp/hcl/v2"
    "github.com/hashicorp/hcl/v2/gohcl"
    "github.com/hashicorp/hcl/v2/hclparse"
)

// File is the top-level HCL document.
type File struct {
    Defaults  *DefaultsBlock  `hcl:"defaults,block"`
    Runtime   *RuntimeBlock   `hcl:"runtime,block"`
    Instances []InstanceBlock `hcl:"instance,block"`
}

// DefaultsBlock applies cross-instance fallbacks.
type DefaultsBlock struct {
    IdempotencyTTL string `hcl:"idempotency_ttl,optional"`
    MaxBodyBytes   int64  `hcl:"max_body_bytes,optional"`
}

// RuntimeBlock configures the process: listen addrs, rate limits,
// tracing.
type RuntimeBlock struct {
    Addr            string        `hcl:"addr"`
    AdminAddr       string        `hcl:"admin_addr"`
    ShutdownTimeout string        `hcl:"shutdown_timeout,optional"`
    RateLimit       *RateLimit    `hcl:"rate_limit,block"`
    Tracing         *TracingBlock `hcl:"tracing,block"`
}

// RateLimit is a token-bucket spec.
type RateLimit struct {
    RPS   int `hcl:"rps"`
    Burst int `hcl:"burst"`
}

// TracingBlock configures OTel.
type TracingBlock struct {
    Enabled  bool   `hcl:"enabled"`
    Endpoint string `hcl:"endpoint,optional"`
    Service  string `hcl:"service,optional"`
}

// InstanceBlock pairs one Provider with one Backend.
type InstanceBlock struct {
    ID       string        `hcl:"id,label"`
    Provider ProviderBlock `hcl:"provider,block"`
    Backend  BackendBlock  `hcl:"backend,block"`
}

// ProviderBlock holds the provider type label and the rest of the
// block body for partial decode by the provider package.
type ProviderBlock struct {
    Type string   `hcl:"type,label"`
    Body hcl.Body `hcl:",remain"`
}

// BackendBlock mirrors ProviderBlock for the backend side.
type BackendBlock struct {
    Type string   `hcl:"type,label"`
    Body hcl.Body `hcl:",remain"`
}

// Load parses an HCL file (or directory) and decodes the skeleton.
// Provider/Backend bodies remain undecoded — the dispatcher invokes
// their owning integration's DecodeConfig later.
func Load(path string) (*File, hcl.Diagnostics) {
    parser := hclparse.NewParser()

    info, err := os.Stat(path)
    if err != nil {
        return nil, hcl.Diagnostics{{
            Severity: hcl.DiagError,
            Summary:  "config not found",
            Detail:   err.Error(),
        }}
    }

    var f *hcl.File
    var diags hcl.Diagnostics

    if info.IsDir() {
        // Directory loading: parse every *.hcl, merge into one Body.
        // Out of scope for the demo (single file is enough). The
        // hclparse.Parser supports this natively — see ADR-0009.
        return nil, hcl.Diagnostics{{
            Severity: hcl.DiagError,
            Summary:  "directory loading not implemented in demo",
            Detail:   "use a single .hcl file",
        }}
    }

    f, diags = parser.ParseHCLFile(path)
    if diags.HasErrors() {
        return nil, diags
    }

    var cfg File
    if d := gohcl.DecodeBody(f.Body, nil, &cfg); d.HasErrors() {
        return nil, d
    }

    if err := validate(&cfg); err != nil {
        return nil, hcl.Diagnostics{{
            Severity: hcl.DiagError,
            Summary:  "config validation",
            Detail:   err.Error(),
        }}
    }

    return &cfg, nil
}

// validate enforces invariants the HCL grammar can't express.
func validate(cfg *File) error {
    if cfg.Runtime == nil {
        return fmt.Errorf("runtime block is required")
    }
    if len(cfg.Instances) == 0 {
        return fmt.Errorf("at least one instance block is required")
    }

    seen := make(map[string]struct{}, len(cfg.Instances))
    for _, inst := range cfg.Instances {
        if inst.ID == "" {
            return fmt.Errorf("instance id cannot be empty")
        }
        if _, dup := seen[inst.ID]; dup {
            return fmt.Errorf("duplicate instance id %q", inst.ID)
        }
        seen[inst.ID] = struct{}{}

        if inst.Provider.Type == "" {
            return fmt.Errorf("instance %q: provider type cannot be empty", inst.ID)
        }
        if inst.Backend.Type == "" {
            return fmt.Errorf("instance %q: backend type cannot be empty", inst.ID)
        }
    }
    return nil
}
```

## Why the partial-decode trick is the win

Look at `ProviderBlock`:

```go
type ProviderBlock struct {
    Type string   `hcl:"type,label"`
    Body hcl.Body `hcl:",remain"`
}
```

The `,remain` tag tells `gohcl.DecodeBody` to **leave the rest of the
block undecoded** and stash the raw `hcl.Body` for someone else to
decode later. That someone else is the JSM provider's `DecodeConfig`,
which we'll write in [04-jsm-provider.md](04-jsm-provider.md):

```go
// in internal/integrations/jsm/config.go (preview)
func (p *Provider) DecodeConfig(body hcl.Body, ctx *hcl.EvalContext) (webhook.ProviderConfig, hcl.Diagnostics) {
    var cfg Config
    diags := gohcl.DecodeBody(body, ctx, &cfg)
    return cfg, diags
}
```

That `Config` struct has `hcl:""` tags for `trigger_status`, `fields`,
`signing`, etc. — all the JSM-specific fields. The top-level loader
never has to know about them. **The integration owns its own schema.**

If you swap JSM for GitHub, you swap the `Config` struct. The top-level
loader code never changes. That's the architectural payoff.

## Time/duration handling

HCL2 doesn't natively parse Go `time.Duration`. The demo treats every
duration as a string (`"5m"`, `"30s"`) and parses with `time.ParseDuration`
at the consumer. Production code can centralize this in a helper —
not worth the abstraction for the demo.

## Verify

Drop the example `webhookd.hcl` next to your project and add a quick
load test in `cmd/webhookd-demo/main.go` (we'll throw it away in
phase 9):

```go
package main

import (
    "fmt"
    "os"

    "github.com/example/webhookd-demo/internal/config"
)

func main() {
    cfg, diags := config.Load("webhookd.hcl")
    if diags.HasErrors() {
        fmt.Fprintln(os.Stderr, diags.Error())
        os.Exit(1)
    }
    fmt.Printf("loaded %d instance(s)\n", len(cfg.Instances))
    for _, inst := range cfg.Instances {
        fmt.Printf("  %s: %s -> %s\n", inst.ID, inst.Provider.Type, inst.Backend.Type)
    }
}
```

```bash
go run ./cmd/webhookd-demo
# loaded 1 instance(s)
#   demo-tenant-a: jsm -> k8s
```

If the file's missing or malformed, you'll get HCL2's diagnostics with
file:line:col precision:

```
webhookd.hcl:18,21-25: Missing required argument; The argument "rps" is required, but no definition was found.
```

## What we proved

- [x] HCL2 file parses into the typed `File` struct
- [x] Provider/Backend bodies are deferred via `,remain` for partial decode
- [x] Validation catches missing instance IDs and duplicates

Next: [03-registry.md](03-registry.md) — the `Provider` / `Backend`
interfaces these blocks bind to.
