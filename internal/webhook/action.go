// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

import (
	"errors"

	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// Action is a typed union of work the executor knows how to perform.
// The unexported sentinel method prevents external packages from
// adding variants the executor doesn't know about; new variants must
// land in this package alongside the matching switch arm in the
// executor.
//
// Phase 2 ships two variants: NoopAction (no-op acknowledgement) and
// ApplySAMLGroupMapping (the JSM provisioning path). Phase 3+ would
// add e.g. PostSlackMessage, CreateGitHubIssue.
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

// ApplySAMLGroupMapping is the only side-effectful Action in Phase 2:
// the executor builds the typed CR from Spec, applies it via SSA, and
// watches its status until Ready=True or the configured timeout.
//
// The cardinality is 1 ticket = 1 CR with one project + one role; the
// Spec.ProjectRefs slice always carries a single element. The list
// shape is preserved to match the operator CRD.
type ApplySAMLGroupMapping struct {
	// IssueKey is the JSM ticket key (e.g., "SEC-1234"). The executor
	// derives the CR name from it (`jsm-<key-lower>`) and stamps it
	// onto the `webhookd.io/jsm-issue-key` annotation for traceability.
	IssueKey string

	// Spec is the desired CR spec. Constructed by the JSM provider's
	// pure cr.BuildSpec helper; the executor copies it into the
	// SAMLGroupMapping's TypeMeta + ObjectMeta envelope.
	Spec wizapi.SAMLGroupMappingSpec
}

func (ApplySAMLGroupMapping) isAction() {}

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
