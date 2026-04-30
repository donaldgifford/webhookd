# 14. Swap to Upstream wiz-operator Types

The demo ships a handwritten stub at `internal/wizapi/v1alpha1/` that
mirrors the canonical CRD. That's the same pattern production webhookd
uses — and for the same reason: the demo can build and run today
without depending on whether `github.com/donaldgifford/wiz-operator`
is importable yet.

When upstream is published and stable, the swap is **one `go get`,
one directory deletion, and four import-line edits.** This phase is
the recipe.

This phase is optional. The demo works fine without it. Skip if you're
just exercising the architecture; come back when you actually want to
share types with the operator.

## Why the stub in the first place

Two reasons:

1. **The upstream module may not be ready.** When the host repo
   (here, webhookd-demo) wants types from a sibling repo that's
   still pre-release, the cleanest pattern is a local stub that
   mirrors the canonical YAML — file path conventions, group/version,
   field tags, DeepCopy method set. Phase 5's
   `internal/wizapi/v1alpha1/` is exactly that.
2. **Test isolation.** Even after upstream lands, you may want the
   demo to keep building when upstream's CI is broken or the module
   is in flux. Stubs decouple the demo's blast radius from upstream
   churn.

Production webhookd's CLAUDE.md captures this verbatim:

> Local stub for cross-repo Go types. When the project depends on
> Go types owned by another repo that hasn't published yet (Phase 2's
> `github.com/donaldgifford/wiz-operator/api/v1alpha1`), ship a local
> stub at `internal/webhook/wizapi/` mirroring the YAML samples in
> `docs/examples/samples/`. Everything imports through one alias;
> the swap to the real module is one line plus stub deletion.

## Preconditions

Before you swap:

- The `github.com/donaldgifford/wiz-operator` module is importable
  at the version you want (`go get github.com/donaldgifford/wiz-operator@v0.x.y`
  succeeds without complaint).
- The upstream module's `api/v1alpha1` package exposes the same type
  names the stub uses: `SAMLGroupMapping`, `SAMLGroupMappingList`,
  `SAMLGroupMappingSpec`, `SAMLGroupMappingStatus`, `RoleReference`,
  `ProjectReference` plus the `GroupVersion` / `AddToScheme` symbols.
  If they diverge, fix the stub-to-upstream rename in step 4 below.
- The upstream module's `k8s.io/*` pins are *compatible with* yours.
  See the gotcha section.

## The swap (4 steps)

### 1. `go get` upstream

```bash
go get github.com/donaldgifford/wiz-operator@v0.1.0   # or whatever's current
```

### 2. Delete the stub

```bash
rm -rf internal/wizapi/v1alpha1/
```

The package directory disappears. Builds will break until step 3 lands.

### 3. Re-point the four import sites

Every file that imported the stub used the alias `wizv1alpha1`. Find them:

```bash
grep -rl 'github.com/example/webhookd-demo/internal/wizapi/v1alpha1' .
# internal/k8s/clients.go
# internal/integrations/k8sbackend/apply.go
# internal/integrations/k8sbackend/watch.go
# cmd/mock-operator/main.go
```

In each one, change:

```go
wizv1alpha1 "github.com/example/webhookd-demo/internal/wizapi/v1alpha1"
```

to:

```go
wizv1alpha1 "github.com/donaldgifford/wiz-operator/api/v1alpha1"
```

Use sed if you trust your shell:

```bash
grep -rl 'github.com/example/webhookd-demo/internal/wizapi/v1alpha1' . \
  | xargs sed -i '' 's|github.com/example/webhookd-demo/internal/wizapi/v1alpha1|github.com/donaldgifford/wiz-operator/api/v1alpha1|g'
```

(Drop the `''` after `-i` on Linux.)

### 4. Tidy + verify

```bash
go mod tidy
go build ./...
go test ./...
```

`go mod tidy` may pull in additional transitive deps from upstream
(controller-tools, kubebuilder helpers, etc.). That's fine.

If everything compiles + tests still pass, the swap is done. The
binary behaves identically — the wire format, RBAC, and config
schema were already canonical.

## What you can delete after the swap

Once upstream is the source of truth, the local stub's design notes
in `05-k8s-backend.md` (the "handwritten DeepCopy" sections) stop
applying. Treat phase 5's `groupversion_info.go`, `types.go`,
`zz_generated.deepcopy.go` as historical — useful when you bootstrap
*another* downstream consumer of `wiz-operator`'s types but obsolete
for this demo.

## Common gotchas

### k8s.io version skew

Upstream `wiz-operator` pins specific `k8s.io/api` / `k8s.io/apimachinery` /
`k8s.io/client-go` versions. The demo pins its own (per the
`controller-runtime` constraint in
[01-bootstrap.md](01-bootstrap.md)). If they diverge, you'll see one
of two failure modes:

- `cannot use ... as k8s.io/apimachinery/pkg/runtime.Object` —
  interface mismatch from version skew
- `HasSyncedChecker has different number of methods` —
  the recurring `controller-runtime` ↔ `k8s.io/*` interface drift

**Fix:** align both repos to the same pins. Easiest path:

```bash
# 1. Find what upstream wiz-operator uses.
go list -m -json github.com/donaldgifford/wiz-operator | jq .GoMod
cat <that-go.mod-path> | grep k8s.io

# 2. Pin webhookd-demo's go.mod to match.
go get k8s.io/api@vX.Y.Z k8s.io/apimachinery@vX.Y.Z k8s.io/client-go@vX.Y.Z
go get sigs.k8s.io/controller-runtime@vA.B.C
go mod tidy
```

This is the same pattern production webhookd documents in CLAUDE.md
("After any controller-runtime bump, verify the dependent k8s.io
modules align").

### Scheme registration

The stub's `internal/wizapi/v1alpha1/groupversion_info.go` exposes
`AddToScheme`. Upstream should expose the same symbol — but if it's
named differently (e.g. `Install` or `RegisterTypes`), update
`internal/k8s/clients.go`:

```go
func init() {
    utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
    utilruntime.Must(wizv1alpha1.AddToScheme(Scheme))   // ← rename if upstream differs
}
```

### Field renames

The stub matches the canonical CRD field-for-field
([`samlmapping.example.yaml`](samlmapping.example.yaml)). If upstream
adds new fields you don't care about, ignore them. If upstream
*renames* fields you do care about, the JSM provider's `applyMapping`
in [05-k8s-backend.md](05-k8s-backend.md) needs the matching update.

### Going back

If upstream regresses or its API churns mid-flight, restore the stub:

```bash
git checkout main -- internal/wizapi/v1alpha1/
# revert the four import-line edits
go mod edit -droprequire github.com/donaldgifford/wiz-operator
go mod tidy
```

The stub is the safety net.

## Verifying the demo still works after the swap

```bash
just kind-up
just dev-stack
just mock-operator   # in another terminal
just run             # in another terminal
just send-jsm        # in another terminal
just get-mappings
```

Same response, same CR shape, same trace + metrics. If anything
changed visibly, your upstream `SAMLGroupMapping` shape diverged
from the stub — investigate before assuming the swap is "done".

## What we proved

- [x] Demo's local stub follows production webhookd's documented pattern
- [x] Swap to upstream is `go get` + `rm -rf` + 4 import-line edits
- [x] k8s.io version-pin alignment is the only real gotcha
- [x] Stub remains the safety net if upstream regresses

That's the demo. Run it, swap if you're ready, and use the
architecture as the template for the production refactor.
