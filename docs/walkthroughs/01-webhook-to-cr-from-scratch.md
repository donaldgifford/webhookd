# Walkthrough: Build a webhook → Kubernetes CR API from scratch (Part 1)

This walkthrough builds, from zero, a small Go service that:

1. Listens on a single HTTP webhook endpoint.
2. Decodes a JSON payload.
3. Creates a Kubernetes Custom Resource (CR) in a cluster the service is
   connected to.
4. Runs in a Docker container built with **`docker buildx bake`** and an HCL2
   config.

It is deliberately stripped-down. We use the **dynamic client** with
`unstructured.Unstructured` here — no codegen, no operator project, no imports
beyond `client-go`. That keeps the moving parts visible.

**Part 2** rewrites the same service against a **typed** client by generating
the CRD with kubebuilder and importing the Go types — the pattern this repo uses
in production. **Part 3** adds the observability stack (OTel, Prometheus,
Grafana, Tempo) via `docker compose`. None of the production concerns this repo
ships with — HMAC signing, OpenTelemetry, structured logging, graceful shutdown
nuance, rate limiting — appear here.

The reference implementation we lean on is the
[platform-operator API](https://github.com/donaldgifford/platform-operator/blob/main/api),
which does roughly the same thing on a larger surface area (six CRDs, full
CRUD). We trim that down to the simplest copy-pasteable shape.

---

## What we're building

```
                ┌──────────────┐                    ┌────────────────┐
   curl POST    │              │  dynamic Create    │                │
  ─────────────▶│ note-api:8080├───────────────────▶│  kube-apiserver│
  {title, body} │  (this app)  │   apiVersion: ...  │                │
                │              │   kind: Note       │   ┌──────────┐ │
                └──────────────┘                    │   │  Note CR │ │
                                                    │   └──────────┘ │
                                                    └────────────────┘
```

Concrete payload and CR:

```json
POST /webhook
Content-Type: application/json

{ "title": "Hello", "body": "World" }
```

becomes:

```yaml
apiVersion: examples.dev/v1alpha1
kind: Note
metadata:
  name: hello
  namespace: default
spec:
  title: Hello
  body: World
```

That's the whole job.

---

## Prerequisites

- **Go 1.22+** (we use the `net/http.ServeMux` path-value support added in
  1.22).
- **Docker 24+** with `buildx` (ships by default in modern Docker Desktop /
  Engine).
- A running Kubernetes cluster you have admin access to. Easiest is
  [kind](https://kind.sigs.k8s.io/): `kind create cluster --name walkthrough`.
- `kubectl` pointing at it.

---

## Project layout

We will end up with eight files in a flat layout:

```
note-api/
├── main.go             # everything: HTTP server, K8s client, handler
├── go.mod
├── go.sum
├── Dockerfile          # multi-stage build → distroless runtime
├── docker-bake.hcl     # buildx bake config (HCL2)
├── .dockerignore
├── crd.yaml            # the Note CRD (apply once before running)
└── sample-request.sh   # convenience script for local testing
```

Real services break this into packages (`internal/server`, `internal/k8s`,
etc.). For this walkthrough, **everything goes in `main.go`**. You can extract
packages once you understand the moving parts.

---

## Step 1 — Initialize the module

```bash
mkdir note-api && cd note-api
go mod init github.com/example/note-api
```

We will need three direct dependencies. Add them now so we can fill in imports
without thinking about it:

```bash
go get k8s.io/apimachinery@v0.34.2
go get k8s.io/client-go@v0.34.2
```

That's it. **No web framework, no router library, no logging library.** stdlib
`net/http` is the whole HTTP stack.

> **Why these versions?** `k8s.io/apimachinery` and `k8s.io/client-go` move
> together — pin both to the same minor (here `v0.34.x`). The
> [Kubernetes module compatibility matrix](https://github.com/kubernetes/client-go#compatibility-matrix)
> tells you which versions match which Kubernetes minor releases.

---

## Step 2 — The HTTP server (stdlib only)

Create `main.go`. We will build it up in pieces; copy each block in order.

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
)

// CreateNoteRequest is the JSON shape the webhook accepts.
type CreateNoteRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Server holds anything a handler needs. Right now it's empty —
// we'll add a Kubernetes client in Step 4.
type Server struct{}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req CreateNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if req.Title == "" || req.Body == "" {
		http.Error(w, `{"error":"title and body are required"}`, http.StatusBadRequest)
		return
	}

	// Real work goes here in Step 5.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"status":"received","title":%q}`, req.Title)
}

func main() {
	s := &Server{}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.handleWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":" + envOr("PORT", "8080")
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

Run it:

```bash
go run .
# in another terminal:
curl -X POST localhost:8080/webhook \
  -H 'Content-Type: application/json' \
  -d '{"title":"Hello","body":"World"}'
# {"status":"received","title":"Hello"}
```

**What we have:** an HTTP server with one POST endpoint and a healthcheck. No
Kubernetes yet.

> **Why stdlib?** stdlib `net/http` has had `ServeMux` path-pattern routing
> since Go 1.22. For small services this removes the need for `gorilla/mux`,
> `chi`, or `echo`. One less dependency, one less surface for vulnerabilities,
> one less doc to read.

---

## Step 3 — Define the CRD

Before we can create a `Note`, the cluster needs to know what one is. Save this
as `crd.yaml`:

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: notes.examples.dev
spec:
  group: examples.dev
  names:
    kind: Note
    listKind: NoteList
    plural: notes
    singular: note
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              required: [title, body]
              properties:
                title:
                  type: string
                  minLength: 1
                body:
                  type: string
                  minLength: 1
```

Apply it once, against your cluster:

```bash
kubectl apply -f crd.yaml
kubectl get crd notes.examples.dev
```

You don't need an operator running. We only need the apiserver to _accept_
`Note` resources for storage. An operator would be the thing that does something
with them — out of scope for this walkthrough.

> **GroupVersionResource (GVR).** Every Kubernetes resource is identified by a
> triple: `group` (`examples.dev`), `version` (`v1alpha1`), and `resource` (the
> lowercase plural — `notes`). The dynamic client we use in Step 4 takes a GVR;
> the apiserver routes the request based on it.

---

## Step 4 — Connect to Kubernetes

Add the imports and the client wiring. Replace the top of `main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// noteGVR identifies our CRD on the apiserver wire format.
// Group/Version come from the CRD's spec.group + spec.versions[*].name.
// Resource is the CRD's spec.names.plural — *not* the kind.
var noteGVR = schema.GroupVersionResource{
	Group:    "examples.dev",
	Version:  "v1alpha1",
	Resource: "notes",
}

// Server now carries a Kubernetes dynamic client.
type Server struct {
	dyn dynamic.Interface
}

// loadKubeConfig returns a *rest.Config for the cluster the binary is
// running against. Two paths:
//
//  1. In-cluster (the binary is running as a pod): use the mounted
//     ServiceAccount token at /var/run/secrets/kubernetes.io/serviceaccount.
//  2. Out-of-cluster (the binary is running on a developer machine):
//     read $KUBECONFIG, falling back to ~/.kube/config.
//
// Try in-cluster first because it's the common production case; on
// failure, fall back to the user's kubeconfig. This is the same pattern
// every controller-runtime / kubebuilder operator uses.
func loadKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, overrides,
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return cfg, nil
}

// NewServer constructs the dynamic client once at startup.
func NewServer() (*Server, error) {
	cfg, err := loadKubeConfig()
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	return &Server{dyn: dyn}, nil
}
```

> **Why the dynamic client?** There are two ways to talk to the apiserver from
> Go: the **typed** client (`*kubernetes.Clientset` for built-in resources,
> generated typed clients for CRDs) or the **dynamic** client
> (`dynamic.Interface`, works with any GVR via `unstructured.Unstructured`).
>
> Typed clients give you compile-time safety. Generating them for your CRDs
> requires `controller-tools` / `kubebuilder` codegen — overkill for a tiny
> service. The dynamic client costs us a little type safety at the call site but
> lets us ship without code generation. The platform-operator API uses the same
> approach.

---

## Step 5 — Create the CR from the webhook payload

Replace `handleWebhook` with the real version. The shape is: decode → validate →
build an `unstructured.Unstructured` → call `Create`.

```go
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req CreateNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if req.Title == "" || req.Body == "" {
		http.Error(w, `{"error":"title and body are required"}`, http.StatusBadRequest)
		return
	}

	// Build the Note resource. Unstructured is just a map[string]interface{}
	// under the hood — same JSON the apiserver would accept on the wire.
	note := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "examples.dev/v1alpha1",
			"kind":       "Note",
			"metadata": map[string]interface{}{
				// Use the title as the name, downcased so it's a valid DNS label.
				// Real services should generate names safely (e.g., RFC1123
				// sanitization + a uniqueness suffix). Kept simple here.
				"name":      sanitize(req.Title),
				"namespace": envOr("NAMESPACE", "default"),
			},
			"spec": map[string]interface{}{
				"title": req.Title,
				"body":  req.Body,
			},
		},
	}

	// 5-second deadline so a stuck apiserver doesn't pin the request.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	created, err := s.dyn.
		Resource(noteGVR).
		Namespace(note.GetNamespace()).
		Create(ctx, note, metav1.CreateOptions{})
	if err != nil {
		log.Printf("create note: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"create failed: %v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "created",
		"name":      created.GetName(),
		"namespace": created.GetNamespace(),
		"uid":       created.GetUID(),
	})
}

// sanitize lower-cases and replaces non-alphanumeric runs with single dashes.
// Good enough for this walkthrough; production code should use a real RFC1123
// validator and reject (not silently rewrite) bad input.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	dash := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
			dash = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			out = append(out, r)
			dash = false
		default:
			if !dash && len(out) > 0 {
				out = append(out, '-')
				dash = true
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return "note"
	}
	return string(out)
}
```

And update `main()` to wire `NewServer`:

```go
func main() {
	s, err := NewServer()
	if err != nil {
		log.Fatalf("server init: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.handleWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":" + envOr("PORT", "8080")
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
```

Run `go mod tidy` to lock dependencies, then test against your kind cluster:

```bash
go mod tidy
go run .
# expects KUBECONFIG / ~/.kube/config to point at your kind cluster.

# in another terminal:
curl -X POST localhost:8080/webhook \
  -H 'Content-Type: application/json' \
  -d '{"title":"hello","body":"world"}'
# {"status":"created","name":"hello","namespace":"default","uid":"..."}

kubectl get notes.examples.dev
# NAME    AGE
# hello   3s

kubectl get note hello -o yaml
# apiVersion: examples.dev/v1alpha1
# kind: Note
# metadata:
#   ...
# spec:
#   body: world
#   title: hello
```

You now have a working webhook → CR pipeline.

---

## Step 6 — The Dockerfile

Multi-stage build: a `golang` builder image compiles the binary, then we copy
just the binary into a minimal `distroless` runtime image. The final image is
~20 MB and contains no shell, no package manager, and no userspace beyond the
binary.

```dockerfile
# syntax=docker/dockerfile:1.7

# ---------- build stage ----------
FROM golang:1.22 AS builder
WORKDIR /src

# Cache deps separately from source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

# CGO_ENABLED=0 → static binary, no glibc dependency at runtime.
# -trimpath → strips local paths from the binary (smaller, more reproducible).
# -ldflags='-s -w' → strips debug info (smaller binary).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags='-s -w' -o /out/note-api .

# ---------- runtime stage ----------
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/note-api /note-api
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/note-api"]
```

Add a `.dockerignore` so we don't copy junk:

```gitignore
.git
*.md
docker-bake.hcl
Dockerfile
.dockerignore
```

Sanity-check the build with the Docker CLI directly first (without bake):

```bash
docker build -t note-api:dev .
docker run --rm -p 8080:8080 note-api:dev &
curl localhost:8080/healthz
# ok
```

It will start but fail to talk to a cluster — we haven't mounted a kubeconfig.
We'll fix that in Step 8.

---

## Step 7 — `docker-bake.hcl`

`docker buildx bake` is the declarative replacement for shell-scripts that wrap
`docker build` with arrays of `--tag`, `--platform`, and `--build-arg` flags.
The config file is in **HCL2** (the same language Terraform uses).

The pieces:

- **Variables** — overridable from CLI (`--set`) or environment.
- **Targets** — one per image, can `inherit` from a base.
- **Groups** — named sets of targets you can build together.

```hcl
# docker-bake.hcl — declarative buildx targets for note-api.
#
# Local dev:
#     docker buildx bake             # builds note-api:dev for your machine
#     docker buildx bake api         # same
#
# Multi-arch (CI):
#     docker buildx bake ci          # builds linux/amd64 + linux/arm64
#
# Override at the CLI:
#     docker buildx bake --set api.tags=note-api:smoke

variable "REGISTRY" {
  default = "ghcr.io/example"
}

variable "VERSION" {
  default = "dev"
}

# `default` is what runs when you call `docker buildx bake` with no arg.
group "default" {
  targets = ["api"]
}

# `ci` is the multi-arch build, called from CI.
group "ci" {
  targets = ["api-multi"]
}

# _common: shared dockerfile + OCI labels. Underscore-prefixed targets
# are conventionally treated as private (still callable, but signals
# "don't build me directly").
target "_common" {
  context    = "."
  dockerfile = "Dockerfile"

  labels = {
    "org.opencontainers.image.title"   = "note-api"
    "org.opencontainers.image.version" = "${VERSION}"
  }
}

# api: single-platform build for local dev.
target "api" {
  inherits = ["_common"]
  tags     = ["note-api:${VERSION}"]
}

# api-multi: multi-arch build with a registry tag, suitable for `docker push`.
target "api-multi" {
  inherits  = ["_common"]
  tags      = ["${REGISTRY}/note-api:${VERSION}"]
  platforms = ["linux/amd64", "linux/arm64"]
}
```

Now build with bake:

```bash
docker buildx bake
# resolves group "default" → target "api" → builds note-api:dev

docker buildx bake api --set api.tags=note-api:test
# overrides the tag for one build

VERSION=v0.1.0 docker buildx bake api
# variable overrides via env

docker buildx bake ci
# multi-arch build (you'll need a buildx builder that supports it; the
# default `docker-container` driver does. `docker buildx ls` to check.)
```

> **Why bake instead of a Makefile of `docker build` lines?** Three reasons:
>
> 1. **Targets compose.** `inherits` removes the copy-paste of common args.
> 2. **Variables are typed and validated** — typos in HCL2 fail at parse time,
>    not silently in a shell substitution.
> 3. **CI parity.** The same `docker buildx bake ci` runs on your laptop and in
>    GitHub Actions; nothing about the build is hidden in CI YAML.

---

## Step 8 — Run it in a container against your cluster

The container needs to reach the apiserver. Three ways, easiest first:

### 8a — Mount your kubeconfig (dev-only)

```bash
# Build first.
docker buildx bake api

# Run, mounting your kubeconfig read-only.
docker run --rm \
  --network host \
  -v ${HOME}/.kube:/home/nonroot/.kube:ro \
  -e KUBECONFIG=/home/nonroot/.kube/config \
  note-api:dev
```

> **kind on Linux:** `--network host` lets the container reach the apiserver at
> `127.0.0.1:<port>` (which is where kind exposes it). On macOS / Windows Docker
> Desktop, you'd instead replace the kubeconfig's `server:` line with
> `https://host.docker.internal:<port>` since `--network host` doesn't behave
> the same way there.

Smoke-test:

```bash
curl -X POST localhost:8080/webhook \
  -H 'Content-Type: application/json' \
  -d '{"title":"from-container","body":"hi"}'

kubectl get notes
# NAME              AGE
# from-container    2s
# hello             5m
```

### 8b — Run in-cluster (production shape)

In a real deployment, you'd:

1. Push the image:
   `VERSION=v0.1.0 docker buildx bake ci && docker push ghcr.io/example/note-api:v0.1.0`.
2. Apply a `Deployment` + `ServiceAccount` + `Role` + `RoleBinding` so the pod
   has `create` on `notes.examples.dev`. Skeleton:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: note-api
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: note-api
rules:
  - apiGroups: ["examples.dev"]
    resources: ["notes"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: note-api
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: note-api
subjects:
  - kind: ServiceAccount
    name: note-api
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: note-api
spec:
  replicas: 1
  selector: { matchLabels: { app: note-api } }
  template:
    metadata: { labels: { app: note-api } }
    spec:
      serviceAccountName: note-api
      containers:
        - name: note-api
          image: ghcr.io/example/note-api:v0.1.0
          ports: [{ containerPort: 8080 }]
          readinessProbe:
            httpGet: { path: /healthz, port: 8080 }
```

The `rest.InClusterConfig()` branch in `loadKubeConfig` picks up the mounted
ServiceAccount token automatically — that's why you don't see any authentication
code in `main.go`.

### 8c — `sample-request.sh`

For repeated local testing:

```bash
#!/usr/bin/env bash
set -euo pipefail

TITLE="${1:-walkthrough-$(date +%s)}"
BODY="${2:-hello from sample-request.sh}"

curl -sS -X POST localhost:8080/webhook \
  -H 'Content-Type: application/json' \
  -d "$(printf '{"title":%q,"body":%q}' "$TITLE" "$BODY")" \
  | jq .
```

`chmod +x sample-request.sh && ./sample-request.sh "my note" "with body"`.

---

## What we glossed over

This is the simplest possible shape that works. A real webhook receiver would
also have:

- **Request signing.** Anyone who can reach `:8080/webhook` can write CRs. In
  production you'd verify HMAC signatures on the incoming body.
- **Idempotency.** If the upstream retries the webhook, you'd get two `Note` CRs
  (or a 409). Use **Server-Side Apply** with a stable name derived from the
  payload to make retries safe.
- **Graceful shutdown.** SIGTERM should drain in-flight requests before exiting.
  `http.Server.Shutdown(ctx)` does this; we used the bare `http.ListenAndServe`
  to keep the example small.
- **Structured logging.** `log.Printf` is fine for a tutorial; production wants
  `log/slog` (stdlib) with JSON output and a request-id field.
- **Rate limiting.** A burst of webhooks could swamp the apiserver. Token bucket
  per source IP is the usual answer.
- **Observability.** No metrics, no traces, no dashboards. **That's Part 3.**
- **Typed clients.** We used `dynamic.Interface` + `unstructured.Unstructured` —
  fine, but every spec field is a string key in a map and typos fail at runtime.
  **Part 2** swaps this for typed Go structs generated by kubebuilder, so the
  api compiles against the same definitions the operator reconciles.

The patterns this repo's `webhookd` ships have all of these. This walkthrough
exists so the _shape_ of the problem is clear before those layers are added.

---

## Recap — where the moving parts live

| Concern            | Where it lives in this walkthrough                                  |
| ------------------ | ------------------------------------------------------------------- |
| HTTP routing       | `http.NewServeMux()` in `main()`                                    |
| Payload shape      | `CreateNoteRequest` struct + `json.NewDecoder`                      |
| Cluster connection | `loadKubeConfig()` → `dynamic.NewForConfig`                         |
| CR identity (GVR)  | `noteGVR` package var                                               |
| CR construction    | `unstructured.Unstructured{Object: …}`                              |
| CR creation        | `s.dyn.Resource(gvr).Namespace(ns).Create(...)`                     |
| Containerization   | `Dockerfile` (multi-stage → distroless)                             |
| Build pipeline     | `docker-bake.hcl` (HCL2 targets + groups)                           |
| Auth (in-cluster)  | `rest.InClusterConfig()` + `ServiceAccount`                         |
| Auth (local)       | `clientcmd.NewDefaultClientConfigLoadingRules()` → `~/.kube/config` |

Total Go code: ~150 lines. Total config: ~80 lines. Everything fits on a laptop
screen.

---

## Coming in Part 2 — typed clients via kubebuilder

`02-typed-clients-with-kubebuilder.md` (next walkthrough) keeps the same service
shape but trades `dynamic.Interface` + `unstructured.Unstructured` for typed Go
structs generated by **kubebuilder**:

- Scaffold a `note-operator` project with `kubebuilder init` and
  `kubebuilder create api --group examples --version v1alpha1 --kind Note`.
- Define `NoteSpec` / `NoteStatus` in Go with kubebuilder markers; let
  `make generate` produce `zz_generated.deepcopy.go` and `make manifests`
  produce the CRD YAML — same YAML we hand-wrote in Step 3, but now derived from
  the Go types so they can't drift.
- Refactor `note-api` to import `github.com/example/note-operator/api/v1alpha1`,
  drop the dynamic client, and use controller-runtime's typed
  `client.Client.Create(ctx, &Note{...})`. Compile-time field safety, IDE
  autocomplete, no string-keyed maps.
- See the side-by-side diff: roughly the same line count, but every spec field
  becomes a Go identifier the compiler can rename and refactor.

This is exactly the pattern this repo uses: `internal/webhook/wizapi/` is a
local stub of the wiz-operator's typed API, and `internal/webhook/executor.go`
does typed `client.Patch(ctx, &SAMLGroupMapping{...}, client.Apply, ...)` with
no `unstructured` anywhere.

## Coming in Part 3 — productionize

`03-productionize.md` (after that) adds the operational layers a real service
needs:

- **Structured logging** with `log/slog`, JSON output, request-id correlation.
- **Graceful shutdown.** `http.Server.Shutdown(ctx)` on SIGTERM, drain in-flight
  requests within a budget, fail-fast if the budget is exceeded.
- **Proper error handling.** Typed errors, classification of K8s API errors
  (forbidden / invalid / transient), correct HTTP status mapping.
- **Prometheus metrics** scraped from a `/metrics` endpoint on a separate admin
  port.
- **OpenTelemetry traces** exported via OTLP to a local collector.
- **Grafana** with a pre-provisioned dashboard, **Tempo** for traces.
- A **`docker-compose.yaml`** wiring the API + collector + Prometheus +
  Grafana + Tempo into one local stack you can `docker compose up`.

Each layer is small on its own; the value is in the composition. Keep this
walkthrough handy — Parts 2 and 3 both build on the exact `note-api` you just
wrote.
