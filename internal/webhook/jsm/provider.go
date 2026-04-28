// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/donaldgifford/webhookd/internal/observability"
	"github.com/donaldgifford/webhookd/internal/webhook"
)

// Config is the narrow per-provider configuration the JSM provider
// needs at construction time. Built from the global *config.Config in
// main.go via a small adapter — the provider never reads env directly.
type Config struct {
	// TriggerStatus is the JSM ticket status that fires
	// `ApplySAMLGroupMapping`. Anything else returns NoopAction.
	TriggerStatus string

	// Custom-field IDs used to extract spec values from the issue.
	// All three are tenant-specific (`customfield_NNNNN`).
	FieldProviderGroupID string
	FieldRole            string
	FieldProject         string

	// IdentityProviderID is stamped onto every CR's
	// `spec.identityProviderId`. Static per webhookd install.
	IdentityProviderID string

	// Signature carries the HMAC verification settings — secret,
	// header names, skew, clock.
	Signature SignatureConfig

	// Metrics, when non-nil, drives the JSM noop / parse-error
	// counters. nil-safe so unit tests don't have to wire metrics.
	Metrics *observability.Metrics
}

// Provider implements webhook.Provider for Jira Service Management.
// Construct with New; the zero value is unusable. Goroutine-safe: the
// dispatcher concurrently calls VerifySignature and Handle from many
// request goroutines, so we hold no mutable state.
type Provider struct {
	cfg Config
}

// Compile-time interface assertion. Cheap belt-and-suspenders against
// drift in `webhook.Provider`.
var _ webhook.Provider = (*Provider)(nil)

// New builds a Provider from the supplied Config. cfg is taken by
// pointer because the SignatureConfig dragging in clock + secret +
// header names tips it past the gocritic hugeParam threshold; callers
// at wiring time pass a `&Config{...}` literal.
func New(cfg *Config) *Provider {
	c := *cfg
	if c.Signature.Now == nil {
		c.Signature.Now = time.Now
	}
	return &Provider{cfg: c}
}

// Name returns the URL path segment that routes to this provider.
// Stable; the value is also used as a metrics label so renaming would
// break dashboards.
func (*Provider) Name() string { return "jsm" }

// VerifySignature delegates to the package-level helper so the same
// path is exercised by signature_test.go. The Provider just supplies
// its configured headers and secret.
func (p *Provider) VerifySignature(r *http.Request, body []byte) error {
	return VerifySignature(r, body, p.cfg.Signature)
}

// Handle decodes the verified body and decides what work to do.
// Return contract:
//
//   - `NoopAction` when the ticket isn't in the configured trigger
//     status. The dispatcher responds 200 with `status: "noop"`.
//   - `ApplySAMLGroupMapping` when the ticket *is* in the trigger
//     status and all three custom fields are present and non-empty.
//   - non-nil error wrapping `webhook.ErrBadRequest` for parse errors
//     (malformed JSON, missing required fields).
//   - non-nil error wrapping `webhook.ErrUnprocessable` for typed
//     extract failures (custom field present but the wrong type).
//
// Distinguishing 400-vs-422 here matters because retrying the same
// payload won't help in either case but the cause is meaningfully
// different — JSON corruption vs tenant misconfiguration of the
// automation rule.
func (p *Provider) Handle(_ context.Context, body []byte) (webhook.Action, error) {
	payload, err := Decode(body)
	if err != nil {
		p.recordParseErr(decodeReason(err))
		return nil, fmt.Errorf("%w: %w", webhook.ErrBadRequest, err)
	}

	if payload.Status() != p.cfg.TriggerStatus {
		p.recordNoop(payload.Status())
		return webhook.NoopAction{
			Reason: fmt.Sprintf("ticket %s status %q does not match trigger %q",
				payload.IssueKey(), payload.Status(), p.cfg.TriggerStatus),
		}, nil
	}

	providerGroupID, err := ExtractString(payload, p.cfg.FieldProviderGroupID)
	if err != nil {
		p.recordParseErr(extractReason(err))
		return nil, classifyExtractErr(err)
	}
	role, err := ExtractString(payload, p.cfg.FieldRole)
	if err != nil {
		p.recordParseErr(extractReason(err))
		return nil, classifyExtractErr(err)
	}
	project, err := ExtractString(payload, p.cfg.FieldProject)
	if err != nil {
		p.recordParseErr(extractReason(err))
		return nil, classifyExtractErr(err)
	}

	spec := BuildSpec(
		providerGroupID, role, project,
		p.cfg.IdentityProviderID,
		BuildDescription(payload.IssueKey()),
	)
	return webhook.ApplySAMLGroupMapping{
		IssueKey: payload.IssueKey(),
		Spec:     spec,
	}, nil
}

// recordNoop is nil-safe; the test path constructs Provider without
// Metrics and we want zero ceremony at the call site.
func (p *Provider) recordNoop(status string) {
	if p.cfg.Metrics == nil {
		return
	}
	p.cfg.Metrics.JSMNoopTotal.WithLabelValues(status).Inc()
}

// recordParseErr counts payload-rejected events by reason. Same
// nil-safety pattern as recordNoop.
func (p *Provider) recordParseErr(reason string) {
	if p.cfg.Metrics == nil {
		return
	}
	p.cfg.Metrics.JSMPayloadParseErrors.WithLabelValues(reason).Inc()
}

// Prometheus label values for JSMPayloadParseErrors. Stable; renaming
// would break dashboards.
const (
	parseReasonInvalidJSON = "invalid_json"
	parseReasonMissing     = "missing_field"
	parseReasonEmpty       = "empty_field"
	parseReasonType        = "wrong_type"
)

// decodeReason maps a Decode error onto its Prometheus label value.
// Defaults to invalid_json for any unmapped error so dashboards never
// see an empty label.
func decodeReason(err error) string {
	switch {
	case errors.Is(err, ErrMissingIssue), errors.Is(err, ErrMissingIssueKey),
		errors.Is(err, ErrMissingStatus):
		return parseReasonMissing
	default:
		return parseReasonInvalidJSON
	}
}

// extractReason maps an ExtractString error onto its Prometheus label
// value.
func extractReason(err error) string {
	switch {
	case errors.Is(err, ErrFieldMissing):
		return parseReasonMissing
	case errors.Is(err, ErrFieldEmpty):
		return parseReasonEmpty
	case errors.Is(err, ErrFieldType):
		return parseReasonType
	default:
		return parseReasonMissing
	}
}

// classifyExtractErr maps the `extract.go` sentinels onto webhook's
// dispatcher-facing sentinels. Missing/empty custom fields → 400
// (the JSM tenant didn't fill in a required field, retry won't help
// without a human edit). Wrong type → 422 (the field exists but the
// JSM custom-field configuration is wrong, also unrecoverable but
// caused by tenant misconfig rather than user error).
func classifyExtractErr(err error) error {
	switch {
	case errors.Is(err, ErrFieldMissing), errors.Is(err, ErrFieldEmpty):
		return fmt.Errorf("%w: %w", webhook.ErrBadRequest, err)
	case errors.Is(err, ErrFieldType):
		return fmt.Errorf("%w: %w", webhook.ErrUnprocessable, err)
	default:
		return err
	}
}
