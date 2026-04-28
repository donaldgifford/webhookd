// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm

import (
	"net/http"
	"time"

	"github.com/donaldgifford/webhookd/internal/webhook"
)

// SignatureConfig is the narrow set of values the JSM signature
// verifier needs. It mirrors `config.Config` knobs but takes them by
// value so tests can construct it without env-var wrangling.
type SignatureConfig struct {
	// SecretBytes is the shared HMAC secret. We take []byte rather
	// than string so callers can clear it from memory after wiring.
	SecretBytes []byte

	// SigHeader is the request header carrying the `sha256=<hex>`
	// signature. Defaults to webhookd's `X-Webhook-Signature`; JSM
	// tenants signing via Slack-style automation rules should send
	// the same header.
	SigHeader string

	// TSHeader carries the unix-seconds timestamp included in the
	// canonical message. Defaults to `X-Webhook-Timestamp`.
	TSHeader string

	// Skew bounds replay protection. The same value is reused from
	// `config.Config.TimestampSkew`.
	Skew time.Duration

	// Now is the clock used for skew comparisons. Tests inject a
	// fixed clock; production code defaults to time.Now.
	Now func() time.Time
}

// VerifySignature runs the project-wide v0:<ts>:<body> HMAC-SHA256
// scheme against the headers configured for the JSM provider. Returns
// the same sentinel set webhook.Verify does so the dispatcher can
// classify with errors.Is.
//
// Implementations note: JSM is configured via its automation rule to
// sign with our v0 contract — there is no JSM-native HMAC scheme
// webhookd has to interoperate with. If a tenant ever needs a
// different scheme, swap this function out (the rest of the package
// stays).
func VerifySignature(r *http.Request, body []byte, cfg SignatureConfig) error {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return webhook.Verify(
		cfg.SecretBytes,
		r.Header.Get(cfg.SigHeader),
		r.Header.Get(cfg.TSHeader),
		body,
		now(),
		cfg.Skew,
	)
}
