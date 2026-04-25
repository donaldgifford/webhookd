package httpx

import (
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/donaldgifford/webhookd/internal/observability"
)

// NewAdminMux returns the handler for the admin listener: liveness,
// readiness, and the metrics scrape. The admin mux runs on a separate
// port (DESIGN-0001 §HTTP Layout) so probe traffic never contends with
// webhook intake; we keep its middleware stack minimal — just Recover
// and Metrics — because OTel + per-request access logs would dominate
// log volume given how often Kubernetes scrapes these endpoints.
//
// ready is shared with main: it flips to true once startup wiring is
// done. Failed dependencies in later phases (e.g., a queue connection)
// will flip it back to false to drain traffic via /readyz.
func NewAdminMux(
	logger *slog.Logger,
	reg *prometheus.Registry,
	m *observability.Metrics,
	ready *atomic.Bool,
) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(w, "ok")
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeBody(w, "not ready")
			return
		}
		w.WriteHeader(http.StatusOK)
		writeBody(w, "ready")
	})

	mux.Handle("GET /metrics", promhttp.HandlerFor(
		reg, promhttp.HandlerOpts{Registry: reg},
	))

	return Chain(mux, Recover(logger, m), Metrics(m))
}

// writeBody writes a fixed string to w. The probes are short, internal,
// and called from within trusted clients (kubelet) — a write error here
// means the connection is already gone and the response is moot, so we
// drop the error rather than logging. The named returns satisfy linters
// that flag both check-blank and unhandled-error patterns.
func writeBody(w http.ResponseWriter, body string) {
	if _, err := w.Write([]byte(body)); err != nil {
		return
	}
}
