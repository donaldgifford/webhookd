// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/donaldgifford/webhookd/internal/httpx"
	"github.com/donaldgifford/webhookd/internal/observability"
)

// ResponseBuilder is the seam between provider-agnostic execution
// (the dispatcher's call to Execute) and provider-specific response
// shaping. JSM responses include `crName`, `traceId`, `requestId`
// fields that JSM's automation rule reads for ticket comments; future
// providers (Slack, GitHub) will want different bodies. Each provider
// ships its own implementation in its own package.
type ResponseBuilder interface {
	BuildResponse(res ExecResult, traceID, requestID string) any
}

// Dispatcher is the HTTP entry point that ties providers, signature
// verification, and the executor together. It is the only piece in
// the package that knows about `*http.Request` — providers stay pure
// and the executor stays K8s-only.
//
// Construct with `NewDispatcher`; the zero value is unusable. Goroutine
// safety is inherited from the underlying components (providers are
// goroutine-safe by contract; the executor is too).
type Dispatcher struct {
	providers       map[string]Provider
	responseBuilder ResponseBuilder
	executor        executorIface
	logger          *slog.Logger
	metrics         *observability.Metrics
	maxBodyBytes    int64
}

// executorIface is what the dispatcher needs from an Executor — pulled
// out as an interface so dispatcher tests can run without spinning up
// envtest. The production type *Executor implements it trivially.
type executorIface interface {
	Execute(ctx context.Context, a Action) ExecResult
}

// DispatcherConfig groups every dependency NewDispatcher needs. We
// take a struct instead of functional options because the field set
// is small and stable; functional options here would be ceremony.
type DispatcherConfig struct {
	// Providers is the registered set, keyed by Name. Routes to the
	// matching provider via the `{provider}` URL path value.
	Providers []Provider

	// ResponseBuilder shapes ExecResult into a provider-specific
	// response body. Phase 2 ships exactly one (JSM); when a second
	// provider lands, this becomes a per-provider lookup.
	ResponseBuilder ResponseBuilder

	// Executor performs the side-effectful Action returned by
	// Provider.Handle.
	Executor executorIface

	// Logger is used for unexpected events: oversized body, parse
	// errors, etc. nil-safe — falls back to the default slog logger.
	Logger *slog.Logger

	// Metrics, when non-nil, drives the response counter on every
	// completed request. nil is allowed for tests that don't care.
	Metrics *observability.Metrics

	// MaxBodyBytes is the request-body size limit. Above this the
	// dispatcher returns 413. Defaults to 1 MiB if zero.
	MaxBodyBytes int64
}

// NewDispatcher returns a configured Dispatcher. Provider names must
// be unique — duplicate registration is a programming error and
// panics at construction (we'd rather crash loudly at startup than
// silently route to whichever Provider was registered last).
//
// cfg is taken by pointer because the field bag dragging in the
// metrics struct + provider slice tips it past gocritic's hugeParam
// threshold; callers pass `&DispatcherConfig{...}`.
func NewDispatcher(cfg *DispatcherConfig) *Dispatcher {
	d := &Dispatcher{
		providers:       make(map[string]Provider, len(cfg.Providers)),
		responseBuilder: cfg.ResponseBuilder,
		executor:        cfg.Executor,
		logger:          cfg.Logger,
		metrics:         cfg.Metrics,
		maxBodyBytes:    cfg.MaxBodyBytes,
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	if d.maxBodyBytes <= 0 {
		d.maxBodyBytes = 1 << 20
	}
	for _, p := range cfg.Providers {
		name := p.Name()
		if _, dup := d.providers[name]; dup {
			panic(fmt.Sprintf("webhook.NewDispatcher: duplicate provider %q", name))
		}
		d.providers[name] = p
	}
	return d
}

// ServeHTTP implements http.Handler.
func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("provider")
	prov, ok := d.providers[name]
	if !ok {
		d.logger.WarnContext(ctx, "unknown provider", "provider", name)
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, d.maxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		d.logger.WarnContext(ctx, "read body failed", "err", err.Error(), "provider", name)
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	if err := prov.VerifySignature(r, body); err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	action, err := prov.Handle(ctx, body)
	if err != nil {
		res := classifyProviderErr(err)
		d.writeResponse(ctx, w, res)
		return
	}

	res := d.executor.Execute(ctx, action)
	d.writeResponse(ctx, w, res)
}

// writeResponse serializes the response body and writes the status
// code derived from result.Kind. The body is provider-specific via
// ResponseBuilder; the status code is provider-agnostic via
// HTTPStatus.
func (d *Dispatcher) writeResponse(ctx context.Context, w http.ResponseWriter, res ExecResult) {
	traceID := traceIDFromContext(ctx)
	reqID := httpx.RequestIDFromContext(ctx)

	status := res.Kind.HTTPStatus()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if d.metrics != nil {
		d.metrics.JSMResponseTotal.WithLabelValues(strconv.Itoa(status)).Inc()
	}

	body := d.responseBuilder.BuildResponse(res, traceID, reqID)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Header already written; nothing useful left to do but log.
		d.logger.WarnContext(ctx, "encode response failed", "err", err.Error())
	}
}

// classifyProviderErr maps a Provider.Handle error onto the matching
// ExecResult. We keep this in the dispatcher (not the provider)
// because the dispatcher is what owns the response contract; the
// provider just signals intent via the `webhook.Err*` sentinels.
func classifyProviderErr(err error) ExecResult {
	switch {
	case errors.Is(err, ErrBadRequest):
		return ExecResult{Kind: ResultBadRequest, Reason: err.Error()}
	case errors.Is(err, ErrUnprocessable):
		return ExecResult{Kind: ResultUnprocessable, Reason: err.Error()}
	default:
		return ExecResult{Kind: ResultInternalError, Reason: err.Error()}
	}
}
