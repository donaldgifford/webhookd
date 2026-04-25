# webhookd — Phase 2 Walkthrough

A start-to-finish tour of Phase 2: how a JSM webhook becomes a `SAMLMapping` CR,
how the provider interface lets us add more sources cheaply, how the dispatcher
and executor split up responsibility, and how to add a second provider without
touching the JSM code.

Companion to: `docs/design/0002-jsm-samlmapping-provisioning.md`. Assumes
familiarity with the Phase 1 walkthrough
(`docs/design/0001-stateless-webhook-receiver-walkthrough.md`).

<!--toc:start-->
<!--toc:end-->

## 1. What Changed Since Phase 1

Phase 1 gave us the receiver: a signed webhook came in, we logged it, we
returned 202. That handler lived at one route and did one thing. Phase 2
replaces that handler with a dispatcher-plus- providers model and adds a
Kubernetes action path. Concretely:

- `/webhook/{provider}` now routes through a `Dispatcher` that looks up a
  registered `Provider` by name.
- Each provider is a package under `internal/webhook/<name>/` that implements
  the `webhook.Provider` interface.
- Providers are **pure parsers**: they turn a request body into an `Action`.
  They do no I/O.
- The `Executor` owns I/O: it reads an `Action` and applies it to Kubernetes,
  watches for sync, and returns a typed `ExecResult`.
- A Phase 1 observability-only path ("log and 202") still exists as the default
  `NoopAction` handling.

The payoff is that adding Slack (or any other provider) is a new package plus
one line in `main.go`. See §6 for the worked example.

## 2. Updated Source Tree

```
webhookd/
├── cmd/webhookd/main.go            # wiring + provider registration
└── internal/
    ├── config/config.go            # Phase 1 + Phase 2 env vars
    ├── observability/              # unchanged from Phase 1 + new metrics
    │   ├── logging.go
    │   ├── tracing.go
    │   └── metrics.go
    ├── httpx/                      # unchanged from Phase 1
    ├── k8s/
    │   └── client.go               # controller-runtime client setup
    └── webhook/
        ├── provider.go             # Provider interface
        ├── action.go               # Action union + concrete variants
        ├── dispatcher.go           # HTTP handler that routes to providers
        ├── executor.go             # Action -> K8s work, returns ExecResult
        ├── result.go               # ExecResult, classifyK8sErr
        └── jsm/
            ├── provider.go         # implements webhook.Provider
            ├── payload.go          # JSM JSON schema + Decode
            ├── extract.go          # custom-field extraction
            └── response.go         # JSM-specific response body
```

The key split is between `webhook/` (provider-agnostic: interface, dispatcher,
executor, result classification) and `webhook/<name>/` (provider-specific:
payload, parsing, extraction).

## 3. The Provider Interface

```go
// internal/webhook/provider.go
type Provider interface {
    Name() string
    VerifySignature(r *http.Request, body []byte) error
    Handle(ctx context.Context, body []byte) (Action, error)
}
```

Three methods, each with one responsibility.

- **`Name()`** returns the path segment the dispatcher routes on. For JSM it
  returns `"jsm"`, which matches `/webhook/jsm`. This string is also used as a
  metric label (`provider="jsm"`) and in log attributes.
- **`VerifySignature`** is the only method that reads headers directly. Every
  provider signs differently — JSM uses `X-Atlassian-Webhook-Identifier` plus
  HMAC over the body, Slack uses `X-Slack-Signature` over a canonical string
  including a timestamp, GitHub uses `X-Hub-Signature-256` — so the signature
  contract has to live in the provider. Return `nil` on success; anything else
  is treated as 401 by the dispatcher.
- **`Handle`** is **pure**. No K8s calls, no HTTP clients, no reading the clock
  (use the context's deadline if you need to). Take a verified body, produce an
  `Action` describing the work. This purity is what makes providers trivially
  testable:

  ```go
  func TestJSM_Handle_InvalidStatus(t *testing.T) {
      p := jsm.New(testConfig)
      body := fixture(t, "jsm_status_not_trigger.json")

      action, err := p.Handle(t.Context(), body)
      if err != nil {
          t.Fatalf("Handle = %v, want nil", err)
      }
      if _, ok := action.(webhook.NoopAction); !ok {
          t.Fatalf("Handle = %T, want NoopAction", action)
      }
  }
  ```

  No test server, no envtest, no fakes. If `Handle` needs K8s to produce its
  answer, you've put too much in the provider.

### 3.1 Why Split Handle and Execute?

The "decide what to do" vs "do it" split is worth dwelling on because it shapes
the rest of the code:

1. **Tests stay fast.** Provider unit tests are stdlib `testing` plus string
   fixtures. Executor tests use envtest. Each runs at the pace appropriate to
   what it's testing.
2. **Cross-cutting concerns live in one place.** The `k8s.apply` span, the
   CR-apply metrics, the retry classification — all happen inside the executor.
   If a second provider lands and also produces `ApplySAMLMapping` actions, it
   gets that behavior for free.
3. **The Action becomes the contract.** You can read every `Action` type and
   know every distinct kind of work webhookd performs. A junior engineer reading
   the codebase can answer "what does webhookd do?" by reading `action.go` in
   one sitting.

## 4. The Action Union

```go
// internal/webhook/action.go
type Action interface{ isAction() }

type NoopAction struct {
    Reason string // human-readable, goes in response body
}

type ApplySAMLMapping struct {
    IssueKey string
    Spec     wizv1alpha1.SAMLMappingSpec
    TraceCtx context.Context
}

func (NoopAction) isAction()        {}
func (ApplySAMLMapping) isAction()  {}
```

Phase 2 has two concrete actions:

- **`NoopAction`** — returned when a webhook is intentionally a no-op. For JSM,
  this is "the status didn't match the trigger." Returning an `Action` rather
  than a special error keeps the dispatcher's control flow uniform: it always
  gets an `Action` from `Handle` unless verification or decoding failed.
- **`ApplySAMLMapping`** — the only real work this phase does. The executor
  knows how to apply it and how to wait for sync.

The `isAction()` marker method is a standard Go pattern for tagged unions. It
prevents unrelated types from accidentally satisfying `Action` and lets the
executor's type switch be exhaustive-ish (the default case stays as a guard).

### 4.1 Adding a New Action Later

When Phase 3 adds Slack, Slack events probably don't need a CR — they might want
to update a ticket, post a message, or record a metric. New `Action` variant:

```go
type PostSlackAck struct {
    ChannelID string
    ThreadTS  string
    Message   string
}

func (PostSlackAck) isAction() {}
```

The executor gains a new case for it; providers that don't produce it don't
care. Crucially, adding this does not change the `Provider` interface — `Handle`
still returns `Action`.

## 5. The Dispatcher and Executor

### 5.1 Dispatcher — HTTP-facing

```go
// internal/webhook/dispatcher.go
type Dispatcher struct {
    providers map[string]Provider
    executor  *Executor
    metrics   *observability.Metrics
    maxBody   int64
}

func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    name := r.PathValue("provider") // Go 1.22+ pattern value

    p, ok := d.providers[name]
    if !ok {
        d.metrics.UnknownProvider.With(prometheus.Labels{
            "provider": name,
        }).Inc()
        http.Error(w, "unknown provider", http.StatusNotFound)
        return
    }

    body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, d.maxBody))
    if err != nil {
        writeProblem(w, http.StatusBadRequest, "body too large", "")
        return
    }

    if err := p.VerifySignature(r, body); err != nil {
        slog.WarnContext(ctx, "signature verification failed",
            "provider", p.Name(), "err", err)
        writeProblem(w, http.StatusUnauthorized, "signature invalid", "")
        return
    }

    action, err := p.Handle(ctx, body)
    if err != nil {
        d.writeProviderErr(ctx, w, p, err)
        return
    }

    result := d.executor.Execute(ctx, action)
    d.writeResult(ctx, w, p, action, result)
}
```

The dispatcher is the boring plumbing: routing, body reading, signature
verification, and response writing. It does not know what kind of `Action` it's
executing, only that the executor returns an `ExecResult` it can convert to
HTTP.

### 5.2 Executor — K8s-facing

```go
// internal/webhook/executor.go
type Executor struct {
    k8s      client.Client
    cfg      ExecutorConfig
    metrics  *observability.Metrics
    tracer   trace.Tracer
}

func (e *Executor) Execute(ctx context.Context, a Action) ExecResult {
    switch act := a.(type) {
    case NoopAction:
        return ExecResult{Kind: ResultNoop, Reason: act.Reason}

    case ApplySAMLMapping:
        return e.executeApplySAMLMapping(ctx, act)

    default:
        slog.ErrorContext(ctx, "unknown action type",
            "action_type", fmt.Sprintf("%T", a))
        return ExecResult{
            Kind:   ResultInternalError,
            Reason: "unknown action type",
        }
    }
}
```

Each case in the switch is a self-contained method. The `ApplySAMLMapping` case
is two operations — apply, then watch — and we span them separately so latency
breakdowns in Tempo make sense:

```go
func (e *Executor) executeApplySAMLMapping(
    ctx context.Context, a ApplySAMLMapping,
) ExecResult {
    // k8s.apply span
    ctx, applySpan := e.tracer.Start(ctx, "k8s.apply",
        trace.WithAttributes(
            attribute.String("k8s.resource.kind", "SAMLMapping"),
            attribute.String("k8s.resource.namespace", e.cfg.Namespace),
            attribute.String("jsm.issue_key", a.IssueKey),
        ),
    )
    applied, err := e.apply(ctx, a)
    applySpan.End()
    if err != nil {
        e.metrics.CRApply.With(prometheus.Labels{
            "kind": "SAMLMapping", "outcome": "error",
        }).Inc()
        return classifyK8sErr(err)
    }
    e.metrics.CRApply.With(prometheus.Labels{
        "kind": "SAMLMapping",
        "outcome": applyOutcome(applied), // created|updated|unchanged
    }).Inc()

    // k8s.watch_cr span
    ctx, watchSpan := e.tracer.Start(ctx, "k8s.watch_cr",
        trace.WithAttributes(
            attribute.String("k8s.resource.name", applied.Name),
            attribute.Int64("k8s.generation", applied.Generation),
        ),
    )
    result := e.waitForSync(ctx, applied)
    watchSpan.SetAttributes(
        attribute.String("k8s.sync.outcome", result.Kind.String()),
    )
    watchSpan.End()

    e.metrics.CRSyncDuration.With(prometheus.Labels{
        "kind":    "SAMLMapping",
        "outcome": result.Kind.String(),
    }).Observe(result.Duration.Seconds())

    return result
}
```

Two things to note about this shape:

- **The executor is the span-authoring boundary for K8s work.** Provider code
  never calls `tracer.Start`. If you ever see `tracer.Start` inside a provider's
  `Handle`, that's a smell — it's probably doing I/O it shouldn't be.
- **Metric labels are set from typed values, not user input.** `kind` is a
  hard-coded string. `outcome` is a typed constant converted via `String()`. No
  way for a malformed webhook to blow up cardinality.

## 6. Adding a Second Provider: Slack Worked Example

Say you want Slack slash-commands to trigger the same SAMLMapping action (useful
for ad-hoc requests from engineers who don't want to file a ticket). This is the
end-to-end change.

### 6.1 New Package

```
internal/webhook/slack/
├── provider.go      # implements webhook.Provider
├── signature.go     # Slack's signing-secret HMAC
└── parse.go         # parses slash-command payload
```

```go
// internal/webhook/slack/provider.go
type Provider struct {
    cfg Config
}

func New(cfg Config) *Provider { return &Provider{cfg: cfg} }

func (p *Provider) Name() string { return "slack" }

func (p *Provider) VerifySignature(r *http.Request, body []byte) error {
    // Slack's spec:
    // v0:<timestamp>:<body> signed with signing secret,
    // compared against X-Slack-Signature header.
    ts := r.Header.Get("X-Slack-Request-Timestamp")
    sig := r.Header.Get("X-Slack-Signature")
    return verifySlackSig(p.cfg.SigningSecret, ts, body, sig)
}

func (p *Provider) Handle(ctx context.Context, body []byte) (webhook.Action, error) {
    cmd, err := parseSlashCommand(body)
    if err != nil {
        return nil, errBadRequest(err)
    }
    if cmd.Command != "/provision-wiz" {
        return webhook.NoopAction{
            Reason: "unknown command " + cmd.Command,
        }, nil
    }

    spec, err := parseProvisionArgs(cmd.Text)
    if err != nil {
        return nil, errUnprocessable(err)
    }

    return webhook.ApplySAMLMapping{
        IssueKey: "slack-" + cmd.TriggerID,
        Spec:     spec,
        TraceCtx: ctx,
    }, nil
}
```

### 6.2 Config

Add Slack-specific env vars to `internal/config/config.go`, in the same pattern
as the JSM block:

```go
type Config struct {
    // ... existing ...
    Slack SlackConfig
}

type SlackConfig struct {
    SigningSecret []byte
    Enabled       bool
}
```

### 6.3 Registration in `main.go`

One line, sitting next to the existing JSM registration:

```go
dispatcher := webhook.NewDispatcher(
    webhook.WithProvider(jsm.New(cfg.JSM)),
    webhook.WithProvider(slack.New(cfg.Slack)),   // ← new
    webhook.WithExecutor(executor),
    webhook.WithMetrics(metrics),
    webhook.WithMaxBody(cfg.MaxBodyBytes),
)
publicMux.Handle("POST /webhook/{provider}", dispatcher)
```

That's it. The executor, metrics middleware, tracing, and the `ApplySAMLMapping`
code path all work unchanged. Slack inherits every bit of observability webhookd
already has.

### 6.4 What You Did Not Have To Touch

- The executor — because the action type is reused.
- The middleware chain — it's provider-agnostic.
- The observability package — metrics are per-provider already because the
  provider label is set by the dispatcher from `p.Name()`.
- The JSM package — no edits, no regressions.

If the new provider needed a new action type (say Slack needed to post a Slack
message back), you would add the action variant in `internal/webhook/action.go`
and a case in the executor's switch. That's still a small, additive change.

## 7. Request Lifecycle in Phase 2

Building on Phase 1's request timeline, the Phase 2 picture for a signed JSM
webhook hitting the trigger status looks like this:

```
  t=0ms  TCP accept → net/http → middleware chain (unchanged from Phase 1):
         │  Recover → otelhttp → RequestID → SLog → Metrics
         ▼
  t=1ms  Dispatcher.ServeHTTP:
         │    name = "jsm"
         │    p = providers["jsm"]
         │    body = ReadAll(MaxBytesReader(...))
         │    p.VerifySignature(r, body)    # HMAC check
         ▼
  t=2ms  p.Handle(ctx, body):               # jsm provider package
         │    decode JSON                   # jsm.decode_payload span
         │    check status == trigger
         │    extract team, project-roles   # jsm.extract_fields span
         │    return ApplySAMLMapping{...}
         ▼
  t=3ms  executor.Execute(ctx, action):
         │    switch on action type
         │    → executeApplySAMLMapping(ctx, act)
         ▼
  t=3ms  executor.apply():                  # k8s.apply span
         │    build SAMLMapping object
         │    client.Patch(ctx, obj, client.Apply, ...)
         │    K8s API admits, CR is created with generation=1
         ▼
  t=5ms  executor.waitForSync():            # k8s.watch_cr span
         │    Watch SAMLMapping by name
         │    ... operator reconciles ...
         ▼
  t=840ms  Watch receives status update:
         │    observedGeneration=1, Ready=True
         │    return ExecResult{Kind: ResultReady, Duration: 837ms}
         ▼
  t=841ms Dispatcher.writeResult:
         │    HTTP 200, JSON body with status, cr, trace_id
         ▼
  t=841ms Middleware unwind (unchanged from Phase 1):
         │    Metrics records outcome, SLog emits request line,
         │    otelhttp closes root span, Recover unwinds.
```

New spans introduced in Phase 2:

- `jsm.decode_payload` (inside the provider)
- `jsm.extract_fields` (inside the provider)
- `k8s.apply` (inside the executor)
- `k8s.watch_cr` (inside the executor)

Existing Phase 1 spans (the otelhttp root span, access log lines) carry through
unchanged. The root span now has ~840ms of duration instead of Phase 1's ~8ms
because of the sync wait — worth thinking about when you look at dashboards
after cutover. The p99 of the HTTP request duration histogram jumps from
"service latency" to "operator reconcile latency", which is mostly what you care
about anyway.

## 8. Extending the Dispatcher: When and When Not

The dispatcher is deliberately minimal. A checklist of "do I add this to the
dispatcher or the provider?"

**Goes in the dispatcher:**

- Body size limit (it's the same for all providers).
- Content-type enforcement (if we ever need it — all current providers use
  JSON).
- Per-provider rate limiting (Phase 3 concern; the dispatcher is the right place
  because it has the provider name before calling `Handle`).
- Top-level metrics that are uniform across providers (request count labeled by
  provider, body-too-large count).

**Goes in the provider:**

- Signature verification (inherently per-provider).
- Payload parsing (inherently per-provider).
- Trigger logic (which events produce actions vs NoopActions).
- Any domain-specific validation of fields.

**Goes in the executor:**

- Anything that touches Kubernetes.
- Anything that touches the Wiz API (when that day comes).
- Anything that needs the `k8s.*` span namespace.
- Retry classification.
- Sync-wait logic.

If you find yourself wanting to plumb a K8s client into a provider, that's the
design pushing back on you. The provider should return a richer `Action`
instead, and the executor should gain the capability.

## 9. What Still Is Not There

Same as the design doc, but worth repeating because it's the question people ask
when they see the provider interface and assume it implies more:

- **No async execution.** The dispatcher calls `executor.Execute(ctx, action)`
  synchronously, on the request goroutine. A channel-fed worker pool would break
  the JSM synchronous-response contract and duplicate the retry mechanism JSM
  already gives us. If we ever need async (for a provider that truly doesn't
  want a synchronous outcome, like a Slack events callback that only expects a
  200 ack), we add a second execution path — synchronous stays the default.
- **No plugin registry.** Registration is a hand-written line in `main.go`.
  That's deliberate. A plugin system makes sense if third parties write
  providers; they don't, we do.
- **No provider versioning.** Providers ship and upgrade with the binary. No
  compatibility surface to maintain.
- **No provider-level config schema.** Each provider reads `WEBHOOK_<NAME>_*`
  env vars the same way the root config does. No framework. No reflection.
  Thirty lines of `os.Getenv`.

Every one of those omissions has a cost to add and a very small benefit at our
scale. If that calculus ever flips, it flips for a specific, named reason — not
because "the architecture should be more extensible."
