// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

import (
	"context"
	"net/http"
)

// Provider is the seam between webhookd's transport (the dispatcher)
// and the per-source decoding/signature logic. Phase 2 ships exactly
// one implementation (`internal/webhook/jsm`); Phase 3+ adds more
// without changing this contract.
//
// Implementations must be:
//
//   - Goroutine-safe — the dispatcher concurrently calls VerifySignature
//     and Handle from many request goroutines.
//   - Pure in Handle — no I/O against Kubernetes, the network, or any
//     other side-effectful system. The dispatcher executes the
//     returned Action via the executor; cross-cutting concerns
//     (spans, metrics, retry classification) live there, not in
//     providers.
//
// See IMPL-0002 §Phase 2 (Provider Interface & Action Union) for the
// architectural rationale.
type Provider interface {
	// Name is the URL path segment that routes to this provider, e.g.
	// "jsm". Must equal the registration key in the dispatcher and
	// is used as a metrics label.
	Name() string

	// VerifySignature validates the request's authenticity using the
	// provider's own conventions (header names, canonical body shape,
	// HMAC algorithm). It must be timing-safe — providers should
	// reuse hmac.Equal or equivalent. A nil return means the request
	// is authentic; any error means reject with 401.
	//
	// body is the already-read request body, bounded by
	// WEBHOOK_MAX_BODY_BYTES. Implementations must not consume r.Body.
	VerifySignature(r *http.Request, body []byte) error

	// Handle decodes the verified body and decides what work to do.
	// Returns one of:
	//   - NoopAction: the webhook was understood but intentionally
	//     does nothing (e.g., wrong status transition). The dispatcher
	//     responds 200 with status: "noop".
	//   - ApplySAMLGroupMapping (or future Action variants): work the
	//     executor will perform.
	//   - non-nil error: malformed payload (400 / ResultBadRequest)
	//     or internal error (500 / ResultInternalError). The
	//     dispatcher classifies via errors.Is checks against this
	//     package's sentinels.
	Handle(ctx context.Context, body []byte) (Action, error)
}
