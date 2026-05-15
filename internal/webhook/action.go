// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

import (
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Action is a typed union of work the executor knows how to perform.
// The unexported sentinel method prevents external packages from
// adding variants the executor doesn't know about; new variants must
// land in this package alongside the matching switch arm in the
// executor.
//
// Phase 2 ships two variants: NoopAction (no-op acknowledgement) and
// ApplyAction (the generic CR-provisioning path). ApplyAction is
// opaque over CR Kind — providers build their own typed
// client.Object and supply the readiness predicate; the executor
// never imports any CR-specific package. See INV-0003 §F-01 / §F-02.
type Action interface {
	isAction()
}

// NoopAction is what providers return when the webhook was received,
// authenticated, and parsed, but intentionally does nothing — for
// example, a JSM ticket whose status doesn't match the configured
// trigger. The dispatcher responds 200 with `status: "noop"` so JSM
// (or any retrying caller) advances the ticket without retrying.
type NoopAction struct {
	// Reason is human-readable context surfaced in the response body
	// and the noop metric. Operators read it to spot misconfigured
	// automation rules.
	Reason string
}

func (NoopAction) isAction() {}

// ApplyAction is the generic side-effectful action: the provider
// builds a typed K8s object, the executor SSA-applies it and watches
// for readiness. The executor stays CR-Kind-agnostic by treating
// Object / ListObject as plain client.Object / client.ObjectList
// values and delegating the readiness predicate back to the provider
// via ReadyCheck.
//
// All provider-specific annotations (e.g. `webhookd.io/jsm-issue-key`)
// belong in Annotations; the executor merges them with its own
// request-scoped annotations (trace-id, request-id, applied-at) before
// the Patch. See INV-0003 §F-03.
type ApplyAction struct {
	// Object is the desired CR, fully constructed by the provider.
	// Must have Name + Namespace set on its ObjectMeta. The executor
	// merges in its own labels + annotations before SSA Apply.
	Object client.Object

	// ListObject is an empty list of the same Kind, used to back the
	// post-Apply Watch call. Typically `&someCRDList{}` matching
	// Object's underlying type.
	ListObject client.ObjectList

	// ReadyCheck inspects a Watch event's object and reports whether
	// the operator has marked it ready relative to applyGen (the
	// generation produced by webhookd's Apply). Returns the observed
	// generation alongside readiness so the executor can populate
	// ExecResult.ObservedGeneration for the response body.
	//
	// The watch step is binary by design (see IMPL-0002 §Resolved
	// Decisions §3) — providers should not try to classify "permanent
	// failure" here.
	ReadyCheck func(obj client.Object, applyGen int64) (ready bool, observedGen int64)

	// Annotations are provider-specific keys merged onto the object's
	// metadata.annotations before SSA Apply. The executor always
	// stamps its own request-scoped annotations on top; this map is
	// for domain-specific keys like `webhookd.io/jsm-issue-key`.
	Annotations map[string]string

	// Kind is the K8s Kind label value for the apply / sync
	// Prometheus metrics. Bounded by the set of registered providers,
	// so cardinality is safe.
	Kind string

	// Source identifies the producing provider (e.g. "jsm"). The
	// executor stamps it as the `webhookd.io/source` label on every
	// CR so operators can run `kubectl get -l webhookd.io/source=jsm`.
	Source string
}

func (ApplyAction) isAction() {}

// ErrBadRequest wraps payload-parse errors that map to HTTP 400.
// Providers return errors that wrap this sentinel; the dispatcher
// classifies via errors.Is. Wrapping keeps the underlying message
// available for logging and the response body's reason field.
var ErrBadRequest = errors.New("bad request")

// ErrUnprocessable wraps payload-validation errors that map to HTTP
// 422. The payload was syntactically valid (so not 400) but
// semantically rejectable — for example, a JSM custom field present
// but containing the wrong type. Distinct from 400 because retrying
// the same payload won't help.
var ErrUnprocessable = errors.New("unprocessable")
