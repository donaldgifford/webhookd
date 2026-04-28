// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/donaldgifford/webhookd/internal/webhook"
	"github.com/donaldgifford/webhookd/internal/webhook/providertest"
)

// stubResponseBuilder is the smallest possible ResponseBuilder for
// dispatcher tests — returns a map keyed by the result-kind string so
// callers can assert on the encoded JSON.
type stubResponseBuilder struct{}

func (stubResponseBuilder) BuildResponse(res webhook.ExecResult, traceID, requestID string) any {
	return map[string]any{
		"kind":      res.Kind.String(),
		"reason":    res.Reason,
		"crName":    res.CRName,
		"namespace": res.Namespace,
		"traceId":   traceID,
		"requestId": requestID,
	}
}

// stubExecutor records the last action it received and returns the
// configured ExecResult. Lets dispatcher tests verify the action got
// through without spinning up envtest.
type stubExecutor struct {
	got    webhook.Action
	result webhook.ExecResult
}

func (s *stubExecutor) Execute(_ context.Context, a webhook.Action) webhook.ExecResult {
	s.got = a
	return s.result
}

func newDispatcher(t *testing.T, prov webhook.Provider, exec *stubExecutor) http.Handler {
	t.Helper()
	d := webhook.NewDispatcher(&webhook.DispatcherConfig{
		Providers:       []webhook.Provider{prov},
		ResponseBuilder: stubResponseBuilder{},
		Executor:        exec,
		MaxBodyBytes:    512,
	})
	mux := http.NewServeMux()
	mux.Handle("POST /webhook/{provider}", d)
	return mux
}

func TestDispatcher_UnknownProvider_404(t *testing.T) {
	t.Parallel()
	mux := newDispatcher(t, &providertest.Mock{NameValue: "jsm"}, &stubExecutor{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/webhook/unknown", strings.NewReader(`{}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestDispatcher_BodyTooLarge_413(t *testing.T) {
	t.Parallel()
	mock := &providertest.Mock{NameValue: "jsm"}
	mux := newDispatcher(t, mock, &stubExecutor{})

	huge := strings.Repeat("x", 600)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/webhook/jsm", strings.NewReader(huge))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rr.Code)
	}
}

func TestDispatcher_BadSignature_401(t *testing.T) {
	t.Parallel()
	mock := &providertest.Mock{
		NameValue:  "jsm",
		VerifyFunc: func(*http.Request, []byte) error { return errors.New("bad sig") },
	}
	mux := newDispatcher(t, mock, &stubExecutor{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/webhook/jsm", strings.NewReader(`{"any":"thing"}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestDispatcher_NoopAction_200WithKindNoop(t *testing.T) {
	t.Parallel()
	mock := &providertest.Mock{
		NameValue: "jsm",
		HandleFunc: func(context.Context, []byte) (webhook.Action, error) {
			return webhook.NoopAction{Reason: "wrong status"}, nil
		},
	}
	exec := &stubExecutor{}
	mux := newDispatcher(t, mock, exec)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/webhook/jsm", strings.NewReader(`{"any":"thing"}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	body := decodeJSON(t, rr.Body)
	if body["kind"] != "noop" {
		t.Errorf("kind = %v, want noop", body["kind"])
	}
}

func TestDispatcher_ProviderError_Classification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantKind   string
	}{
		{"bad_request", fmt.Errorf("%w: malformed", webhook.ErrBadRequest), http.StatusBadRequest, "bad_request"},
		{"unprocessable", fmt.Errorf("%w: wrong field type", webhook.ErrUnprocessable), http.StatusUnprocessableEntity, "unprocessable"},
		{"internal", errors.New("unexpected"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := &providertest.Mock{
				NameValue: "jsm",
				HandleFunc: func(context.Context, []byte) (webhook.Action, error) {
					return nil, tc.err
				},
			}
			mux := newDispatcher(t, mock, &stubExecutor{})
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"/webhook/jsm", strings.NewReader(`{"any":"thing"}`))
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			body := decodeJSON(t, rr.Body)
			if body["kind"] != tc.wantKind {
				t.Errorf("kind = %v, want %s", body["kind"], tc.wantKind)
			}
		})
	}
}

func TestDispatcher_ExecutorResultMaps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		kind       webhook.ResultKind
		wantStatus int
	}{
		{"ready", webhook.ResultReady, http.StatusOK},
		{"timeout", webhook.ResultTimeout, http.StatusGatewayTimeout},
		{"transient", webhook.ResultTransientFailure, http.StatusServiceUnavailable},
		{"unprocessable", webhook.ResultUnprocessable, http.StatusUnprocessableEntity},
		{"forbidden", webhook.ResultInternalError, http.StatusInternalServerError},
		{"bad_request", webhook.ResultBadRequest, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := &providertest.Mock{
				NameValue: "jsm",
				HandleFunc: func(context.Context, []byte) (webhook.Action, error) {
					return webhook.NoopAction{}, nil
				},
			}
			exec := &stubExecutor{result: webhook.ExecResult{
				Kind:      tc.kind,
				CRName:    "jsm-sec-1",
				Namespace: "wiz-operator",
			}}
			mux := newDispatcher(t, mock, exec)
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"/webhook/jsm", strings.NewReader(`{"any":"thing"}`))
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
		})
	}
}

func TestDispatcher_DuplicateProviderPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewDispatcher with duplicate names did not panic")
		}
	}()
	_ = webhook.NewDispatcher(&webhook.DispatcherConfig{
		Providers: []webhook.Provider{
			&providertest.Mock{NameValue: "jsm"},
			&providertest.Mock{NameValue: "jsm"},
		},
		ResponseBuilder: stubResponseBuilder{},
		Executor:        &stubExecutor{},
	})
}

func decodeJSON(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}
