// Package observability initializes webhookd's logging, tracing, and
// metrics subsystems. The three signals are configured independently
// (see ADR-0002) but share a single config struct and are wired
// together in cmd/webhookd/main.go.
package observability

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// NewLogger returns a *slog.Logger that writes to w in either JSON or
// text format. Logs emitted with a context that carries an active OTel
// span gain `trace_id` and `span_id` attributes automatically; calls
// without a span emit normally.
//
// format must be "json" or "text"; any other value is treated as
// "json" because Load already validates the field.
func NewLogger(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}

	var base slog.Handler
	if format == "text" {
		base = slog.NewTextHandler(w, opts)
	} else {
		base = slog.NewJSONHandler(w, opts)
	}
	return slog.New(&traceHandler{Handler: base})
}

// traceHandler wraps any slog.Handler so each emitted record carries
// `trace_id` and `span_id` attributes when the call's context has an
// active OTel span. The wrapper is the entire correlation mechanism;
// downstream code only needs to pass the request context.
type traceHandler struct {
	slog.Handler
}

// Handle takes slog.Record by value because the slog.Handler interface
// requires it; gocritic's hugeParam warning is a false positive here.
//
//nolint:gocritic // interface signature is fixed by log/slog.
func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		sc := span.SpanContext()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must be re-implemented to preserve the
// trace-correlation wrapping when callers derive sub-loggers via
// logger.With(...) or logger.WithGroup(...). Without these, a derived
// handler would lose the wrapper and stop adding trace_id/span_id.
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}
