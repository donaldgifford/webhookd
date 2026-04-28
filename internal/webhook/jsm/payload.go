// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

// Package jsm decodes Jira Service Management webhook payloads and
// turns them into typed `webhook.Action`s. The package is purposefully
// pure: no Kubernetes calls, no outbound HTTP. The Provider returned
// by `New` plugs into the generic dispatcher in `internal/webhook`.
//
// Three sources together define the JSM-specific contract:
//
//   - The custom-field IDs (provider group / role / project) come from
//     `config.JSMConfig`. They're tenant-specific and so always
//     injected, never hard-coded.
//   - The trigger status is also configured. Anything else returns a
//     NoopAction so JSM advances the ticket without retrying.
//   - The HMAC signing scheme reuses webhookd's project-wide
//     `webhook.Verify` (`v0:<ts>:<body>` over HMAC-SHA256). JSM is
//     configured via its automation rule to sign payloads with this
//     scheme.
package jsm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinel errors used by Decode and the provider. All wrap into the
// dispatcher's bad-request sentinel (`webhook.ErrBadRequest`) — they
// flag payloads that cannot be the work of a well-behaved JSM tenant.
var (
	// ErrInvalidJSON wraps json.Unmarshal failures.
	ErrInvalidJSON = errors.New("invalid json")

	// ErrMissingIssue is returned when the top-level "issue" key is
	// absent or null. JSM's webhook always carries an issue object;
	// missing it means the payload is not from JSM (or is a malformed
	// test request).
	ErrMissingIssue = errors.New("missing issue object")

	// ErrMissingIssueKey is returned when issue.key is absent or empty.
	// We rely on the issue key for the CR name, so an empty key is
	// fatal.
	ErrMissingIssueKey = errors.New("missing issue key")

	// ErrMissingStatus is returned when issue.fields.status.name is
	// absent or empty. The trigger comparison can't run otherwise.
	ErrMissingStatus = errors.New("missing issue status")
)

// Payload is the subset of a JSM webhook body that webhookd cares
// about. Custom fields are kept as `json.RawMessage` so the extract
// helpers can produce typed errors on missing / wrong-type fields
// without re-marshaling.
//
// The shape is intentionally minimal. JSM payloads carry many other
// fields (changelog, user, project, etc.) — we don't unmarshal them
// because every additional field is one more thing the test fixture
// has to mock and one more thing that can break on JSM-side changes.
type Payload struct {
	Issue Issue `json:"issue"`
}

// Issue holds the parts of issue we need: key (for the CR name) and
// fields (status + custom fields).
type Issue struct {
	Key    string      `json:"key"`
	Fields IssueFields `json:"fields"`
}

// IssueFields carries the typed status and the bag of custom fields
// keyed by the tenant-specific custom-field IDs configured at startup.
//
// Status is unmarshaled into a typed struct so we can require
// status.name; CustomFields stays as RawMessage so the extract layer
// can produce richly-typed errors without re-decoding.
type IssueFields struct {
	Status Status `json:"status"`

	// CustomFields holds every other field present on the issue,
	// keyed by JSM custom-field id (e.g., "customfield_10201"). The
	// extract layer pulls strings out by id at runtime.
	CustomFields map[string]json.RawMessage `json:"-"`
}

// Status is the JSM ticket status block.
type Status struct {
	Name string `json:"name"`
}

// UnmarshalJSON for IssueFields preserves the typed Status field while
// stuffing the rest of the fields object into CustomFields. The
// alternative (declaring every custom field explicitly) doesn't scale
// to a tenant-configurable field id set.
func (f *IssueFields) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("%w: fields: %w", ErrInvalidJSON, err)
	}
	if rawStatus, ok := raw["status"]; ok {
		if err := json.Unmarshal(rawStatus, &f.Status); err != nil {
			return fmt.Errorf("%w: status: %w", ErrInvalidJSON, err)
		}
		delete(raw, "status")
	}
	f.CustomFields = raw
	return nil
}

// Decode parses body into a Payload, validating the minimum invariants
// the rest of the package depends on (issue object, issue.key,
// issue.fields.status.name). All sentinel errors wrap %w-style so
// callers can errors.Is them.
func Decode(body []byte) (*Payload, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: empty body", ErrInvalidJSON)
	}

	// Pre-check that the top-level "issue" key is present at all —
	// json.Unmarshal would otherwise leave Issue zero-valued and our
	// later Key/Status emptiness checks would fire a confusingly less
	// specific error.
	probe := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	if _, ok := probe["issue"]; !ok {
		return nil, ErrMissingIssue
	}
	if string(probe["issue"]) == "null" {
		return nil, ErrMissingIssue
	}

	p := &Payload{}
	if err := json.Unmarshal(body, p); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	if p.Issue.Key == "" {
		return nil, ErrMissingIssueKey
	}
	if p.Issue.Fields.Status.Name == "" {
		return nil, ErrMissingStatus
	}
	return p, nil
}

// Status returns the ticket's status name. Convenience accessor so
// callers don't have to traverse Issue.Fields.Status.Name.
func (p *Payload) Status() string {
	return p.Issue.Fields.Status.Name
}

// IssueKey returns the JSM ticket key (e.g., "SEC-1234").
func (p *Payload) IssueKey() string {
	return p.Issue.Key
}
