// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/donaldgifford/webhookd/internal/webhook"
	"github.com/donaldgifford/webhookd/internal/webhook/jsm"
)

func TestBuild_StatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		kind       webhook.ResultKind
		wantStatus string
	}{
		{"noop", webhook.ResultNoop, "noop"},
		{"ready", webhook.ResultReady, "success"},
		{"timeout", webhook.ResultTimeout, "failure"},
		{"transient", webhook.ResultTransientFailure, "failure"},
		{"bad_request", webhook.ResultBadRequest, "failure"},
		{"unprocessable", webhook.ResultUnprocessable, "failure"},
		{"internal_error", webhook.ResultInternalError, "failure"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := jsm.Build(webhook.ExecResult{Kind: tc.kind}, "", "")
			if got.Status != tc.wantStatus {
				t.Errorf("Build({Kind: %v}).Status = %q, want %q", tc.kind, got.Status, tc.wantStatus)
			}
		})
	}
}

func TestBuild_PassesThroughIdentityFields(t *testing.T) {
	t.Parallel()
	res := webhook.ExecResult{
		Kind:               webhook.ResultReady,
		CRName:             "jsm-sec-100",
		Namespace:          "wiz-operator",
		ObservedGeneration: 3,
	}
	got := jsm.Build(res, "trace-abc", "req-xyz")
	if got.CRName != "jsm-sec-100" {
		t.Errorf("CRName = %q", got.CRName)
	}
	if got.Namespace != "wiz-operator" {
		t.Errorf("Namespace = %q", got.Namespace)
	}
	if got.ObservedGeneration != 3 {
		t.Errorf("ObservedGeneration = %d", got.ObservedGeneration)
	}
	if got.TraceID != "trace-abc" {
		t.Errorf("TraceID = %q", got.TraceID)
	}
	if got.RequestID != "req-xyz" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
}

func TestBuild_OmitEmptyHonored(t *testing.T) {
	t.Parallel()
	body := jsm.Build(webhook.ExecResult{Kind: webhook.ResultReady}, "", "")
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Empty optional fields should not appear in the JSON.
	for _, field := range []string{"reason", "crName", "namespace", "observedGeneration", "traceId", "requestId"} {
		if strings.Contains(string(b), field) {
			t.Errorf("encoded body contains %q despite empty value: %s", field, b)
		}
	}
}
