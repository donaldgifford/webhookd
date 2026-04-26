// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

// Package config loads webhookd's runtime configuration from environment
// variables.
//
// See DESIGN-0001 §Configuration for the canonical variable table and
// ADR-0003 for why webhookd uses environment variables exclusively (no
// CLI flags, no config files, no runtime reload). Load is called once at
// startup; the returned *Config is immutable for the process lifetime.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds parsed runtime configuration. All fields are populated by
// Load and are not modified afterwards.
type Config struct {
	// HTTP listeners.
	Addr              string
	AdminAddr         string
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	// Webhook intake.
	MaxBodyBytes    int64
	SigningSecret   []byte
	SignatureHeader string
	TimestampHeader string
	TimestampSkew   time.Duration

	// Rate limiting (per-replica, per-provider, in-process).
	RateLimitRPS   float64
	RateLimitBurst int

	// Logging.
	LogLevel  slog.Level
	LogFormat string

	// Tracing.
	TracingEnabled     bool
	TracingSampleRatio float64

	// Profiling — kill switch for net/http/pprof on the admin listener.
	// Default true; flip false in environments that forbid pprof
	// endpoints. Pyroscope uses a pull model against /debug/pprof, so
	// no client-side SDK needs configuration here.
	PProfEnabled bool

	// OTel resource attributes captured for our own resource assembly.
	// The OTel SDK reads OTEL_EXPORTER_OTLP_* directly; only fields we
	// also use ourselves appear here.
	ServiceName    string
	ServiceVersion string

	// BuildInfo carries build-time provenance. It is not populated from
	// the environment — main injects it from -ldflags variables.
	BuildInfo BuildInfo
}

// BuildInfo carries build-time provenance for the
// webhookd_build_info{version, commit, go_version} metric and trace
// resource attributes.
type BuildInfo struct {
	Version   string
	Commit    string
	GoVersion string
}

// ErrSigningSecretRequired is returned by Load when WEBHOOK_SIGNING_SECRET
// is unset or empty. Replicas without a signing secret cannot validate
// any incoming webhook, so we fail fast at startup rather than accepting
// every request.
var ErrSigningSecretRequired = errors.New(
	"WEBHOOK_SIGNING_SECRET is required",
)

// Load reads configuration from the environment, applies defaults, and
// validates each setting. It returns the first error encountered, which
// is sufficient because errors at startup are operator-fixable: surfacing
// only the first one keeps the message tight.
func Load() (*Config, error) {
	var l loader
	cfg := &Config{
		// HTTP listeners.
		Addr:              l.str("WEBHOOK_ADDR", ":8080"),
		AdminAddr:         l.str("WEBHOOK_ADMIN_ADDR", ":9090"),
		ReadTimeout:       l.dur("WEBHOOK_READ_TIMEOUT", 5*time.Second),
		ReadHeaderTimeout: l.dur("WEBHOOK_READ_HEADER_TIMEOUT", 2*time.Second),
		WriteTimeout:      l.dur("WEBHOOK_WRITE_TIMEOUT", 10*time.Second),
		IdleTimeout:       l.dur("WEBHOOK_IDLE_TIMEOUT", 60*time.Second),
		ShutdownTimeout:   l.dur("WEBHOOK_SHUTDOWN_TIMEOUT", 25*time.Second),

		// Webhook intake.
		MaxBodyBytes:    l.i64("WEBHOOK_MAX_BODY_BYTES", 1<<20),
		SignatureHeader: l.str("WEBHOOK_SIGNATURE_HEADER", "X-Webhook-Signature"),
		TimestampHeader: l.str("WEBHOOK_TIMESTAMP_HEADER", "X-Webhook-Timestamp"),
		TimestampSkew:   l.dur("WEBHOOK_TIMESTAMP_SKEW", 5*time.Minute),

		// Rate limiting.
		RateLimitRPS:   l.f64("WEBHOOK_RATE_LIMIT_RPS", 100),
		RateLimitBurst: int(l.i64("WEBHOOK_RATE_LIMIT_BURST", 200)),

		// Logging.
		LogFormat: l.str("WEBHOOK_LOG_FORMAT", "json"),
		LogLevel:  l.level("WEBHOOK_LOG_LEVEL", slog.LevelInfo),

		// Tracing.
		TracingEnabled:     l.boolean("WEBHOOK_TRACING_ENABLED", true),
		TracingSampleRatio: l.f64("WEBHOOK_TRACING_SAMPLE_RATIO", 1.0),

		// Profiling.
		PProfEnabled: l.boolean("WEBHOOK_PPROF_ENABLED", true),

		// OTel.
		ServiceName:    l.str("OTEL_SERVICE_NAME", "webhookd"),
		ServiceVersion: l.str("OTEL_SERVICE_VERSION", ""),
	}
	if l.err != nil {
		return nil, l.err
	}

	secret := os.Getenv("WEBHOOK_SIGNING_SECRET")
	if secret == "" {
		return nil, ErrSigningSecretRequired
	}
	cfg.SigningSecret = []byte(secret)

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate checks cross-field and range constraints that are not
// expressible at parse time.
func validate(cfg *Config) error {
	if cfg.TracingSampleRatio < 0 || cfg.TracingSampleRatio > 1 {
		return fmt.Errorf(
			"WEBHOOK_TRACING_SAMPLE_RATIO out of range [0,1]: %v",
			cfg.TracingSampleRatio,
		)
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		return fmt.Errorf(
			"WEBHOOK_LOG_FORMAT must be json or text: %q",
			cfg.LogFormat,
		)
	}
	if cfg.MaxBodyBytes <= 0 {
		return fmt.Errorf(
			"WEBHOOK_MAX_BODY_BYTES must be positive: %d",
			cfg.MaxBodyBytes,
		)
	}
	if cfg.RateLimitRPS <= 0 {
		return fmt.Errorf(
			"WEBHOOK_RATE_LIMIT_RPS must be positive: %v",
			cfg.RateLimitRPS,
		)
	}
	if cfg.RateLimitBurst <= 0 {
		return fmt.Errorf(
			"WEBHOOK_RATE_LIMIT_BURST must be positive: %d",
			cfg.RateLimitBurst,
		)
	}
	return nil
}

// loader is a small helper that records the first parse error it
// encounters, so Load can call a sequence of typed lookups without an
// `if err != nil` after each one.
type loader struct {
	err error
}

func (l *loader) str(name, fallback string) string {
	if l.err != nil {
		return fallback
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	return v
}

func (l *loader) dur(name string, fallback time.Duration) time.Duration {
	if l.err != nil {
		return fallback
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.err = fmt.Errorf("%s: %w", name, err)
		return fallback
	}
	return d
}

func (l *loader) i64(name string, fallback int64) int64 {
	if l.err != nil {
		return fallback
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		l.err = fmt.Errorf("%s: %w", name, err)
		return fallback
	}
	return n
}

func (l *loader) f64(name string, fallback float64) float64 {
	if l.err != nil {
		return fallback
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.err = fmt.Errorf("%s: %w", name, err)
		return fallback
	}
	return f
}

func (l *loader) boolean(name string, fallback bool) bool {
	if l.err != nil {
		return fallback
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.err = fmt.Errorf("%s: %w", name, err)
		return fallback
	}
	return b
}

// level parses a slog level name. Accepts the four canonical names
// (debug, info, warn, error) case-insensitively.
func (l *loader) level(name string, fallback slog.Level) slog.Level {
	if l.err != nil {
		return fallback
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		l.err = fmt.Errorf("%s: unknown level %q", name, v)
		return fallback
	}
}
