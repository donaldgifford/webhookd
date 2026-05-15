// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

// Package providertest provides a configurable fake implementation of
// webhook.Provider for dispatcher and integration tests that don't
// want to depend on the real JSM payload shape.
//
// The mock is intentionally minimal: caller-controlled VerifyFunc and
// HandleFunc, sane defaults that pass authentication and return a
// NoopAction. Anything more elaborate belongs in the real provider
// tests, not here.
package providertest

import (
	"context"
	"net/http"

	"github.com/donaldgifford/webhookd/internal/webhook"
)

// Mock is a webhook.Provider whose VerifySignature, Handle, and
// BuildResponse are driven by caller-supplied closures. Zero-valued
// Mock is usable: VerifySignature returns nil (authenticated), Handle
// returns NoopAction{Reason: "mock"}, BuildResponse returns a small
// map keyed by ResultKind for test assertions.
type Mock struct {
	// NameValue overrides Name; defaults to "mock" when empty.
	NameValue string

	// VerifyFunc, when non-nil, replaces the default
	// VerifySignature behavior (which returns nil).
	VerifyFunc func(r *http.Request, body []byte) error

	// HandleFunc, when non-nil, replaces the default Handle behavior
	// (which returns NoopAction{Reason: "mock"}, nil).
	HandleFunc func(ctx context.Context, body []byte) (webhook.Action, error)

	// ResponseFunc, when non-nil, replaces the default BuildResponse
	// behavior (which returns a flat map for test assertions).
	ResponseFunc func(res webhook.ExecResult, traceID, requestID string) any
}

// Compile-time check that *Mock satisfies the Provider contract.
var _ webhook.Provider = (*Mock)(nil)

// Name returns NameValue, or "mock" if NameValue is empty.
func (m *Mock) Name() string {
	if m.NameValue == "" {
		return "mock"
	}
	return m.NameValue
}

// VerifySignature delegates to VerifyFunc when set; otherwise treats
// every request as authentic. Tests that need 401 paths set
// VerifyFunc.
func (m *Mock) VerifySignature(r *http.Request, body []byte) error {
	if m.VerifyFunc == nil {
		return nil
	}
	return m.VerifyFunc(r, body)
}

// Handle delegates to HandleFunc when set; otherwise returns a
// NoopAction.
func (m *Mock) Handle(ctx context.Context, body []byte) (webhook.Action, error) {
	if m.HandleFunc == nil {
		return webhook.NoopAction{Reason: "mock"}, nil
	}
	return m.HandleFunc(ctx, body)
}

// BuildResponse delegates to ResponseFunc when set; otherwise returns
// a flat map carrying every ExecResult field plus the trace/request
// IDs. The default suits dispatcher tests that just want to inspect
// what reached the response writer.
func (m *Mock) BuildResponse(res webhook.ExecResult, traceID, requestID string) any {
	if m.ResponseFunc != nil {
		return m.ResponseFunc(res, traceID, requestID)
	}
	return map[string]any{
		"kind":      res.Kind.String(),
		"reason":    res.Reason,
		"crName":    res.CRName,
		"namespace": res.Namespace,
		"traceId":   traceID,
		"requestId": requestID,
	}
}
