# 12. Kustomize Deploy

A minimal kustomize layout that deploys webhookd-demo + mock-operator
+ the `SAMLGroupMapping` CRD into a kind cluster. No overlays — one
base manifests set, intentionally small.

## Files

```
docs/demo/kustomize/
├── kustomization.yaml
├── crd.yaml                 # SAMLGroupMapping CRD (wiz.rtkwlf.io/v1alpha1)
├── namespace.yaml
├── configmap.yaml           # webhookd.hcl config
├── secret.yaml.example      # signing secret stub
├── serviceaccount.yaml
├── role.yaml
├── rolebinding.yaml
├── deployment.yaml
├── service.yaml
└── mock-operator.yaml       # Deployment for the mock operator
```

> **Why no overlays?** The demo runs in one place (your laptop's kind
> cluster). Production webhookd splits per-environment via Helm or
> kustomize overlays — out of scope here.

## kind-config.yaml

Top-level helper that the justfile uses to create a kind cluster with
the right port-forwarding for the host to hit `:8080`.

### `docs/demo/kind-config.yaml`

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: webhookd-demo
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 30080
    hostPort: 8080
    protocol: TCP
  - containerPort: 30090
    hostPort: 9090
    protocol: TCP
```

The Service (below) uses NodePorts 30080/30090 so kind's port-mapping
exposes them to your laptop on `:8080` / `:9090`.

## kustomization.yaml

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: webhookd-demo

resources:
- namespace.yaml
- crd.yaml
- serviceaccount.yaml
- role.yaml
- rolebinding.yaml
- configmap.yaml
- secret.yaml
- deployment.yaml
- service.yaml
- mock-operator.yaml

# Optional: pin image tags in one place.
images:
- name: webhookd-demo
  newName: webhookd-demo
  newTag: dev
- name: mock-operator
  newName: mock-operator
  newTag: dev
```

## namespace.yaml

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: webhookd-demo
---
apiVersion: v1
kind: Namespace
metadata:
  name: wiz-operator
```

Two namespaces: webhookd-demo runs in `webhookd-demo`, applies
`SAMLGroupMapping` CRs in `wiz-operator` (where the Wiz operator
watches in production). Cross-namespace RBAC mirrors the production
pattern.

## crd.yaml

The canonical Wiz operator CRD copied verbatim into
[`kustomize/crd.yaml`](kustomize/crd.yaml). Group `wiz.rtkwlf.io`,
kind `SAMLGroupMapping`, with `spec.providerGroupId`,
`spec.identityProviderId` (required), `spec.roleRef.{name,roleId}`,
and `spec.projectRefs[].{name,projectId}`. The full file is ~140 lines
and includes printer columns for `Ready` / `Synced` / `Valid` / `Age`.

A reference instance is committed at
[`samlmapping.example.yaml`](samlmapping.example.yaml) — the shape the
JSM provider produces from a happy-path payload.

## configmap.yaml

The HCL config rendered into a ConfigMap, mounted by the Deployment.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: webhookd-config
data:
  webhookd.hcl: |
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
        endpoint = "otel-collector.observability:4317"
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
        kubeconfig_env       = ""               # in-cluster
        namespace            = "wiz-operator"
        identity_provider_id = "saml-idp-abc123"
        sync_timeout         = "20s"
      }
    }
```

## secret.yaml

Stubbed in the repo as `secret.yaml.example`; copy and fill in
locally — never commit real secrets.

### `secret.yaml.example`

```yaml
# Copy to secret.yaml and replace the value.
apiVersion: v1
kind: Secret
metadata:
  name: webhookd-signing
type: Opaque
stringData:
  WEBHOOK_DEMO_SECRET: "topsecret"
```

## RBAC

ServiceAccount in `webhookd-demo`; Role in `wiz-operator`; RoleBinding
that grants the SA permission cross-namespace.

### `serviceaccount.yaml`

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: webhookd-demo
```

### `role.yaml`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: webhookd-demo
  namespace: wiz-operator
rules:
- apiGroups: ["wiz.rtkwlf.io"]
  resources: ["samlgroupmappings"]
  verbs: ["get", "list", "watch", "create", "patch", "update"]
- apiGroups: ["wiz.rtkwlf.io"]
  resources: ["samlgroupmappings/status"]
  verbs: ["get", "patch", "update"]
```

### `rolebinding.yaml`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: webhookd-demo
  namespace: wiz-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: webhookd-demo
subjects:
- kind: ServiceAccount
  name: webhookd-demo
  namespace: webhookd-demo
```

## deployment.yaml

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: webhookd-demo
  labels:
    app: webhookd-demo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: webhookd-demo
  template:
    metadata:
      labels:
        app: webhookd-demo
    spec:
      serviceAccountName: webhookd-demo
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: webhookd-demo
        image: webhookd-demo:dev
        imagePullPolicy: IfNotPresent
        args: ["--config", "/etc/webhookd/webhookd.hcl"]
        ports:
        - name: public
          containerPort: 8080
        - name: admin
          containerPort: 9090
        env:
        - name: LOG_LEVEL
          value: "info"
        - name: WEBHOOK_DEMO_SECRET
          valueFrom:
            secretKeyRef:
              name: webhookd-signing
              key: WEBHOOK_DEMO_SECRET
        readinessProbe:
          httpGet:
            path: /readyz
            port: admin
        livenessProbe:
          httpGet:
            path: /healthz
            port: admin
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
          limits:
            cpu: 200m
            memory: 256Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop: [ALL]
          readOnlyRootFilesystem: true
        volumeMounts:
        - name: config
          mountPath: /etc/webhookd
          readOnly: true
      volumes:
      - name: config
        configMap:
          name: webhookd-config
```

## service.yaml

NodePorts so kind's port-mapping reaches the host.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: webhookd-demo
spec:
  type: NodePort
  selector:
    app: webhookd-demo
  ports:
  - name: public
    port: 8080
    targetPort: public
    nodePort: 30080
  - name: admin
    port: 9090
    targetPort: admin
    nodePort: 30090
```

## mock-operator.yaml

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mock-operator
  labels:
    app: mock-operator
spec:
  replicas: 1
  selector:
    matchLabels:
      app: mock-operator
  template:
    metadata:
      labels:
        app: mock-operator
    spec:
      serviceAccountName: webhookd-demo  # reuse the SA — same RBAC scope
      securityContext:
        runAsNonRoot: true
      containers:
      - name: mock-operator
        image: mock-operator:dev
        imagePullPolicy: IfNotPresent
        env:
        - name: DEMO_NAMESPACE
          value: wiz-operator
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop: [ALL]
          readOnlyRootFilesystem: true
```

## Deploy it

```bash
# 1. Bring up kind with the right port mappings.
just kind-up

# 2. Build the images (phase 11).
just bake

# 3. Load images into kind (kind doesn't pull from local dockerd).
kind load docker-image webhookd-demo:dev --name webhookd-demo
kind load docker-image mock-operator:dev --name webhookd-demo

# 4. Copy the secret stub and fill it in.
cp kustomize/secret.yaml.example kustomize/secret.yaml
# (edit WEBHOOK_DEMO_SECRET if you want)

# 5. Apply.
just deploy
# kubectl apply -k kustomize/

# 6. Watch.
kubectl get pods -n webhookd-demo -w
# webhookd-demo-...    1/1 Running
# mock-operator-...    1/1 Running
```

## What we proved

- [x] Cross-namespace RBAC pattern (Role in `wiz-operator`, SA in `webhookd-demo`)
- [x] Canonical Wiz CRD (`wiz.rtkwlf.io/v1alpha1.SAMLGroupMapping`) installed via kustomize
- [x] Distroless image runs as nonroot under restricted SecurityContext
- [x] HCL config carried in a ConfigMap; secret in a Secret
- [x] kind NodePorts expose `:8080` / `:9090` to the host
- [x] Mock operator deployment reuses the same RBAC scope

Next: [13-smoke-test.md](13-smoke-test.md) — end-to-end validation.
