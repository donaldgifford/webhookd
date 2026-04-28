// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/donaldgifford/webhookd/internal/httpx"
	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// Annotation and label keys stamped onto every CR webhookd applies.
// `webhookd.io/` is the project-namespaced prefix for everything
// webhookd writes; the operator's annotations live in its own
// `wiz.webhookd.io/` namespace.
const (
	LabelManagedBy    = "webhookd.io/managed-by"
	LabelSource       = "webhookd.io/source"
	AnnotationTraceID = "webhookd.io/trace-id"
	AnnotationReqID   = "webhookd.io/request-id"
	AnnotationIssue   = "webhookd.io/jsm-issue-key"
	AnnotationApplied = "webhookd.io/applied-at"
)

// ExecutorConfig is the narrow configuration the executor needs. It
// is constructed from *config.Config in main.go — the executor itself
// never touches the full Config, mirroring the rest of the project.
type ExecutorConfig struct {
	// Namespace is where SAMLGroupMapping CRs are applied.
	Namespace string

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
// the cache.ListWatch backing tools/watch.UntilWithSync — typed
// throughout, so the operator's CRD shape is enforced at compile
// time.
type Executor struct {
	ctrlClient client.WithWatch
	logger     *slog.Logger
	cfg        ExecutorConfig
}

// NewExecutor returns an Executor wired against the supplied client.
// The caller is responsible for ensuring `Scheme` (from internal/k8s)
// is the scheme behind ctrlClient — typed Patch on SAMLGroupMapping
// won't work otherwise.
func NewExecutor(
	ctrlClient client.WithWatch,
	logger *slog.Logger,
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
		cfg:        cfg,
	}
}

// Execute runs the supplied Action. The returned ExecResult carries
// everything the dispatcher needs to write the JSM response.
func (e *Executor) Execute(ctx context.Context, a Action) ExecResult {
	switch act := a.(type) {
	case NoopAction:
		return ExecResult{Kind: ResultNoop, Reason: act.Reason}
	case ApplySAMLGroupMapping:
		applied, err := e.apply(ctx, &act)
		if err != nil {
			return classifyK8sErr(err, e.cfg.Namespace, crName(act.IssueKey))
		}
		return e.waitForSync(ctx, applied)
	default:
		return ExecResult{Kind: ResultInternalError, Reason: fmt.Sprintf("unknown action %T", a)}
	}
}

// apply builds the typed CR and SSA-patches it. After Patch, an
// explicit Get refetches the object so the caller has the
// authoritative `metadata.generation` regardless of in-place mutation
// semantics. act is taken by pointer to avoid copying the embedded
// SAMLGroupMappingSpec on every call.
func (e *Executor) apply(ctx context.Context, act *ApplySAMLGroupMapping) (*wizapi.SAMLGroupMapping, error) {
	obj := &wizapi.SAMLGroupMapping{
		TypeMeta: metav1.TypeMeta{
			APIVersion: wizapi.GroupVersion.String(),
			Kind:       "SAMLGroupMapping",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        crName(act.IssueKey),
			Namespace:   e.cfg.Namespace,
			Labels:      labels(),
			Annotations: e.annotations(ctx, act.IssueKey),
		},
		Spec: act.Spec,
	}

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
		return nil, fmt.Errorf("ssa patch: %w", err)
	}

	got := &wizapi.SAMLGroupMapping{}
	if err := e.ctrlClient.Get(ctx, client.ObjectKey{
		Namespace: e.cfg.Namespace,
		Name:      obj.Name,
	}, got); err != nil {
		return nil, fmt.Errorf("post-apply get: %w", err)
	}
	return got, nil
}

// waitForSync watches the supplied CR until either Ready=True is
// observed (with observedGeneration >= the apply'd generation) or the
// timeout deadline expires.
//
// The watch step is binary by design: Ready=False with any reason is
// treated as still-pending. The Wiz API the operator talks to gives
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
func (e *Executor) waitForSync(parent context.Context, applied *wizapi.SAMLGroupMapping) ExecResult {
	ctx, cancel := context.WithTimeout(parent, e.cfg.SyncTimeout)
	defer cancel()

	base := ExecResult{CRName: applied.Name, Namespace: applied.Namespace}

	// Initial Get covers the case where Ready=True was set between our
	// SSA Patch and the Watch establishing. Errors here aren't fatal —
	// the watch loop will retry naturally.
	cur := &wizapi.SAMLGroupMapping{}
	if err := e.ctrlClient.Get(ctx, client.ObjectKey{
		Namespace: applied.Namespace, Name: applied.Name,
	}, cur); err == nil && isReady(cur, applied.Generation) {
		base.Kind = ResultReady
		base.ObservedGeneration = cur.Status.ObservedGeneration
		return base
	}

	list := &wizapi.SAMLGroupMappingList{}
	w, err := e.ctrlClient.Watch(ctx, list, client.InNamespace(applied.Namespace))
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
						"cr", applied.Name)
				}
				return base
			}
			obj, ok := ev.Object.(*wizapi.SAMLGroupMapping)
			if !ok || obj.Name != applied.Name {
				continue
			}
			if isReady(obj, applied.Generation) {
				base.Kind = ResultReady
				base.ObservedGeneration = obj.Status.ObservedGeneration
				return base
			}
		}
	}
}

// isReady reports whether obj has been marked Ready=True with an
// observedGeneration at least as high as the supplied apply
// generation. Encapsulated separately so waitForSync's initial Get
// and watch loop share one definition.
func isReady(obj *wizapi.SAMLGroupMapping, applyGen int64) bool {
	if obj.Status.ObservedGeneration < applyGen {
		return false
	}
	for i := range obj.Status.Conditions {
		c := obj.Status.Conditions[i]
		if c.Type != wizapi.ConditionTypeReady {
			continue
		}
		return c.Status == metav1.ConditionTrue
	}
	return false
}

// labels returns the constant set stamped onto every CR for
// `kubectl get -l webhookd.io/source=jsm`-style queries.
func labels() map[string]string {
	return map[string]string{
		LabelManagedBy: "webhookd",
		LabelSource:    "jsm",
	}
}

// annotations returns the per-request annotation set: trace-id from
// the active span (if any), request-id from the middleware-stamped
// context, the JSM issue key, and a deterministic apply timestamp.
func (e *Executor) annotations(ctx context.Context, issueKey string) map[string]string {
	out := map[string]string{
		AnnotationIssue:   issueKey,
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

// crName normalizes a JSM issue key into a DNS-1123-safe CR name.
// JSM keys are `[A-Z]+-[0-9]+`; lowercasing produces a valid label
// directly. Anything outside `[a-z0-9-]` is replaced with `-` to
// defend against future free-form keys.
func crName(issueKey string) string {
	lower := strings.ToLower(issueKey)
	var b strings.Builder
	b.Grow(len("jsm-") + len(lower))
	b.WriteString("jsm-")
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// classifyK8sErr maps an apply-step error to the right ResultKind.
// Keep this deterministic — apply-step errors are how we surface
// permanent rejections (422 / 500); over-classifying as transient
// would let JSM retry forever into a wall.
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
			Kind: ResultTransientFailure, Reason: err.Error(),
			CRName: name, Namespace: namespace,
		}
	}
}
