package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/donaldgifford/webhookd/internal/config"
)

// NewTracerProvider returns a configured OTel TracerProvider for webhookd.
//
// When cfg.TracingEnabled is false, the provider is built with no exporter
// and a NeverSample sampler — spans started against it are valid but never
// recorded. This keeps the rest of the codebase free of nil checks: every
// call site can assume otel.GetTracerProvider() returns a usable value.
//
// When enabled, the provider exports via OTLP/HTTP, reading
// OTEL_EXPORTER_OTLP_* env vars natively (see ADR-0002). The resource
// attaches service.name and service.version, then layers OTEL_RESOURCE_ATTRIBUTES
// on top so operators can override or extend at deploy time.
//
// Callers must invoke Shutdown(ctx) before process exit to flush pending
// spans; Phase 5 wires this into the shutdown sequence.
func NewTracerProvider(
	ctx context.Context,
	cfg *config.Config,
) (*sdktrace.TracerProvider, error) {
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	if !cfg.TracingEnabled {
		return sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.NeverSample()),
		), nil
	}

	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp http exporter: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(samplerFor(cfg.TracingSampleRatio)),
		sdktrace.WithBatcher(exp),
	), nil
}

// buildResource composes the OTel resource for this process. WithFromEnv
// runs last so OTEL_RESOURCE_ATTRIBUTES wins ties against the values we
// set programmatically — operators always have the final say.
func buildResource(
	ctx context.Context,
	cfg *config.Config,
) (*resource.Resource, error) {
	attrs := []resource.Option{
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
		resource.WithFromEnv(),
	}
	res, err := resource.New(ctx, attrs...)
	if err != nil {
		return nil, fmt.Errorf("resource.New: %w", err)
	}
	return res, nil
}

// samplerFor maps a 0..1 ratio to a ParentBased sampler. Values outside
// the closed range collapse to the nearest extreme: ratios at or above
// 1.0 always sample, ratios at or below 0.0 never sample. This matches
// the bounds-checking already done by config.validate, but is defensive
// in case the helper is reused later with unvalidated input.
func samplerFor(ratio float64) sdktrace.Sampler {
	switch {
	case ratio >= 1.0:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case ratio <= 0.0:
		return sdktrace.ParentBased(sdktrace.NeverSample())
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}
