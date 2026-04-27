// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package config_test

import (
	"errors"
	"log/slog"
	"os"
	"slices"
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

// withBaselineEnv sets the minimum env required for Load to succeed
// without enabling any provider. Tests that do not exercise JSM
// behavior call this so they don't need to think about JSM-required
// fields.
func withBaselineEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WEBHOOK_SIGNING_SECRET", "test-secret")
	t.Setenv("WEBHOOK_PROVIDERS", "")
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
	withBaselineEnv(t)

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
		{"PProfEnabled", cfg.PProfEnabled, true},
		{"ServiceName", cfg.ServiceName, "webhookd"},
		{"ServiceVersion", cfg.ServiceVersion, ""},
		{"CR.Namespace", cfg.CR.Namespace, "wiz-operator"},
		{"CR.APIGroup", cfg.CR.APIGroup, "wiz.webhookd.io"},
		{"CR.APIVersion", cfg.CR.APIVersion, "v1alpha1"},
		{"CR.FieldManager", cfg.CR.FieldManager, "webhookd"},
		{"CR.SyncTimeout", cfg.CR.SyncTimeout, 20 * time.Second},
		{"JSM.TriggerStatus", cfg.JSM.TriggerStatus, "Ready to Provision"},
		{"Kubeconfig", cfg.Kubeconfig, ""},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Load().%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(cfg.EnabledProviders) != 0 {
		t.Errorf("Load().EnabledProviders = %v, want empty (set via baseline env)",
			cfg.EnabledProviders)
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
		"WEBHOOK_PPROF_ENABLED":        "false",
		"OTEL_SERVICE_NAME":            "webhookd-test",
		"OTEL_SERVICE_VERSION":         "v1.2.3",

		// Provider + JSM + CR overrides — required when JSM is enabled.
		"WEBHOOK_PROVIDERS":                   "jsm,slack",
		"WEBHOOK_JSM_TRIGGER_STATUS":          "Approved",
		"WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID": "customfield_10100",
		"WEBHOOK_JSM_FIELD_ROLE":              "customfield_10101",
		"WEBHOOK_JSM_FIELD_PROJECT":           "customfield_10102",
		"WEBHOOK_CR_NAMESPACE":                "custom-ns",
		"WEBHOOK_CR_API_GROUP":                "wiz.example.test",
		"WEBHOOK_CR_API_VERSION":              "v1beta1",
		"WEBHOOK_CR_FIELD_MANAGER":            "custom-manager",
		"WEBHOOK_CR_SYNC_TIMEOUT":             "5s",
		"WEBHOOK_CR_IDENTITY_PROVIDER_ID":     "saml-idp-prod",
		"WEBHOOK_KUBECONFIG":                  "/tmp/kubeconfig",
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
		{"PProfEnabled", cfg.PProfEnabled, false},
		{"ServiceName", cfg.ServiceName, "webhookd-test"},
		{"ServiceVersion", cfg.ServiceVersion, "v1.2.3"},
		{"JSM.TriggerStatus", cfg.JSM.TriggerStatus, "Approved"},
		{"JSM.FieldProviderGroupID", cfg.JSM.FieldProviderGroupID, "customfield_10100"},
		{"JSM.FieldRole", cfg.JSM.FieldRole, "customfield_10101"},
		{"JSM.FieldProject", cfg.JSM.FieldProject, "customfield_10102"},
		{"CR.Namespace", cfg.CR.Namespace, "custom-ns"},
		{"CR.APIGroup", cfg.CR.APIGroup, "wiz.example.test"},
		{"CR.APIVersion", cfg.CR.APIVersion, "v1beta1"},
		{"CR.FieldManager", cfg.CR.FieldManager, "custom-manager"},
		{"CR.SyncTimeout", cfg.CR.SyncTimeout, 5 * time.Second},
		{"CR.IdentityProviderID", cfg.CR.IdentityProviderID, "saml-idp-prod"},
		{"Kubeconfig", cfg.Kubeconfig, "/tmp/kubeconfig"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Load().%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	wantProviders := []string{"jsm", "slack"}
	if !slices.Equal(cfg.EnabledProviders, wantProviders) {
		t.Errorf("Load().EnabledProviders = %v, want %v",
			cfg.EnabledProviders, wantProviders)
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
			withBaselineEnv(t)
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
			withBaselineEnv(t)
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
			withBaselineEnv(t)
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

// jsmEnv returns the minimum env required for Load to succeed with
// JSM enabled. Tests that exercise JSM behavior call this and then
// override individual fields per case.
func jsmEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WEBHOOK_SIGNING_SECRET", "test-secret")
	t.Setenv("WEBHOOK_PROVIDERS", "jsm")
	t.Setenv("WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID", "customfield_10100")
	t.Setenv("WEBHOOK_JSM_FIELD_ROLE", "customfield_10101")
	t.Setenv("WEBHOOK_JSM_FIELD_PROJECT", "customfield_10102")
	t.Setenv("WEBHOOK_CR_IDENTITY_PROVIDER_ID", "saml-idp-test")
}

// TestLoad_JSMRequiredFields covers the "JSM enabled, required field
// missing" failure mode — one error per missing variable, all mapping
// to ErrJSMFieldsRequired.
func TestLoad_JSMRequiredFields(t *testing.T) {
	tests := []string{
		"WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID",
		"WEBHOOK_JSM_FIELD_ROLE",
		"WEBHOOK_JSM_FIELD_PROJECT",
	}
	for _, missing := range tests {
		t.Run(missing, func(t *testing.T) {
			jsmEnv(t)
			t.Setenv(missing, "")

			_, err := config.Load()
			if !errors.Is(err, config.ErrJSMFieldsRequired) {
				t.Fatalf("Load() err = %v, want ErrJSMFieldsRequired", err)
			}
		})
	}
}

// TestLoad_IdentityProviderIDRequired verifies the
// WEBHOOK_CR_IDENTITY_PROVIDER_ID guard fires when JSM is enabled
// but the IdP isn't set.
func TestLoad_IdentityProviderIDRequired(t *testing.T) {
	jsmEnv(t)
	t.Setenv("WEBHOOK_CR_IDENTITY_PROVIDER_ID", "")

	_, err := config.Load()
	if !errors.Is(err, config.ErrIdentityProviderIDRequired) {
		t.Fatalf("Load() err = %v, want ErrIdentityProviderIDRequired", err)
	}
}

// TestLoad_SyncTimeoutTooLong asserts SyncTimeout >= ShutdownTimeout
// fails fast — letting the watch outlast the drain budget would
// either truncate the JSM response or violate the JSM tenant timeout.
func TestLoad_SyncTimeoutTooLong(t *testing.T) {
	tests := []struct {
		name     string
		sync     string
		shutdown string
	}{
		{"equal", "10s", "10s"},
		{"sync greater", "30s", "20s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jsmEnv(t)
			t.Setenv("WEBHOOK_CR_SYNC_TIMEOUT", tt.sync)
			t.Setenv("WEBHOOK_SHUTDOWN_TIMEOUT", tt.shutdown)

			_, err := config.Load()
			if !errors.Is(err, config.ErrSyncTimeoutTooLong) {
				t.Fatalf("Load() err = %v, want ErrSyncTimeoutTooLong", err)
			}
		})
	}
}

// TestLoad_ProvidersCSV covers the comma-separated parsing helper:
// trim, lowercase, dedupe, drop empty entries. Uses unknown provider
// names so validation doesn't trigger required-field checks.
func TestLoad_ProvidersCSV(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want []string
	}{
		{"single", "slack", []string{"slack"}},
		{"two", "slack,github", []string{"slack", "github"}},
		{"whitespace + case", "  Slack ,  GitHub  ", []string{"slack", "github"}},
		{"dupes drop", "slack,slack,github", []string{"slack", "github"}},
		{"empty entries drop", ",,slack,,", []string{"slack"}},
		{"all empty", ",,,", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WEBHOOK_SIGNING_SECRET", "test-secret")
			t.Setenv("WEBHOOK_PROVIDERS", tt.val)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() err = %v", err)
			}
			if len(tt.want) == 0 && len(cfg.EnabledProviders) != 0 {
				t.Errorf("Load().EnabledProviders = %v, want empty",
					cfg.EnabledProviders)
			}
			if len(tt.want) > 0 && !slices.Equal(cfg.EnabledProviders, tt.want) {
				t.Errorf("Load().EnabledProviders = %v, want %v",
					cfg.EnabledProviders, tt.want)
			}
		})
	}
}

// TestProviderEnabled is a tiny smoke test on the helper.
func TestProviderEnabled(t *testing.T) {
	cfg := &config.Config{EnabledProviders: []string{"jsm", "slack"}}
	if !cfg.ProviderEnabled("jsm") {
		t.Error(`ProviderEnabled("jsm") = false, want true`)
	}
	if cfg.ProviderEnabled("github") {
		t.Error(`ProviderEnabled("github") = true, want false`)
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
			withBaselineEnv(t)
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
