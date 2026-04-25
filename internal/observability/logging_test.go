package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/donaldgifford/webhookd/internal/observability"
)

func TestNewLogger_AttachesTraceContext(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo, "json")

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	wantTrace := span.SpanContext().TraceID().String()
	wantSpan := span.SpanContext().SpanID().String()

	logger.InfoContext(ctx, "hello", "k", "v")
	span.End()

	got := decodeLog(t, buf.Bytes())
	if got["trace_id"] != wantTrace {
		t.Errorf("trace_id = %q, want %q", got["trace_id"], wantTrace)
	}
	if got["span_id"] != wantSpan {
		t.Errorf("span_id = %q, want %q", got["span_id"], wantSpan)
	}
	if got["msg"] != "hello" || got["k"] != "v" {
		t.Errorf("log payload missing expected fields: %v", got)
	}
}

func TestNewLogger_NoSpanNoTraceFields(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo, "json")

	logger.InfoContext(context.Background(), "no span")

	got := decodeLog(t, buf.Bytes())
	if _, ok := got["trace_id"]; ok {
		t.Errorf("trace_id present without span: %v", got)
	}
	if _, ok := got["span_id"]; ok {
		t.Errorf("span_id present without span: %v", got)
	}
}

func TestNewLogger_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo, "text")

	logger.Info("plain text")

	out := buf.String()
	if !strings.Contains(out, "msg=\"plain text\"") {
		t.Errorf("text output missing msg: %q", out)
	}
}

// TestNewLogger_WithPreservesWrapper confirms that derived loggers
// (logger.With, logger.WithGroup) keep the trace-correlation wrapper.
// Without WithAttrs/WithGroup re-implementations on traceHandler, the
// derived handler would strip the wrapper.
func TestNewLogger_WithPreservesWrapper(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo, "json")

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	wantTrace := span.SpanContext().TraceID().String()

	derived := logger.With("service", "webhookd")
	derived.InfoContext(ctx, "via With")
	span.End()

	got := decodeLog(t, buf.Bytes())
	if got["trace_id"] != wantTrace {
		t.Errorf("derived logger lost trace_id: got %v, want %q",
			got["trace_id"], wantTrace)
	}
	if got["service"] != "webhookd" {
		t.Errorf("derived logger lost With attr: %v", got)
	}
}

// TestNewLogger_LevelGate verifies the configured level filters lower
// records.
func TestNewLogger_LevelGate(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelWarn, "json")

	logger.Info("dropped")
	logger.Warn("kept")

	out := buf.String()
	if strings.Contains(out, "dropped") {
		t.Errorf("info-level record leaked at warn gate: %q", out)
	}
	if !strings.Contains(out, "kept") {
		t.Errorf("warn-level record missing: %q", out)
	}
}

// decodeLog unmarshals one JSON log line; it fatals the test on
// malformed input.
func decodeLog(t *testing.T, b []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(bytes.TrimSpace(b), &out); err != nil {
		t.Fatalf("decode log line: %v\nbody: %s", err, b)
	}
	return out
}
