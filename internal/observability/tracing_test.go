package observability

import (
	"context"
	"strings"
	"testing"

	"github.com/donaldgifford/webhookd/internal/config"
)

// TestSamplerFor_Boundaries confirms each ratio bucket lands on the right
// sampler. We assert on Description() rather than reflecting the type so
// the test stays close to operator-visible behavior (the description
// shows up in OTel debug output).
func TestSamplerFor_Boundaries(t *testing.T) {
	tests := []struct {
		name     string
		ratio    float64
		wantDesc string
	}{
		{"one", 1.0, "ParentBased{root:AlwaysOnSampler"},
		{"above one", 1.5, "ParentBased{root:AlwaysOnSampler"},
		{"zero", 0.0, "ParentBased{root:AlwaysOffSampler"},
		{"below zero", -0.1, "ParentBased{root:AlwaysOffSampler"},
		{"midpoint", 0.5, "ParentBased{root:TraceIDRatioBased{0.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := samplerFor(tt.ratio)
			got := s.Description()
			if !strings.HasPrefix(got, tt.wantDesc) {
				t.Errorf("samplerFor(%v).Description() = %q, want prefix %q",
					tt.ratio, got, tt.wantDesc)
			}
		})
	}
}

// TestNewTracerProvider_Disabled returns a usable provider when tracing is
// off. We do not assert on internal sampler state because the tracer
// provider does not expose it; we only require the constructor to succeed
// and Shutdown to be a clean no-op so callers can defer it unconditionally.
func TestNewTracerProvider_Disabled(t *testing.T) {
	cfg := &config.Config{
		TracingEnabled:     false,
		TracingSampleRatio: 1.0,
		ServiceName:        "webhookd-test",
	}
	tp, err := NewTracerProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewTracerProvider() err = %v", err)
	}
	if tp == nil {
		t.Fatal("NewTracerProvider() returned nil provider")
	}
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() err = %v", err)
	}
}

// TestNewTracerProvider_Enabled exercises the OTLP-exporter path. We
// point OTEL_EXPORTER_OTLP_ENDPOINT at an unreachable host; the exporter
// is constructed lazily and dial errors only surface on export, so the
// constructor itself must succeed.
func TestNewTracerProvider_Enabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	cfg := &config.Config{
		TracingEnabled:     true,
		TracingSampleRatio: 0.5,
		ServiceName:        "webhookd-test",
		ServiceVersion:     "v0.0.0",
	}
	tp, err := NewTracerProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewTracerProvider() err = %v", err)
	}
	if tp == nil {
		t.Fatal("NewTracerProvider() returned nil provider")
	}
	// Use a separate context for shutdown so a hanging exporter cannot
	// deadlock the test indefinitely.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = tp.Shutdown(ctx)
}
