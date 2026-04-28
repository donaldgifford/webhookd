// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

import "net/http"

// ResultKind classifies an executor outcome. Each kind maps to a
// distinct HTTP status code via HTTPStatus and a distinct response
// body status field ("success", "noop", "failure") via the dispatcher.
//
// There is intentionally no ResultTerminalFailure — the operator's
// status signal is binary (Ready=True or not), so webhookd cannot
// reliably distinguish "permanent failure" from "Wiz had a bad day"
// at watch time. Apply-step K8s errors (which are deterministic) keep
// distinct kinds; Ready=False at watch time falls through to
// ResultTimeout once the budget expires. See IMPL-0002 §Resolved
// Decisions §3 for the full rationale.
type ResultKind int

const (
	// ResultNoop — provider returned NoopAction; nothing to do.
	// HTTP 200 with body status "noop".
	ResultNoop ResultKind = iota

	// ResultReady — CR applied and Ready=True observed within budget.
	// HTTP 200 with body status "success".
	ResultReady

	// ResultTransientFailure — apply-step or watch-step error that
	// callers should retry (K8s API timeout, conflict, watch
	// disconnect). HTTP 503.
	ResultTransientFailure

	// ResultBadRequest — request body unreadable, malformed, or
	// rejected by IsInvalid at apply time. HTTP 400 for body errors,
	// 422 for spec-level rejects (the dispatcher distinguishes via
	// the wrapped sentinel).
	ResultBadRequest

	// ResultUnprocessable — semantic validation failure at the
	// provider layer (e.g., JSM custom field present but wrong type),
	// or apierrors.IsInvalid at apply time. HTTP 422.
	ResultUnprocessable

	// ResultInternalError — RBAC misconfiguration (IsForbidden) or
	// any other fault that won't go away on retry. HTTP 500. The
	// distinction from ResultTransientFailure matters: a 500 should
	// page a human, a 503 should just be retried.
	ResultInternalError

	// ResultTimeout — watch deadline expired before Ready=True. HTTP
	// 504. Distinct from ResultTransientFailure because the CR did
	// apply successfully; only the sync didn't complete in time.
	ResultTimeout
)

// String returns a stable, label-safe name for metrics and logs.
// Kept separate from %v so a renamed enum doesn't quietly change the
// metric label cardinality.
func (k ResultKind) String() string {
	switch k {
	case ResultNoop:
		return "noop"
	case ResultReady:
		return "ready"
	case ResultTransientFailure:
		return "transient_failure"
	case ResultBadRequest:
		return "bad_request"
	case ResultUnprocessable:
		return "unprocessable"
	case ResultInternalError:
		return "internal_error"
	case ResultTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// HTTPStatus returns the HTTP status code the dispatcher writes for k.
// Mapping mirrors DESIGN-0002 §HTTP Response Contract.
func (k ResultKind) HTTPStatus() int {
	switch k {
	case ResultNoop, ResultReady:
		return http.StatusOK
	case ResultBadRequest:
		return http.StatusBadRequest
	case ResultUnprocessable:
		return http.StatusUnprocessableEntity
	case ResultTransientFailure:
		return http.StatusServiceUnavailable
	case ResultTimeout:
		return http.StatusGatewayTimeout
	case ResultInternalError:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// ExecResult is the value flowing from the executor back to the
// dispatcher. Reason is included in the response body and as a span
// attribute; CR identity fields populate response body fields the
// JSM-side automation rule reads for ticket comments.
type ExecResult struct {
	Kind   ResultKind
	Reason string

	// CR identity, populated for ResultReady / ResultTransientFailure
	// / ResultTimeout (any path where a CR was actually applied or
	// targeted). Empty for ResultNoop and ResultBadRequest.
	CRName    string
	Namespace string

	// ObservedGeneration is the operator's last-seen spec generation.
	// Populated only for ResultReady; useful in operator-side logs
	// for cross-referencing.
	ObservedGeneration int64
}
