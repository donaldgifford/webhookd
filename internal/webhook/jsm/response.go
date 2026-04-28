// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm

import (
	"github.com/donaldgifford/webhookd/internal/webhook"
)

// ResponseBody is the JSON shape webhookd writes back to JSM. JSM's
// automation rule reads `status` to decide whether to add a comment
// and `crName` / `namespace` / `traceId` to put in that comment.
//
// The shape is provider-specific even though the executor is
// provider-agnostic — different downstream callers (Slack, GitHub)
// will want different bodies, and pushing this into the dispatcher
// would force a switch over provider.
type ResponseBody struct {
	// Status is "success" | "noop" | "failure". Mapped from
	// `webhook.ResultKind` by Build.
	Status string `json:"status"`

	// Reason is human-readable context, populated for noop and failure
	// outcomes. Empty for success.
	Reason string `json:"reason,omitempty"`

	// CRName / Namespace identify the CR webhookd applied. Populated
	// for ResultReady, ResultTimeout, and ResultTransientFailure —
	// any kind where we actually got far enough to know the target.
	CRName    string `json:"crName,omitempty"`
	Namespace string `json:"namespace,omitempty"`

	// ObservedGeneration is the operator's last-seen spec generation.
	// Populated only for success — used in JSM comments to confirm
	// the operator caught up.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// TraceID and RequestID are stamped onto every response so JSM
	// automation comments include a one-click pivot into Tempo /
	// Loki. The dispatcher passes them in from the per-request
	// context.
	TraceID   string `json:"traceId,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Build constructs the ResponseBody from an ExecResult plus
// per-request trace and request IDs. Status text is derived from
// ResultKind:
//
//   - ResultNoop → "noop"
//   - ResultReady → "success"
//   - everything else → "failure"
//
// Pure function; no I/O.
func Build(res webhook.ExecResult, traceID, requestID string) ResponseBody {
	body := ResponseBody{
		Reason:             res.Reason,
		CRName:             res.CRName,
		Namespace:          res.Namespace,
		ObservedGeneration: res.ObservedGeneration,
		TraceID:            traceID,
		RequestID:          requestID,
	}
	switch res.Kind {
	case webhook.ResultNoop:
		body.Status = "noop"
	case webhook.ResultReady:
		body.Status = "success"
	default:
		body.Status = "failure"
	}
	return body
}
