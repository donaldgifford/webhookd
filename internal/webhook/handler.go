package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/donaldgifford/webhookd/internal/observability"
)

const tracerName = "github.com/donaldgifford/webhookd/internal/webhook"

// HandlerConfig carries every value the webhook handler needs from
// runtime config. Threading the full *config.Config would be tighter
// coupling than this layer needs — the handler only reads these fields,
// so the receiver keeps a narrower contract that's easier to fake in
// tests.
type HandlerConfig struct {
	SigningSecret   []byte
	MaxBodyBytes    int64
	SignatureHeader string
	TimestampHeader string
	TimestampSkew   time.Duration

	// Now lets tests inject a deterministic clock; nil falls back to
	// time.Now in production wiring.
	Now func() time.Time
}

// Envelope is the minimum shape the Phase 1 handler decodes. Provider
// dispatchers in Phase 2 will read further into Data; for now we only
// validate JSON and surface EventType for logs and metrics.
type Envelope struct {
	EventType string          `json:"event_type"`
	Data      json.RawMessage `json:"data"`
}

// NewHandler returns the receiver for `POST /webhook/{provider}`. It
// runs the signature/timestamp verification, parses the envelope,
// records the canonical metrics, and emits the domain-event log line
// the operator dashboards depend on.
//
// cfg is taken by value at the constructor boundary so call sites can
// build literals; the closure captures a single copy and never mutates.
//
//nolint:gocritic // by-value construction is intentional at this seam.
func NewHandler(
	cfg HandlerConfig,
	logger *slog.Logger,
	m *observability.Metrics,
) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	tracer := otel.Tracer(tracerName)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		provider := r.PathValue("provider")
		started := cfg.Now()

		body, ok := readBody(w, r, cfg.MaxBodyBytes, provider, m)
		if !ok {
			return
		}

		if !verifyDelivery(ctx, tracer, &cfg, r, body, provider, w, logger, m) {
			return
		}

		eventType, ok := parseEnvelope(ctx, tracer, body, provider, w, logger, m)
		if !ok {
			return
		}

		logger.InfoContext(ctx, "webhook accepted",
			"provider", provider,
			"event_type", eventType,
			"bytes", len(body),
		)
		w.WriteHeader(http.StatusAccepted)

		m.WebhookEvents.WithLabelValues(provider, eventType, "accepted").Inc()
		m.WebhookProcessing.WithLabelValues(provider, eventType).
			Observe(cfg.Now().Sub(started).Seconds())
	})
}

// readBody enforces the body-size cap and writes the proper status on
// any read failure. The boolean return lets the caller bail without
// double-writing the response.
func readBody(
	w http.ResponseWriter,
	r *http.Request,
	maxBytes int64,
	provider string,
	m *observability.Metrics,
) ([]byte, bool) {
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	body, err := io.ReadAll(limited)
	if err == nil {
		return body, true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		m.WebhookEvents.WithLabelValues(provider, "", "too_large").Inc()
		return nil, false
	}
	w.WriteHeader(http.StatusBadRequest)
	m.WebhookEvents.WithLabelValues(provider, "", "read_error").Inc()
	return nil, false
}

// verifyDelivery wraps the signature/timestamp checks in a span so
// trace consumers see the verify cost separately from JSON parse cost.
// Returns false when verification has already written the response.
func verifyDelivery(
	ctx context.Context,
	tracer trace.Tracer,
	cfg *HandlerConfig,
	r *http.Request,
	body []byte,
	provider string,
	w http.ResponseWriter,
	logger *slog.Logger,
	m *observability.Metrics,
) bool {
	ctx, span := tracer.Start(ctx, "webhook.verify_signature",
		trace.WithAttributes(attribute.String("provider", provider)),
	)
	defer span.End()

	sig := r.Header.Get(cfg.SignatureHeader)
	ts := r.Header.Get(cfg.TimestampHeader)
	err := Verify(cfg.SigningSecret, sig, ts, body, cfg.Now(), cfg.TimestampSkew)
	if err == nil {
		m.WebhookSigResults.WithLabelValues(provider, "valid").Inc()
		return true
	}
	span.RecordError(err)

	result, outcome := classifyVerifyError(err)
	m.WebhookSigResults.WithLabelValues(provider, result).Inc()
	m.WebhookEvents.WithLabelValues(provider, "", outcome).Inc()
	logger.WarnContext(ctx, "webhook signature rejected",
		"provider", provider,
		"reason", err.Error(),
	)
	w.WriteHeader(http.StatusUnauthorized)
	return false
}

// classifyVerifyError maps Verify's typed errors to the metric labels
// DESIGN-0001 §Metrics specifies. Centralizing the mapping keeps the
// label vocabulary stable across handler refactors.
func classifyVerifyError(err error) (result, outcome string) {
	switch {
	case errors.Is(err, ErrTimestampMissing):
		return "missing", "missing_signature"
	case errors.Is(err, ErrMalformed),
		errors.Is(err, ErrTimestampMalformed):
		return "missing", "missing_signature"
	case errors.Is(err, ErrTimestampSkewed):
		return "invalid", "timestamp_skewed"
	default:
		return "invalid", "invalid_signature"
	}
}

// parseEnvelope decodes the JSON envelope inside a span so the parse
// cost is visible to operators. Returns false when JSON is malformed
// (the response is already written).
func parseEnvelope(
	ctx context.Context,
	tracer trace.Tracer,
	body []byte,
	provider string,
	w http.ResponseWriter,
	logger *slog.Logger,
	m *observability.Metrics,
) (string, bool) {
	_, span := tracer.Start(ctx, "webhook.parse",
		trace.WithAttributes(attribute.String("provider", provider)),
	)
	defer span.End()

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		span.RecordError(err)
		m.WebhookEvents.WithLabelValues(provider, "", "malformed").Inc()
		logger.WarnContext(ctx, "webhook payload malformed",
			"provider", provider,
			"err", err.Error(),
		)
		w.WriteHeader(http.StatusBadRequest)
		return "", false
	}
	return env.EventType, true
}
