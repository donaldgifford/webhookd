package config_test

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/donaldgifford/webhookd/internal/config"
)

// TestMain clears every WEBHOOK_* and OTEL_* env var before any test
// runs so tests start from a deterministic baseline regardless of the
// developer's host environment.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(name, "WEBHOOK_") ||
			strings.HasPrefix(name, "OTEL_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}

func TestLoad_RequiresSigningSecret(t *testing.T) {
	cfg, err := config.Load()
	if err == nil {
		t.Fatalf("Load() = %v, nil; want error", cfg)
	}
	if !errors.Is(err, config.ErrSigningSecretRequired) {
		t.Fatalf("Load() err = %v, want ErrSigningSecretRequired", err)
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("WEBHOOK_SIGNING_SECRET", "test-secret")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"Addr", cfg.Addr, ":8080"},
		{"AdminAddr", cfg.AdminAddr, ":9090"},
		{"ReadTimeout", cfg.ReadTimeout, 5 * time.Second},
		{"ReadHeaderTimeout", cfg.ReadHeaderTimeout, 2 * time.Second},
		{"WriteTimeout", cfg.WriteTimeout, 10 * time.Second},
		{"IdleTimeout", cfg.IdleTimeout, 60 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 25 * time.Second},
		{"MaxBodyBytes", cfg.MaxBodyBytes, int64(1 << 20)},
		{"SignatureHeader", cfg.SignatureHeader, "X-Webhook-Signature"},
		{"TimestampHeader", cfg.TimestampHeader, "X-Webhook-Timestamp"},
		{"TimestampSkew", cfg.TimestampSkew, 5 * time.Minute},
		{"RateLimitRPS", cfg.RateLimitRPS, 100.0},
		{"RateLimitBurst", cfg.RateLimitBurst, 200},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
		{"LogFormat", cfg.LogFormat, "json"},
		{"TracingEnabled", cfg.TracingEnabled, true},
		{"TracingSampleRatio", cfg.TracingSampleRatio, 1.0},
		{"ServiceName", cfg.ServiceName, "webhookd"},
		{"ServiceVersion", cfg.ServiceVersion, ""},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Load().%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if string(cfg.SigningSecret) != "test-secret" {
		t.Errorf("Load().SigningSecret = %q, want %q",
			cfg.SigningSecret, "test-secret")
	}
}

func TestLoad_AllOverrides(t *testing.T) {
	overrides := map[string]string{
		"WEBHOOK_ADDR":                 ":18080",
		"WEBHOOK_ADMIN_ADDR":           ":19090",
		"WEBHOOK_READ_TIMEOUT":         "1s",
		"WEBHOOK_READ_HEADER_TIMEOUT":  "500ms",
		"WEBHOOK_WRITE_TIMEOUT":        "2s",
		"WEBHOOK_IDLE_TIMEOUT":         "30s",
		"WEBHOOK_SHUTDOWN_TIMEOUT":     "10s",
		"WEBHOOK_MAX_BODY_BYTES":       "2048",
		"WEBHOOK_SIGNING_SECRET":       "override-secret",
		"WEBHOOK_SIGNATURE_HEADER":     "X-Custom-Signature",
		"WEBHOOK_TIMESTAMP_HEADER":     "X-Custom-Timestamp",
		"WEBHOOK_TIMESTAMP_SKEW":       "30s",
		"WEBHOOK_RATE_LIMIT_RPS":       "50.5",
		"WEBHOOK_RATE_LIMIT_BURST":     "75",
		"WEBHOOK_LOG_LEVEL":            "debug",
		"WEBHOOK_LOG_FORMAT":           "text",
		"WEBHOOK_TRACING_ENABLED":      "false",
		"WEBHOOK_TRACING_SAMPLE_RATIO": "0.25",
		"OTEL_SERVICE_NAME":            "webhookd-test",
		"OTEL_SERVICE_VERSION":         "v1.2.3",
	}
	for k, v := range overrides {
		t.Setenv(k, v)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"Addr", cfg.Addr, ":18080"},
		{"AdminAddr", cfg.AdminAddr, ":19090"},
		{"ReadTimeout", cfg.ReadTimeout, time.Second},
		{"ReadHeaderTimeout", cfg.ReadHeaderTimeout, 500 * time.Millisecond},
		{"WriteTimeout", cfg.WriteTimeout, 2 * time.Second},
		{"IdleTimeout", cfg.IdleTimeout, 30 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 10 * time.Second},
		{"MaxBodyBytes", cfg.MaxBodyBytes, int64(2048)},
		{"SignatureHeader", cfg.SignatureHeader, "X-Custom-Signature"},
		{"TimestampHeader", cfg.TimestampHeader, "X-Custom-Timestamp"},
		{"TimestampSkew", cfg.TimestampSkew, 30 * time.Second},
		{"RateLimitRPS", cfg.RateLimitRPS, 50.5},
		{"RateLimitBurst", cfg.RateLimitBurst, 75},
		{"LogLevel", cfg.LogLevel, slog.LevelDebug},
		{"LogFormat", cfg.LogFormat, "text"},
		{"TracingEnabled", cfg.TracingEnabled, false},
		{"TracingSampleRatio", cfg.TracingSampleRatio, 0.25},
		{"ServiceName", cfg.ServiceName, "webhookd-test"},
		{"ServiceVersion", cfg.ServiceVersion, "v1.2.3"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Load().%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestLoad_ParseErrors covers each typed env helper's failure path.
// The helper short-circuits on the first error, so we set only the
// problem variable plus the required signing secret per case.
func TestLoad_ParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		envVar  string
		envVal  string
		wantSub string // substring expected in the error message
	}{
		{"duration", "WEBHOOK_READ_TIMEOUT", "not-a-duration", "WEBHOOK_READ_TIMEOUT"},
		{"int64", "WEBHOOK_MAX_BODY_BYTES", "abc", "WEBHOOK_MAX_BODY_BYTES"},
		{"float", "WEBHOOK_RATE_LIMIT_RPS", "xyz", "WEBHOOK_RATE_LIMIT_RPS"},
		{"bool", "WEBHOOK_TRACING_ENABLED", "maybe", "WEBHOOK_TRACING_ENABLED"},
		{"level", "WEBHOOK_LOG_LEVEL", "silly", "WEBHOOK_LOG_LEVEL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WEBHOOK_SIGNING_SECRET", "test-secret")
			t.Setenv(tt.envVar, tt.envVal)

			cfg, err := config.Load()
			if err == nil {
				t.Fatalf("Load() = %v, nil; want error", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Load() err = %q, want substring %q",
					err.Error(), tt.wantSub)
			}
		})
	}
}

// TestLoad_RangeValidation covers post-parse value checks.
func TestLoad_RangeValidation(t *testing.T) {
	tests := []struct {
		name    string
		envVar  string
		envVal  string
		wantSub string
	}{
		{
			"sample ratio negative",
			"WEBHOOK_TRACING_SAMPLE_RATIO", "-0.1",
			"WEBHOOK_TRACING_SAMPLE_RATIO",
		},
		{
			"sample ratio above 1",
			"WEBHOOK_TRACING_SAMPLE_RATIO", "1.1",
			"WEBHOOK_TRACING_SAMPLE_RATIO",
		},
		{
			"log format invalid",
			"WEBHOOK_LOG_FORMAT", "xml",
			"WEBHOOK_LOG_FORMAT",
		},
		{
			"max body zero",
			"WEBHOOK_MAX_BODY_BYTES", "0",
			"WEBHOOK_MAX_BODY_BYTES",
		},
		{
			"max body negative",
			"WEBHOOK_MAX_BODY_BYTES", "-1",
			"WEBHOOK_MAX_BODY_BYTES",
		},
		{
			"rate limit rps zero",
			"WEBHOOK_RATE_LIMIT_RPS", "0",
			"WEBHOOK_RATE_LIMIT_RPS",
		},
		{
			"rate limit burst zero",
			"WEBHOOK_RATE_LIMIT_BURST", "0",
			"WEBHOOK_RATE_LIMIT_BURST",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WEBHOOK_SIGNING_SECRET", "test-secret")
			t.Setenv(tt.envVar, tt.envVal)

			cfg, err := config.Load()
			if err == nil {
				t.Fatalf("Load() = %v, nil; want error", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Load() err = %q, want substring %q",
					err.Error(), tt.wantSub)
			}
		})
	}
}

// TestLoad_SampleRatioBoundaries verifies that 0.0 and 1.0 are accepted.
func TestLoad_SampleRatioBoundaries(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want float64
	}{
		{"zero", "0.0", 0.0},
		{"one", "1.0", 1.0},
		{"midpoint", "0.5", 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WEBHOOK_SIGNING_SECRET", "test-secret")
			t.Setenv("WEBHOOK_TRACING_SAMPLE_RATIO", tt.val)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() err = %v", err)
			}
			if cfg.TracingSampleRatio != tt.want {
				t.Errorf("Load().TracingSampleRatio = %v, want %v",
					cfg.TracingSampleRatio, tt.want)
			}
		})
	}
}

// TestLoad_LogLevels verifies case-insensitive parsing of every accepted
// level name.
func TestLoad_LogLevels(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Setenv("WEBHOOK_SIGNING_SECRET", "test-secret")
			t.Setenv("WEBHOOK_LOG_LEVEL", tt.input)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() err = %v", err)
			}
			if cfg.LogLevel != tt.want {
				t.Errorf("Load().LogLevel = %v, want %v",
					cfg.LogLevel, tt.want)
			}
		})
	}
}
