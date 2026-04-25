// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package httpx

import (
	"context"
	"net"
	"net/http"

	"github.com/donaldgifford/webhookd/internal/config"
)

// NewServer returns an *http.Server with all timeouts wired from
// cfg. Every timeout matters: ReadHeaderTimeout caps the time for slow
// pre-body attacks, ReadTimeout caps the full request, WriteTimeout
// caps the response, IdleTimeout caps keepalive connections. Defaults
// in config.Load are tuned for a sub-second webhook handler; operators
// raise them via env vars when integrating with slow upstreams.
//
// BaseContext returns a single shared context so the run loop can
// cancel every in-flight request as part of graceful shutdown — Phase 5
// passes a child of the rootCtx here so SIGTERM cascades to handlers.
func NewServer(
	baseCtx context.Context,
	addr string,
	h http.Handler,
	cfg *config.Config,
) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		BaseContext: func(_ net.Listener) context.Context {
			return baseCtx
		},
	}
}
