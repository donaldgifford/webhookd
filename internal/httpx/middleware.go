// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

// Package httpx wires the HTTP layer for webhookd: middleware, the
// admin mux (probes + metrics scrape), and the server constructor that
// applies our timeout policy.
//
// Middleware order is load-bearing — see Walk1.md §5 and DESIGN-0001
// §HTTP Layer. The intended outermost-to-innermost stack on the public
// listener is: Recover → OTel → RequestID → SLog → Metrics → handler.
// Recover sits outermost so a panic in any inner layer is caught; OTel
// is next so the resulting span ID is available to RequestID's log
// echo; SLog and Metrics observe the request after RequestID has
// stamped it.
package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/donaldgifford/webhookd/internal/observability"
)

// requestIDHeader is the canonical header name. We accept it inbound
// (operators can correlate from upstream proxies) and always echo it
// outbound so curl-driven debugging can grep for the value.
const requestIDHeader = "X-Request-ID"

// ctxKey is unexported so external packages cannot accidentally collide
// with our context keys.
type ctxKey struct{ name string }

var requestIDKey = ctxKey{name: "request_id"}

// RequestIDFromContext returns the request ID stored by the RequestID
// middleware, or "" if the value is absent. Returning a zero string for
// the absent case keeps callers from having to type-assert; "" is a
// valid attribute value for slog and Prometheus alike.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// Chain composes middlewares in outermost-first order: the first arg
// runs first on the way in and last on the way out. This matches how
// developers naturally read the call site (`Chain(h, Recover, OTel,
// RequestID)` reads as "Recover wraps OTel wraps RequestID wraps h").
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Recover catches panics from inner handlers, logs them with a stack,
// increments the panic counter, and writes a 500. The logger is passed
// in (rather than read from package-level state) so tests can capture
// output and so the production caller controls level/format.
func Recover(logger *slog.Logger, m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				m.HTTPPanics.Inc()
				logger.ErrorContext(
					ctx,
					"http handler panic",
					"panic", rec,
					"stack", string(debug.Stack()),
					"method", r.Method,
					"route", routeOf(r),
				)
				w.WriteHeader(http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// OTel wraps the chain in otelhttp so traces attach to every request.
// We pass the empty operation string ("") so otelhttp uses the matched
// pattern; the alternative — naming each handler — would duplicate the
// route label we already stamp on metrics.
func OTel(serviceName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(
			next, "",
			otelhttp.WithServerName(serviceName),
		)
	}
}

// RequestID reads or generates a UUIDv7 request ID, stuffs it on the
// context, and echoes it on the response. UUIDv7 is time-ordered so
// log greps stay sorted; google/uuid is a temporary home until the
// stdlib uuid package lands in Go 1.27 (see ADR notes).
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestIDHeader)
			if id == "" {
				v7, err := uuid.NewV7()
				if err != nil {
					// uuid.NewV7 only fails if rand.Read fails — fall
					// back to V4 so the request still gets an ID.
					v7 = uuid.New()
				}
				id = v7.String()
			}
			w.Header().Set(requestIDHeader, id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// SLog emits one log line per request after the inner handler returns.
// The line carries the canonical fields plus auto-injected trace_id /
// span_id from the traceHandler installed in observability.NewLogger.
func SLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := newStatusRecorder(w)
			next.ServeHTTP(rec, r)
			logger.InfoContext(
				r.Context(),
				"http request",
				"method", r.Method,
				"route", routeOf(r),
				"status", rec.status,
				"bytes_in", r.ContentLength,
				"bytes_out", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFromContext(r.Context()),
			)
		})
	}
}

// Metrics records request counts, latency, and sizes. The route label
// uses r.Pattern (Go 1.22+ ServeMux); requests that miss every pattern
// get a stable "__unmatched__" sentinel so 404 floods cannot blow up
// the cardinality of the counter.
func Metrics(m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.HTTPInflight.Inc()
			defer m.HTTPInflight.Dec()

			start := time.Now()
			rec := newStatusRecorder(w)
			next.ServeHTTP(rec, r)

			method := r.Method
			route := routeOf(r)
			status := strconv.Itoa(rec.status)

			m.HTTPRequests.WithLabelValues(method, route, status).Inc()
			m.HTTPDuration.WithLabelValues(method, route, status).
				Observe(time.Since(start).Seconds())
			if r.ContentLength > 0 {
				m.HTTPRequestSize.WithLabelValues(method, route).
					Observe(float64(r.ContentLength))
			}
			m.HTTPResponseSize.WithLabelValues(method, route).
				Observe(float64(rec.bytes))
		})
	}
}

// routeOf returns the matched ServeMux pattern, or a stable sentinel
// when nothing matched. The sentinel keeps unmatched requests from
// inflating route-label cardinality.
func routeOf(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return "__unmatched__"
}

// statusRecorder wraps an http.ResponseWriter so middleware can read
// the status code and response byte count after the inner handler
// returns. It defaults status to 200 because handlers that write a
// body without calling WriteHeader implicitly send 200.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}
