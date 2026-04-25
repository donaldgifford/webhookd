package httpx_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/httpx"
)

// TestNewServer_PropagatesTimeouts confirms the constructor copies every
// timeout from config — this matters because missing a single timeout
// (e.g., WriteTimeout) lets a stuck client hold a goroutine open
// forever, which silently bleeds capacity until the pod OOMs.
func TestNewServer_PropagatesTimeouts(t *testing.T) {
	cfg := &config.Config{
		ReadTimeout:       3 * time.Second,
		ReadHeaderTimeout: 1 * time.Second,
		WriteTimeout:      4 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	srv := httpx.NewServer(context.Background(), ":0", http.NotFoundHandler(), cfg)

	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadTimeout", srv.ReadTimeout, cfg.ReadTimeout},
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, cfg.ReadHeaderTimeout},
		{"WriteTimeout", srv.WriteTimeout, cfg.WriteTimeout},
		{"IdleTimeout", srv.IdleTimeout, cfg.IdleTimeout},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestNewServer_BaseContext confirms request contexts inherit from the
// supplied context — the shutdown sequence relies on this to cancel
// in-flight requests when SIGTERM arrives.
func TestNewServer_BaseContext(t *testing.T) {
	type ctxKey struct{}
	base := context.WithValue(context.Background(), ctxKey{}, "marker")

	srv := httpx.NewServer(base, ":0", http.NotFoundHandler(), &config.Config{})

	got := srv.BaseContext(nil)
	if got.Value(ctxKey{}) != "marker" {
		t.Errorf("BaseContext lost value: %v", got.Value(ctxKey{}))
	}
}
