# 05. Kubernetes Backend

The K8s backend takes a `*jsm.SAMLGroupMappingRequest` (or any future
`BackendRequest` it learns to handle), Server-Side-Applies it as a
`SAMLGroupMapping` CR (`wiz.rtkwlf.io/v1alpha1`), and watches for
`Ready=True` before returning.

This is the side-effect side of the architecture. ADR-0005 says SSA;
ADR-0006 says synchronous response; ADR-0007 says trace-id propagation
via annotation.

## Files in this phase

```
internal/wizapi/v1alpha1/
├── groupversion_info.go
├── types.go
└── zz_generated.deepcopy.go        # handwritten for the demo

internal/k8s/
└── clients.go                       # scheme + controller-runtime client

internal/integrations/k8sbackend/
├── config.go
├── backend.go
├── apply.go
├── watch.go
└── init.go
```

## The Wiz CRD types

A handwritten typed model that mirrors the canonical
[`samlmapping_crd.yaml`](kustomize/crd.yaml). Production code generates
DeepCopy via `controller-gen`; for a demo, a handful of handwritten
DeepCopy methods is faster than configuring codegen.

> **Looking ahead:** when `github.com/donaldgifford/wiz-operator` is
> ready to import, the stub here can be deleted in favor of upstream
> types — see [14-upstream-types.md](14-upstream-types.md) for the
> swap recipe. The shape below mirrors upstream verbatim, so the
> rest of this demo doesn't need to change when you do.

### `internal/wizapi/v1alpha1/groupversion_info.go`

```go
// Package v1alpha1 holds the Wiz operator CRD types — mirror of
// what the operator publishes at wiz.rtkwlf.io/v1alpha1. Replace
// this stub with the real upstream module when it's importable.
package v1alpha1

import (
    "k8s.io/apimachinery/pkg/runtime/schema"
    "sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the schema.GroupVersion for this API.
var GroupVersion = schema.GroupVersion{
    Group:   "wiz.rtkwlf.io",
    Version: "v1alpha1",
}

// SchemeBuilder is used to add this group to a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the types in this group to the given scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
    SchemeBuilder.Register(&SAMLGroupMapping{}, &SAMLGroupMappingList{})
}
```

### `internal/wizapi/v1alpha1/types.go`

```go
package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime"
)

// ProjectReference identifies a Wiz project by K8s CR name or direct ID.
// Exactly one of Name or ProjectID should be set; if both are empty
// the operator may treat the entry as invalid.
type ProjectReference struct {
    Name      string `json:"name,omitempty"`
    ProjectID string `json:"projectId,omitempty"`
}

// RoleReference identifies a Wiz user role by K8s UserRole CR name
// or direct Wiz role ID.
type RoleReference struct {
    Name   string `json:"name,omitempty"`
    RoleID string `json:"roleId,omitempty"`
}

// SAMLGroupMappingSpec is the desired state — what the JSM provider
// produces and what the K8s backend SSA-applies.
type SAMLGroupMappingSpec struct {
    // IdentityProviderID is the Wiz SAML IDP this mapping belongs to.
    // Comes from the K8s backend's per-instance config (one IDP per
    // tenant), not from the JSM payload.
    IdentityProviderID string `json:"identityProviderId"`

    // ProviderGroupID is the Okta group ID from the JSM custom field.
    ProviderGroupID string `json:"providerGroupId"`

    // Description is a human-readable description (typically the
    // JSM issue summary).
    Description string `json:"description,omitempty"`

    // RoleRef references the Wiz user role to assign.
    RoleRef RoleReference `json:"roleRef"`

    // ProjectRefs is a list of project references scoping the mapping.
    // Empty means unscoped (per the Wiz operator's semantics).
    ProjectRefs []ProjectReference `json:"projectRefs,omitempty"`
}

// SAMLGroupMappingStatus is the observed state — what the operator
// writes back. The backend watches for `Ready=True` to consider the
// mapping synced.
type SAMLGroupMappingStatus struct {
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
    LastSyncTime       *metav1.Time       `json:"lastSyncTime,omitempty"`
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    SpecHash           string             `json:"specHash,omitempty"`
    WizResourceID      string             `json:"wizResourceId,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SAMLGroupMapping is the demo's target CR (mirror of
// wiz.rtkwlf.io/v1alpha1.SAMLGroupMapping).
type SAMLGroupMapping struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   SAMLGroupMappingSpec   `json:"spec,omitempty"`
    Status SAMLGroupMappingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SAMLGroupMappingList contains a list of SAMLGroupMapping.
type SAMLGroupMappingList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []SAMLGroupMapping `json:"items"`
}

// DeepCopyObject implements runtime.Object.
func (in *SAMLGroupMapping) DeepCopyObject() runtime.Object {
    return in.DeepCopy()
}

// DeepCopyObject implements runtime.Object.
func (in *SAMLGroupMappingList) DeepCopyObject() runtime.Object {
    return in.DeepCopy()
}
```

### `internal/wizapi/v1alpha1/zz_generated.deepcopy.go`

```go
// DeepCopy code is normally generated by controller-gen. Hand-rolled
// here to keep the demo dependency-light.
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// DeepCopyInto copies the receiver into out.
func (in *SAMLGroupMapping) DeepCopyInto(out *SAMLGroupMapping) {
    *out = *in
    out.TypeMeta = in.TypeMeta
    in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
    in.Spec.DeepCopyInto(&out.Spec)
    in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy of the receiver.
func (in *SAMLGroupMapping) DeepCopy() *SAMLGroupMapping {
    if in == nil {
        return nil
    }
    out := new(SAMLGroupMapping)
    in.DeepCopyInto(out)
    return out
}

// DeepCopyInto copies the spec, including ProjectRefs.
func (in *SAMLGroupMappingSpec) DeepCopyInto(out *SAMLGroupMappingSpec) {
    *out = *in
    out.RoleRef = in.RoleRef
    if in.ProjectRefs != nil {
        out.ProjectRefs = make([]ProjectReference, len(in.ProjectRefs))
        copy(out.ProjectRefs, in.ProjectRefs)
    }
}

// DeepCopyInto copies the status conditions slice element-by-element.
func (in *SAMLGroupMappingStatus) DeepCopyInto(out *SAMLGroupMappingStatus) {
    *out = *in
    if in.Conditions != nil {
        out.Conditions = make([]metav1.Condition, len(in.Conditions))
        for i := range in.Conditions {
            in.Conditions[i].DeepCopyInto(&out.Conditions[i])
        }
    }
    if in.LastSyncTime != nil {
        t := in.LastSyncTime.DeepCopy()
        out.LastSyncTime = &t
    }
}

// DeepCopyInto copies the receiver into out.
func (in *SAMLGroupMappingList) DeepCopyInto(out *SAMLGroupMappingList) {
    *out = *in
    out.TypeMeta = in.TypeMeta
    in.ListMeta.DeepCopyInto(&out.ListMeta)
    if in.Items != nil {
        out.Items = make([]SAMLGroupMapping, len(in.Items))
        for i := range in.Items {
            in.Items[i].DeepCopyInto(&out.Items[i])
        }
    }
}

// DeepCopy returns a deep copy of the receiver.
func (in *SAMLGroupMappingList) DeepCopy() *SAMLGroupMappingList {
    if in == nil {
        return nil
    }
    out := new(SAMLGroupMappingList)
    in.DeepCopyInto(out)
    return out
}
```

## The CRD manifest

The canonical CRD lives at [`kustomize/crd.yaml`](kustomize/crd.yaml).
It's the production Wiz operator's CRD copied verbatim — `apiextensions.k8s.io/v1`,
group `wiz.rtkwlf.io`, kind `SAMLGroupMapping`, with `spec.providerGroupId`,
`spec.roleRef.{name,roleId}`, `spec.projectRefs[].{name,projectId}`,
`spec.identityProviderId` (required), `spec.description` (optional).

The `additionalPrinterColumns` show `Ready` / `Synced` / `Valid` /
`Age` so `kubectl get samlgroupmappings` is informative without `-o yaml`.

## Shared scheme + clients

One package, one purpose: build a `runtime.Scheme` with both core types
and the demo's CRD, and produce a controller-runtime client off a
`*rest.Config`.

### `internal/k8s/clients.go`

```go
// Package k8s holds the shared scheme and a Clients constructor that
// returns the controller-runtime client used by the K8s backend.
package k8s

import (
    "fmt"

    apiruntime "k8s.io/apimachinery/pkg/runtime"
    utilruntime "k8s.io/apimachinery/pkg/util/runtime"
    clientgoscheme "k8s.io/client-go/kubernetes/scheme"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/clientcmd"
    "sigs.k8s.io/controller-runtime/pkg/client"

    wizv1alpha1 "github.com/example/webhookd-demo/internal/wizapi/v1alpha1"
)

// Scheme is the package-level runtime.Scheme — register every API
// group the backend touches at init time.
var Scheme = apiruntime.NewScheme()

func init() {
    utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
    utilruntime.Must(wizv1alpha1.AddToScheme(Scheme))
}

// Clients holds the typed controller-runtime client plus the
// underlying *rest.Config (kept for contexts where the raw config
// is useful — informer factories, dynamic clients, etc.)
type Clients struct {
    Ctrl       client.WithWatch
    RESTConfig *rest.Config
}

// NewClients builds a Clients from the kubeconfig at path. Empty path
// means in-cluster.
func NewClients(kubeconfigPath string) (*Clients, error) {
    var cfg *rest.Config
    var err error
    if kubeconfigPath == "" {
        cfg, err = rest.InClusterConfig()
    } else {
        cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
    }
    if err != nil {
        return nil, fmt.Errorf("build rest config: %w", err)
    }

    c, err := client.NewWithWatch(cfg, client.Options{Scheme: Scheme})
    if err != nil {
        return nil, fmt.Errorf("build ctrl client: %w", err)
    }
    return &Clients{Ctrl: c, RESTConfig: cfg}, nil
}
```

> **Why `client.WithWatch` and not `client.Client`?** The watch step
> needs a typed Watch method. `WithWatch` is a strict superset of
> `Client`, so we use it everywhere — no second client construction.

## Backend config

### `internal/integrations/k8sbackend/config.go`

```go
// Package k8sbackend implements the Kubernetes Backend: SSA apply
// followed by a synchronous Watch for Ready=True.
package k8sbackend

import (
    "fmt"
    "time"

    "github.com/hashicorp/hcl/v2"
    "github.com/hashicorp/hcl/v2/gohcl"
)

// Config is the typed shape of `backend "k8s" { ... }`.
type Config struct {
    KubeconfigEnv      string `hcl:"kubeconfig_env,optional"`
    Namespace          string `hcl:"namespace"`
    // IdentityProviderID is the tenant-wide Wiz SAML IDP. Populates
    // spec.identityProviderId on every SAMLGroupMapping the backend
    // applies for this instance (one IDP per JSM tenant).
    IdentityProviderID string `hcl:"identity_provider_id"`
    SyncTimeout        string `hcl:"sync_timeout,optional"`
}

// SyncTimeoutDuration parses Config.SyncTimeout, defaulting to 20s.
func (c Config) SyncTimeoutDuration() time.Duration {
    if c.SyncTimeout == "" {
        return 20 * time.Second
    }
    d, err := time.ParseDuration(c.SyncTimeout)
    if err != nil {
        return 20 * time.Second
    }
    return d
}

func decodeConfig(body hcl.Body, ctx *hcl.EvalContext) (Config, hcl.Diagnostics) {
    var cfg Config
    diags := gohcl.DecodeBody(body, ctx, &cfg)
    if diags.HasErrors() {
        return Config{}, diags
    }
    if cfg.Namespace == "" {
        return Config{}, hcl.Diagnostics{{
            Severity: hcl.DiagError,
            Summary:  "k8s backend",
            Detail:   "namespace is required",
        }}
    }
    if cfg.IdentityProviderID == "" {
        return Config{}, hcl.Diagnostics{{
            Severity: hcl.DiagError,
            Summary:  "k8s backend",
            Detail:   "identity_provider_id is required",
        }}
    }
    return cfg, nil
}

// fieldOwner is the SSA field-manager identifier. K8s uses this to
// track ownership of fields on the object.
const fieldOwner = "webhookd-demo"

// withSyncTimeout returns a context derived from ctx with the
// configured sync timeout. Caller must call cancel().
func withSyncTimeout(ctx context.Context, cfg Config) (context.Context, func()) {
    return context.WithTimeout(ctx, cfg.SyncTimeoutDuration())
}

// (intentionally unused stub — illustrates the helper pattern; remove
// if you don't end up needing it.)
var _ = fmt.Sprintf
var _ context.Context = nil
```

> Drop the bottom two `var _ = ...` lines once the rest of the package
> compiles — they're there only so this snippet stands alone.

## Apply path (SSA)

### `internal/integrations/k8sbackend/apply.go`

```go
package k8sbackend

import (
    "context"
    "errors"
    "fmt"

    apierrors "k8s.io/apimachinery/pkg/api/errors"
    "sigs.k8s.io/controller-runtime/pkg/client"

    "github.com/example/webhookd-demo/internal/integrations/jsm"
    "github.com/example/webhookd-demo/internal/k8s"
    wizv1alpha1 "github.com/example/webhookd-demo/internal/wizapi/v1alpha1"
)

// applyMapping translates a *jsm.SAMLGroupMappingRequest into a
// SAMLGroupMapping CR and Server-Side-Applies it.
//
// Returns the applied object's name + a typed errorClass for the
// caller to map onto an ExecResult.
func (b *Backend) applyMapping(ctx context.Context, req *jsm.SAMLGroupMappingRequest, cfg Config) (string, errorClass, error) {
    obj := &wizv1alpha1.SAMLGroupMapping{}
    obj.SetGroupVersionKind(wizv1alpha1.GroupVersion.WithKind("SAMLGroupMapping"))
    obj.SetNamespace(cfg.Namespace)
    obj.SetName(req.IssueKey)

    obj.Spec = wizv1alpha1.SAMLGroupMappingSpec{
        IdentityProviderID: cfg.IdentityProviderID,   // backend-config-supplied
        ProviderGroupID:    req.ProviderGroupID,
        Description:        req.Description,
        RoleRef: wizv1alpha1.RoleReference{
            Name: req.RoleName,
        },
        ProjectRefs: []wizv1alpha1.ProjectReference{
            {Name: req.ProjectName},
        },
    }

    annotations := map[string]string{
        "webhookd.io/jsm-issue-key": req.IssueKey,
    }
    if req.TraceID != "" {
        // ADR-0007: propagate trace-id so the operator can link spans.
        annotations["webhookd.io/trace-id"] = req.TraceID
    }
    obj.SetAnnotations(annotations)

    err := b.clients.Ctrl.Patch(
        ctx,
        obj,
        client.Apply,
        client.FieldOwner(fieldOwner),
        client.ForceOwnership,
    )
    if err != nil {
        return "", classifyApplyErr(err), fmt.Errorf("apply: %w", err)
    }
    return obj.Name, errClassNone, nil
}

// errorClass is the K8s-error → ExecResult bridge.
type errorClass int

const (
    errClassNone errorClass = iota
    errClassClient            // 4xx-mappable: Forbidden, Invalid, …
    errClassServer            // 5xx-mappable: ServerTimeout, ServiceUnavailable, …
    errClassConflict          // SSA conflict — usually retryable, but the
                               // demo treats it as 5xx for simplicity.
)

func classifyApplyErr(err error) errorClass {
    switch {
    case err == nil:
        return errClassNone
    case apierrors.IsForbidden(err), apierrors.IsInvalid(err), apierrors.IsNotFound(err):
        return errClassClient
    case apierrors.IsConflict(err):
        return errClassConflict
    case apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err), apierrors.IsTooManyRequests(err):
        return errClassServer
    case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
        return errClassServer
    default:
        return errClassServer
    }
}

// Compile-time check that we still build against the controller-runtime
// types we expect.
var _ client.Patch = client.Apply
```

## Watch path

The synchronous wait. ADR-0006 says we wait inline; ADR-0007 says
we surface the trace ID; the executor design (CLAUDE.md note) says
the watch step is binary — we don't classify Ready=False vs Pending,
we just wait for Ready=True or time out.

### `internal/integrations/k8sbackend/watch.go`

```go
package k8sbackend

import (
    "context"
    "errors"
    "fmt"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/fields"
    "k8s.io/apimachinery/pkg/watch"
    "sigs.k8s.io/controller-runtime/pkg/client"

    wizv1alpha1 "github.com/example/webhookd-demo/internal/wizapi/v1alpha1"
)

// errTimeout is returned when the watch deadline elapses before
// Ready=True is observed.
var errTimeout = errors.New("watch timeout")

// waitForReady opens a namespace-scoped Watch on SAMLGroupMapping and
// returns once the named object reports Ready=True. Times out on the
// context deadline.
func (b *Backend) waitForReady(ctx context.Context, name, namespace string) error {
    list := &wizv1alpha1.SAMLGroupMappingList{}
    w, err := b.clients.Ctrl.Watch(
        ctx,
        list,
        client.InNamespace(namespace),
        client.MatchingFieldsSelector{Selector: fields.Everything()},
    )
    if err != nil {
        return fmt.Errorf("open watch: %w", err)
    }
    defer w.Stop()

    for {
        select {
        case <-ctx.Done():
            return errTimeout
        case ev, ok := <-w.ResultChan():
            if !ok {
                return fmt.Errorf("watch channel closed")
            }
            if ev.Type == watch.Error {
                return fmt.Errorf("watch error: %v", ev.Object)
            }
            obj, ok := ev.Object.(*wizv1alpha1.SAMLGroupMapping)
            if !ok {
                continue
            }
            if obj.Name != name {
                continue
            }
            if isReady(obj.Status.Conditions) {
                return nil
            }
        }
    }
}

// isReady scans conditions for a Ready condition with status=True.
func isReady(conds []metav1.Condition) bool {
    for _, c := range conds {
        if c.Type == "Ready" && c.Status == metav1.ConditionTrue {
            return true
        }
    }
    return false
}
```

> **Why not `tools/watch.UntilWithSync`?** Production webhookd's
> CLAUDE.md captures this: under `client-go` v0.35+, the reflector
> backing `UntilWithSync` requires bookmark events from the server
> that hand-rolled list/watches don't supply, and the watch deadlocks
> for the full timeout. The hand-rolled `select`-on-`ResultChan` loop
> sidesteps the bookmark dependency.

## The Backend struct

### `internal/integrations/k8sbackend/backend.go`

```go
package k8sbackend

import (
    "context"
    "fmt"
    "net/http"
    "os"

    "github.com/hashicorp/hcl/v2"

    "github.com/example/webhookd-demo/internal/integrations/jsm"
    "github.com/example/webhookd-demo/internal/k8s"
    "github.com/example/webhookd-demo/internal/webhook"
)

// Backend implements webhook.Backend. One instance per process; each
// Execute call is allowed to read backend.Config off the dispatcher.
type Backend struct {
    clients *k8s.Clients
}

// New constructs a Backend.
func New(c *k8s.Clients) *Backend {
    return &Backend{clients: c}
}

// Type implements webhook.Backend.
func (b *Backend) Type() string { return "k8s" }

// DecodeConfig implements webhook.Backend.
func (b *Backend) DecodeConfig(body hcl.Body, ctx *hcl.EvalContext) (webhook.BackendConfig, hcl.Diagnostics) {
    return decodeConfig(body, ctx)
}

// Execute implements webhook.Backend.
func (b *Backend) Execute(ctx context.Context, req webhook.BackendRequest, cfg webhook.BackendConfig) webhook.ExecResult {
    c, ok := cfg.(Config)
    if !ok {
        return errResult(http.StatusInternalServerError, "ConfigType",
            fmt.Sprintf("k8s backend: unexpected config type %T", cfg))
    }

    mr, ok := req.(*jsm.SAMLGroupMappingRequest)
    if !ok {
        return errResult(http.StatusBadRequest, "UnsupportedRequest",
            fmt.Sprintf("k8s backend: unsupported request type %T", req))
    }

    // 1. Apply.
    name, ec, err := b.applyMapping(ctx, mr, c)
    switch ec {
    case errClassClient:
        return errResult(http.StatusUnprocessableEntity, "ApplyRejected", err.Error())
    case errClassServer, errClassConflict:
        return errResult(http.StatusBadGateway, "ApplyFailed", err.Error())
    }

    // 2. Wait for Ready=True with the configured budget.
    syncCtx, cancel := context.WithTimeout(ctx, c.SyncTimeoutDuration())
    defer cancel()

    if err := b.waitForReady(syncCtx, name, c.Namespace); err != nil {
        if err == errTimeout {
            return webhook.ExecResult{
                Kind:       webhook.ResultTimeout,
                HTTPStatus: http.StatusGatewayTimeout,
                Reason:     "TimedOut",
                Detail:     fmt.Sprintf("waited %s for Ready=True", c.SyncTimeoutDuration()),
            }
        }
        return errResult(http.StatusBadGateway, "WatchFailed", err.Error())
    }

    return webhook.ExecResult{
        Kind:       webhook.ResultSuccess,
        HTTPStatus: http.StatusOK,
        Reason:     "Synced",
        Detail:     fmt.Sprintf("SAMLGroupMapping/%s reached Ready=True", name),
    }
}

// errResult is a small helper for the error-path cases above.
func errResult(status int, reason, detail string) webhook.ExecResult {
    kind := webhook.ResultServerError
    if status >= 400 && status < 500 {
        kind = webhook.ResultClientError
    }
    return webhook.ExecResult{
        Kind:       kind,
        HTTPStatus: status,
        Reason:     reason,
        Detail:     detail,
    }
}

// kubeconfigPath resolves the kubeconfig path from the Config's
// KubeconfigEnv, defaulting to in-cluster (empty string) when unset.
func kubeconfigPath(c Config) string {
    if c.KubeconfigEnv == "" {
        return ""
    }
    return os.Getenv(c.KubeconfigEnv)
}
```

> **Where does `b.clients` come from?** `main.go` (phase 9) constructs
> it once at startup with `k8s.NewClients(...)` and threads it into the
> Backend at registration time.

## Registration

The Backend's `init()` is a little different from the JSM provider's:
the Backend needs `*k8s.Clients` to function, which can't exist at
package-init time (we don't have a kubeconfig yet).

We use a sentinel: register a *placeholder* in `init()`, then have
`main.go` swap in the real backend after building the clients.
Production code can do this more cleanly via the explicit
`webhook.NewRegistry()` path; for the demo, we accept the small wart.

### `internal/integrations/k8sbackend/init.go`

```go
package k8sbackend

import (
    "context"
    "fmt"

    "github.com/hashicorp/hcl/v2"

    "github.com/example/webhookd-demo/internal/k8s"
    "github.com/example/webhookd-demo/internal/webhook"
)

// placeholder is a Backend that returns "not initialized" errors.
// Replaced by Setup() once main.go has built the K8s clients.
type placeholder struct{}

func (placeholder) Type() string { return "k8s" }

func (placeholder) DecodeConfig(body hcl.Body, ctx *hcl.EvalContext) (webhook.BackendConfig, hcl.Diagnostics) {
    return decodeConfig(body, ctx)
}

func (placeholder) Execute(_ context.Context, _ webhook.BackendRequest, _ webhook.BackendConfig) webhook.ExecResult {
    return webhook.ExecResult{
        Kind:       webhook.ResultServerError,
        HTTPStatus: 500,
        Reason:     "BackendNotInitialized",
        Detail:     "k8s backend has not been Setup() yet",
    }
}

func init() {
    webhook.RegisterBackend(placeholder{})
}

// Setup replaces the placeholder backend with a real one constructed
// from the given clients. main.go calls this after k8s.NewClients()
// returns.
func Setup(clients *k8s.Clients) error {
    if clients == nil {
        return fmt.Errorf("k8sbackend.Setup: nil clients")
    }
    real := New(clients)
    // Re-register: the registry's panic-on-duplicate is exactly what
    // we don't want here. Use ReplaceBackend instead — see below.
    webhook.Default.ReplaceBackend(real)
    return nil
}
```

We need one small addition to the registry to support replacement:

### Patch to `internal/webhook/registry.go`

```go
// ReplaceBackend swaps the registered backend. Used once at startup to
// upgrade init()-registered placeholders. NOT for runtime use — there's
// no synchronization with in-flight dispatches.
func (r *Registry) ReplaceBackend(b Backend) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.backends[b.Type()] = b
}
```

> **Cleaner alternative for production.** Don't use `init()` for
> backends that depend on runtime state. Either (a) require the
> backend to take its dependencies in `Execute` rather than at
> construction (more verbose, less natural), or (b) skip `init()`
> entirely and have `main.go` construct + register every integration
> explicitly. ADR-0010's `webhook.NewRegistry()` path supports (b).
> The demo uses the placeholder pattern because it minimizes the
> diff between the JSM provider (clean `init()`) and the K8s backend
> (state-dependent) — the architectural shape is the same.

## Mock operator (for the watch path to terminate)

The K8s backend's watch step never resolves unless something writes
`Ready=True` to the CR. Production has a real operator. The demo
ships a tiny stub.

### `cmd/mock-operator/main.go`

```go
// Mock operator: every 2s, scan SAMLGroupMapping CRs in the configured
// namespace; if one lacks Ready=True, write it.
//
// Not a real operator — no informers, no rate limiting, no leader
// election, no upstream Wiz reconciliation. Exists only so the K8s
// backend's watch step terminates during the demo.
package main

import (
    "context"
    "fmt"
    "log/slog"
    "os"
    "os/signal"
    "syscall"
    "time"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "sigs.k8s.io/controller-runtime/pkg/client"

    "github.com/example/webhookd-demo/internal/k8s"
    wizv1alpha1 "github.com/example/webhookd-demo/internal/wizapi/v1alpha1"
)

func main() {
    namespace := os.Getenv("DEMO_NAMESPACE")
    if namespace == "" {
        namespace = "wiz-operator"
    }
    kubeconfig := os.Getenv("KUBECONFIG")

    clients, err := k8s.NewClients(kubeconfig)
    if err != nil {
        slog.Error("build clients", "err", err)
        os.Exit(1)
    }

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    slog.Info("mock operator running", "namespace", namespace)

    t := time.NewTicker(2 * time.Second)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            slog.Info("shutdown")
            return
        case <-t.C:
            if err := tick(ctx, clients.Ctrl, namespace); err != nil {
                slog.Warn("tick", "err", err)
            }
        }
    }
}

func tick(ctx context.Context, c client.Client, namespace string) error {
    var list wizv1alpha1.SAMLGroupMappingList
    if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
        return fmt.Errorf("list: %w", err)
    }
    for i := range list.Items {
        sgm := &list.Items[i]
        if alreadyReady(sgm) {
            continue
        }
        sgm.Status.Conditions = upsertReady(sgm.Status.Conditions)
        if err := c.Status().Update(ctx, sgm); err != nil {
            slog.Warn("update status", "name", sgm.Name, "err", err)
            continue
        }
        slog.Info("flipped Ready=True", "name", sgm.Name)
    }
    return nil
}

func alreadyReady(sgm *wizv1alpha1.SAMLGroupMapping) bool {
    for _, c := range sgm.Status.Conditions {
        if c.Type == "Ready" && c.Status == metav1.ConditionTrue {
            return true
        }
    }
    return false
}

func upsertReady(conds []metav1.Condition) []metav1.Condition {
    now := metav1.Now()
    for i, c := range conds {
        if c.Type == "Ready" {
            conds[i].Status = metav1.ConditionTrue
            conds[i].LastTransitionTime = now
            conds[i].Reason = "MockOperator"
            conds[i].Message = "demo: synthetically marked ready"
            return conds
        }
    }
    return append(conds, metav1.Condition{
        Type:               "Ready",
        Status:             metav1.ConditionTrue,
        LastTransitionTime: now,
        Reason:             "MockOperator",
        Message:            "demo: synthetically marked ready",
    })
}
```

## What we proved

- [x] One typed CRD model + handwritten DeepCopy is enough for a demo
- [x] SSA apply via `client.Patch(obj, client.Apply, FieldOwner, ForceOwnership)`
- [x] Watch step uses a hand-rolled `ResultChan` loop (avoids the
      bookmark-dependency deadlock with `UntilWithSync` under client-go v0.35+)
- [x] Trace-id is propagated as an annotation per ADR-0007
- [x] `ExecResult` shape preserved end-to-end

Next: [06-observability.md](06-observability.md) — slog, OTel, Prometheus.
