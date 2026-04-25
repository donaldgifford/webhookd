// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/donaldgifford/webhookd/internal/observability"
)

// webhookPathPrefix anchors RateLimit to the `/webhook/{provider}/*`
// shape. Anything outside this prefix bypasses the limiter — admin
// probes and 404s must never get rate-limited or healthchecks could
// fail under load.
const webhookPathPrefix = "/webhook/"

// RateLimitConfig captures the values RateLimit needs from runtime
// config. Threading the full *config.Config would over-couple this
// boundary; the middleware only cares about RPS and burst.
type RateLimitConfig struct {
	RPS   float64
	Burst int
}

// RateLimit returns middleware that applies a per-provider, per-replica
// token-bucket limiter. The provider key is parsed from the URL path
// directly (`/webhook/{provider}/...`) because middleware runs before
// the mux — `r.PathValue` is empty pre-routing. Anything outside the
// `/webhook/` prefix bypasses the limiter so admin probes and 404s
// cannot be locked out under load.
//
// Limiters are created lazily and stored in a sync.Map so the middleware
// stays allocation-free on the hot path after warm-up. We do not garbage
// collect entries — providers are bounded by an allow-list in Phase 2,
// so the map stays small.
func RateLimit(cfg RateLimitConfig, m *observability.Metrics) func(http.Handler) http.Handler {
	limit := rate.Limit(cfg.RPS)
	burst := cfg.Burst
	var limiters sync.Map

	getLimiter := func(provider string) *rate.Limiter {
		if v, ok := limiters.Load(provider); ok {
			if lim, ok := v.(*rate.Limiter); ok {
				return lim
			}
		}
		newLim := rate.NewLimiter(limit, burst)
		actual, _ := limiters.LoadOrStore(provider, newLim)
		if lim, ok := actual.(*rate.Limiter); ok {
			return lim
		}
		return newLim
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provider := providerFromPath(r.URL.Path)
			if provider == "" {
				next.ServeHTTP(w, r)
				return
			}
			lim := getLimiter(provider)
			if lim.Allow() {
				next.ServeHTTP(w, r)
				return
			}
			m.HTTPRateLimited.WithLabelValues(provider).Inc()
			retry := retryAfter(lim)
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
}

// providerFromPath returns the segment immediately after `/webhook/`,
// or "" if the path is outside that prefix or has an empty provider
// segment. We match exactly one segment here — `/webhook/foo/bar`
// gives `foo`, not the whole tail — so future sub-route shapes don't
// blow up the limiter's label cardinality.
func providerFromPath(p string) string {
	rest, ok := strings.CutPrefix(p, webhookPathPrefix)
	if !ok || rest == "" {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// retryAfter computes a conservative seconds-until-token estimate from
// the limiter's reservation. We round up so clients always wait at
// least as long as the limiter would refuse them.
func retryAfter(lim *rate.Limiter) int {
	r := lim.Reserve()
	defer r.Cancel() // we don't actually want to spend a token here.
	d := r.Delay()
	if d <= 0 {
		return 1
	}
	secs := int(d / time.Second)
	if d%time.Second > 0 {
		secs++
	}
	if secs < 1 {
		return 1
	}
	return secs
}
