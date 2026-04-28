// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook_test

import (
	"net/http"
	"testing"

	"github.com/donaldgifford/webhookd/internal/webhook"
)

// TestResultKind_HTTPStatus pins the kind→status mapping. The
// dispatcher and the JSM response builder both depend on this; a
// regression here would silently shift JSM's retry behavior.
func TestResultKind_HTTPStatus(t *testing.T) {
	tests := []struct {
		kind webhook.ResultKind
		want int
	}{
		{webhook.ResultNoop, http.StatusOK},
		{webhook.ResultReady, http.StatusOK},
		{webhook.ResultBadRequest, http.StatusBadRequest},
		{webhook.ResultUnprocessable, http.StatusUnprocessableEntity},
		{webhook.ResultTransientFailure, http.StatusServiceUnavailable},
		{webhook.ResultTimeout, http.StatusGatewayTimeout},
		{webhook.ResultInternalError, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.kind.String(), func(t *testing.T) {
			if got := tt.kind.HTTPStatus(); got != tt.want {
				t.Errorf("%s.HTTPStatus() = %d, want %d",
					tt.kind, got, tt.want)
			}
		})
	}
}

// TestResultKind_String pins the label-safe names. Prometheus labels
// build off these strings, so renaming an entry would re-bucket
// historical data.
func TestResultKind_String(t *testing.T) {
	tests := []struct {
		kind webhook.ResultKind
		want string
	}{
		{webhook.ResultNoop, "noop"},
		{webhook.ResultReady, "ready"},
		{webhook.ResultTransientFailure, "transient_failure"},
		{webhook.ResultBadRequest, "bad_request"},
		{webhook.ResultUnprocessable, "unprocessable"},
		{webhook.ResultInternalError, "internal_error"},
		{webhook.ResultTimeout, "timeout"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("%v.String() = %q, want %q", int(tt.kind), got, tt.want)
		}
	}
}

// Compile-time guard that both NoopAction and ApplySAMLGroupMapping
// satisfy the Action interface. Lives at package scope rather than in
// a test function so a regression fails compilation, not just `go test`.
var (
	_ webhook.Action = webhook.NoopAction{}
	_ webhook.Action = webhook.ApplySAMLGroupMapping{}
)
