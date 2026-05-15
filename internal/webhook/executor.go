// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/donaldgifford/webhookd/internal/httpx"
	"github.com/donaldgifford/webhookd/internal/observability"
)

// tracerName is the instrumentation-scope name for spans the executor
// emits. Stable; dashboards filter on this.
const tracerName = "github.com/donaldgifford/webhookd/internal/webhook"

// Annotation and label keys stamped onto every CR webhookd applies.
// `webhookd.io/` is the project-namespaced prefix for everything
// webhookd writes; the operator's annotations live in its own
// `wiz.webhookd.io/` namespace.
//
// AnnotationIssue was historically defined here but is JSM-specific;
// per INV-0003 §F-03 it now lives in internal/webhook/jsm/ and is
// merged onto the CR via ApplyAction.Annotations.
const (
	LabelManagedBy    = "webhookd.io/managed-by"
	LabelSource       = "webhookd.io/source"
	AnnotationTraceID = "webhookd.io/trace-id"
	AnnotationReqID   = "webhookd.io/request-id"
	AnnotationApplied = "webhookd.io/applied-at"
)

// ExecutorConfig is the narrow configuration the executor needs. It
// is constructed from *config.Config in main.go — the executor itself
// never touches the full Config, mirroring the rest of the project.
type ExecutorConfig struct {
	// FieldManager is the SSA fieldManager identity. Defaults to
	// "webhookd"; lets ops distinguish webhookd-applied fields from
	// operator-applied or human-applied ones in managedFields.
	FieldManager string

	// SyncTimeout caps the watch loop. Strictly less than the
	// shutdown timeout so a SIGTERM during a long sync still drains
	// within budget.
	SyncTimeout time.Duration

	// Now defaults to time.Now; tests override it for deterministic
	// `applied-at` annotations.
	Now func() time.Time
}

// Executor executes Actions against Kubernetes. It uses a single
// controller-runtime client.WithWatch for both SSA Patch / Get and
// the Watch loop — typed throughout via the shared k8s Scheme.
//
// The executor is generic over CR Kind: every Apply path goes through
// ApplyAction which carries the typed object + the readiness
// predicate the provider supplied. The executor never imports a
// concrete CR package, so adding a new provider does not require
// touching this file. See INV-0003 §F-02.
type Executor struct {
	ctrlClient client.WithWatch
	logger     *slog.Logger
	metrics    *observability.Metrics
	cfg        ExecutorConfig
}

// NewExecutor returns an Executor wired against the supplied client.
// The caller is responsible for ensuring `Scheme` (from internal/k8s)
// is the scheme behind ctrlClient — typed Patch on provider-supplied
// CRs won't work otherwise.
//
// metrics may be nil; tests use the nil shorthand to skip metric
// observations entirely. Production wires it through from
// `observability.NewMetrics`.
func NewExecutor(
	ctrlClient client.WithWatch,
	logger *slog.Logger,
	metrics *observability.Metrics,
	cfg ExecutorConfig,
) *Executor {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.FieldManager == "" {
		cfg.FieldManager = "webhookd"
	}
	return &Executor{
		ctrlClient: ctrlClient,
		logger:     logger,
		metrics:    metrics,
		cfg:        cfg,
	}
}

// Execute runs the supplied Action. The returned ExecResult carries
// everything the dispatcher needs to write the response.
func (e *Executor) Execute(ctx context.Context, a Action) ExecResult {
	switch act := a.(type) {
	case NoopAction:
		return ExecResult{Kind: ResultNoop, Reason: act.Reason}
	case ApplyAction:
		applied, err := e.apply(ctx, &act)
		if err != nil {
			e.observeApply(act.Kind, "error")
			return classifyK8sErr(err, act.Object.GetNamespace(), act.Object.GetName())
		}
		e.observeApply(act.Kind, applyOutcome(applied))
		return e.waitForSync(ctx, &act, applied)
	default:
		return ExecResult{Kind: ResultInternalError, Reason: fmt.Sprintf("unknown action %T", a)}
	}
}

// apply merges executor-managed labels + annotations into the
// provider's typed Object and SSA-patches it. After Patch, an
// explicit Get refetches so the caller has the authoritative
// `metadata.generation` regardless of in-place mutation semantics.
// act is taken by pointer to avoid copying the embedded client.Object
// field on every call.
func (e *Executor) apply(ctx context.Context, act *ApplyAction) (client.Object, error) {
	obj := act.Object

	ctx, span := otel.Tracer(tracerName).Start(ctx, "k8s.apply",
		trace.WithAttributes(
			attribute.String("k8s.resource.kind", act.Kind),
			attribute.String("k8s.resource.namespace", obj.GetNamespace()),
			attribute.String("k8s.resource.name", obj.GetName()),
		),
	)
	defer span.End()

	obj.SetLabels(mergeLabels(obj.GetLabels(), systemLabels(act.Source)))
	obj.SetAnnotations(mergeAnnotations(obj.GetAnnotations(), act.Annotations, e.systemAnnotations(ctx)))

	// client.Apply is deprecated in favor of client.Client.Apply(), but
	// the new API takes an ApplyConfiguration which is generated only
	// for client-go-managed types. For typed CRDs without generated
	// ApplyConfigurations, the Patch+client.Apply path remains the
	// supported approach.
	//
	//nolint:staticcheck // SA1019: typed-CR SSA path; new Apply API requires generated ApplyConfiguration.
	if err := e.ctrlClient.Patch(ctx, obj,
		client.Apply,
		client.FieldOwner(e.cfg.FieldManager),
		client.ForceOwnership,
	); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ssa patch")
		span.SetAttributes(attribute.String("webhookd.outcome", "error"))
		return nil, fmt.Errorf("ssa patch: %w", err)
	}

	if err := e.ctrlClient.Get(ctx, client.ObjectKey{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}, obj); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "post-apply get")
		span.SetAttributes(attribute.String("webhookd.outcome", "error"))
		return nil, fmt.Errorf("post-apply get: %w", err)
	}
	span.SetAttributes(
		attribute.Int64("k8s.generation", obj.GetGeneration()),
		attribute.String("webhookd.outcome", applyOutcome(obj)),
	)
	return obj, nil
}

// waitForSync watches the supplied CR until either ReadyCheck reports
// ready (with observedGeneration >= the apply'd generation) or the
// timeout deadline expires.
//
// The watch step is binary by design: anything other than Ready=True
// is treated as still-pending. The Wiz API the operator talks to gives
// no way to distinguish "permanent failure" from "Wiz had a bad day,"
// so terminal-vs-transient classification lives at the apply step
// only.
//
// Implementation: single namespace-scoped Watch backed by an initial
// Get to close the race window between Patch and Watch establishing.
// We deliberately don't use cache.Reflector / tools/watch.UntilWithSync
// — those depend on streaming-list bookmarks (WatchListClient feature
// gate, default-on in client-go v0.35+) that our custom ListWatch
// can't supply, and our hard deadline obviates Reflector's
// auto-reconnect logic.
func (e *Executor) waitForSync(parent context.Context, act *ApplyAction, applied client.Object) ExecResult {
	ctx, cancel := context.WithTimeout(parent, e.cfg.SyncTimeout)
	defer cancel()

	applyGen := applied.GetGeneration()
	ctx, span := otel.Tracer(tracerName).Start(ctx, "k8s.watch_cr",
		trace.WithAttributes(
			attribute.String("k8s.resource.kind", act.Kind),
			attribute.String("k8s.resource.namespace", applied.GetNamespace()),
			attribute.String("k8s.resource.name", applied.GetName()),
			attribute.Int64("k8s.generation", applyGen),
		),
	)
	start := time.Now()
	base := ExecResult{CRName: applied.GetName(), Namespace: applied.GetNamespace()}
	defer func() {
		outcome := syncOutcome(base.Kind)
		span.SetAttributes(attribute.String("k8s.sync.outcome", outcome))
		span.End()
		e.observeSync(act.Kind, outcome, time.Since(start))
	}()

	// Initial Get covers the case where ready was set between our SSA
	// Patch and the Watch establishing. Errors here aren't fatal — the
	// watch loop will retry naturally. We DeepCopy the applied object
	// to get a blank-shaped Object of the right type for the Get.
	cur, ok := applied.DeepCopyObject().(client.Object)
	if ok {
		if err := e.ctrlClient.Get(ctx, client.ObjectKey{
			Namespace: applied.GetNamespace(), Name: applied.GetName(),
		}, cur); err == nil {
			if ready, observedGen := act.ReadyCheck(cur, applyGen); ready {
				base.Kind = ResultReady
				base.ObservedGeneration = observedGen
				return base
			}
		}
	}

	w, err := e.ctrlClient.Watch(ctx, act.ListObject, client.InNamespace(applied.GetNamespace()))
	if err != nil {
		base.Kind = ResultTransientFailure
		base.Reason = "watch: " + err.Error()
		return base
	}
	defer w.Stop()

	for {
		select {
		case <-ctx.Done():
			base.Kind = ResultTimeout
			base.Reason = fmt.Sprintf("operator did not mark Ready=True within %s", e.cfg.SyncTimeout)
			return base
		case ev, ok := <-w.ResultChan():
			if !ok {
				base.Kind = ResultTransientFailure
				base.Reason = "watch channel closed before sync"
				if e.logger != nil {
					e.logger.WarnContext(parent, "watch closed",
						"cr", applied.GetName())
				}
				return base
			}
			obj, ok := ev.Object.(client.Object)
			if !ok || obj.GetName() != applied.GetName() {
				continue
			}
			if ready, observedGen := act.ReadyCheck(obj, applyGen); ready {
				base.Kind = ResultReady
				base.ObservedGeneration = observedGen
				return base
			}
		}
	}
}

// systemLabels returns the executor-managed label set: managed-by =
// "webhookd" (constant) and source = the provider name. Callers merge
// this with any labels the provider's Object already carries.
func systemLabels(source string) map[string]string {
	return map[string]string{
		LabelManagedBy: "webhookd",
		LabelSource:    source,
	}
}

// systemAnnotations returns the per-request annotation set the
// executor stamps on every CR: trace-id from the active span (if
// any), request-id from the middleware-stamped context, and a
// deterministic apply timestamp.
func (e *Executor) systemAnnotations(ctx context.Context) map[string]string {
	out := map[string]string{
		AnnotationApplied: e.cfg.Now().UTC().Format(time.RFC3339),
	}
	if reqID := httpx.RequestIDFromContext(ctx); reqID != "" {
		out[AnnotationReqID] = reqID
	}
	if tid := traceIDFromContext(ctx); tid != "" {
		out[AnnotationTraceID] = tid
	}
	return out
}

// mergeLabels combines maps left-to-right, with later maps winning
// on key conflicts. The provider's labels are preserved unless the
// executor explicitly overrides them.
func mergeLabels(provider, executor map[string]string) map[string]string {
	out := make(map[string]string, len(provider)+len(executor))
	maps.Copy(out, provider)
	maps.Copy(out, executor)
	return out
}

// mergeAnnotations combines maps left-to-right; same precedence as
// mergeLabels but kept distinct so callers reading either site see
// the intended composition order.
func mergeAnnotations(provider, providerExtra, executor map[string]string) map[string]string {
	out := make(map[string]string, len(provider)+len(providerExtra)+len(executor))
	maps.Copy(out, provider)
	maps.Copy(out, providerExtra)
	maps.Copy(out, executor)
	return out
}

// traceIDFromContext extracts the active OTel trace ID, or "" when no
// valid span is on ctx. Stable across log records and CR annotations
// so a single trace ID correlates the JSM webhook request, the K8s
// audit log, and the operator's reconcile traces.
func traceIDFromContext(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// syncOutcome maps a watch-step ResultKind onto the histogram label
// (`ready` | `timeout` | `transient`).
func syncOutcome(kind ResultKind) string {
	switch kind {
	case ResultReady:
		return "ready"
	case ResultTimeout:
		return "timeout"
	default:
		return "transient"
	}
}

// observeApply increments the apply counter on the configured outcome
// label, parametric over CR Kind. nil-safe so tests can pass nil
// metrics.
func (e *Executor) observeApply(kind, outcome string) {
	if e.metrics == nil {
		return
	}
	e.metrics.K8sApplyTotal.WithLabelValues(kind, outcome).Inc()
}

// observeSync histograms the watch-loop duration on the configured
// outcome label, parametric over CR Kind.
func (e *Executor) observeSync(kind, outcome string, d time.Duration) {
	if e.metrics == nil {
		return
	}
	e.metrics.K8sSyncDuration.WithLabelValues(kind, outcome).Observe(d.Seconds())
}

// applyOutcome derives the apply-step Prometheus label from the
// object's metadata.generation. SSA noops produce the same generation
// as the prior state; detecting "unchanged" rigorously would require
// the provider-supplied Status.ObservedGeneration which the executor
// is generic over. Phase 2 keeps this lossy (everything past 1 is
// "updated") until we plumb a per-action OutcomeOf hook.
func applyOutcome(obj client.Object) string {
	switch obj.GetGeneration() {
	case 0:
		// Shouldn't happen post-Get, but defensively distinct.
		return "error"
	case 1:
		return "created"
	default:
		return "updated"
	}
}

// classifyK8sErr maps an apply-step error to the right ResultKind.
// Keep this deterministic — apply-step errors are how we surface
// permanent rejections (422 / 500); over-classifying as transient
// would let JSM retry forever into a wall.
//
// The default arm returns ResultInternalError rather than
// ResultTransientFailure: an error the K8s SDK doesn't classify as
// IsServerTimeout / IsServiceUnavailable / IsTooManyRequests / IsConflict
// is most likely a deterministic bug (nil-pointer, malformed REST
// config, scheme drift). Calling it transient invites JSM retry storms
// against an issue a retry won't fix; surfacing 500 pages a human
// instead. See INV-0003 §F-11.
func classifyK8sErr(err error, namespace, name string) ExecResult {
	switch {
	case apierrors.IsForbidden(err):
		return ExecResult{
			Kind: ResultInternalError, Reason: "forbidden: " + err.Error(),
			CRName: name, Namespace: namespace,
		}
	case apierrors.IsInvalid(err):
		return ExecResult{
			Kind: ResultUnprocessable, Reason: "invalid spec: " + err.Error(),
			CRName: name, Namespace: namespace,
		}
	case apierrors.IsServerTimeout(err),
		apierrors.IsServiceUnavailable(err),
		apierrors.IsTooManyRequests(err),
		apierrors.IsConflict(err):
		return ExecResult{
			Kind: ResultTransientFailure, Reason: err.Error(),
			CRName: name, Namespace: namespace,
		}
	default:
		return ExecResult{
			Kind: ResultInternalError, Reason: err.Error(),
			CRName: name, Namespace: namespace,
		}
	}
}
