// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

// Package main is the entry point for the webhookd webhook receiver
// service. It performs the five-phase startup sequence documented in
// Walk1.md §2: config → observability → handlers → servers → run loop.
// Every package below this binary is single-purpose; main is the only
// file allowed to know about more than one of them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/httpx"
	"github.com/donaldgifford/webhookd/internal/observability"
	"github.com/donaldgifford/webhookd/internal/webhook"
)

// Build-time provenance, injected via
// -ldflags "-X main.version=... -X main.commit=...".
// See the build target in the Makefile.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	os.Exit(realMain())
}

// realMain runs the program and returns an exit code so deferred
// cleanup (signal cancel, etc.) executes before os.Exit terminates the
// process. Splitting this out is the standard workaround for "os.Exit
// after defer".
func realMain() int {
	ctx, cancel := signal.NotifyContext(
		context.Background(), syscall.SIGTERM, syscall.SIGINT,
	)
	defer cancel()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "webhookd: %v\n", err)
		return 1
	}
	return 0
}

// run is split out from main so tests can drive the same wiring with a
// cancelable context. Returning an error (rather than calling os.Exit)
// keeps deferred cleanup running and the codebase testable.
func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.BuildInfo = config.BuildInfo{
		Version:   version,
		Commit:    commit,
		GoVersion: runtime.Version(),
	}

	logger := observability.NewLogger(os.Stdout, cfg.LogLevel, cfg.LogFormat)

	tp, err := observability.NewTracerProvider(ctx, cfg)
	if err != nil {
		return fmt.Errorf("tracer provider: %w", err)
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	//nolint:contextcheck // tracer flush must outlive signal-canceled parent ctx.
	defer shutdownTracerProvider(tp, cfg.ShutdownTimeout, logger)

	reg, metrics := observability.NewMetrics(cfg)

	publicHandler := buildPublicHandler(cfg, logger, metrics)

	var ready atomic.Bool
	adminHandler := httpx.NewAdminMux(logger, reg, metrics, &ready,
		httpx.AdminConfig{PProfEnabled: cfg.PProfEnabled})

	publicSrv := httpx.NewServer(ctx, cfg.Addr, publicHandler, cfg)
	adminSrv := httpx.NewServer(ctx, cfg.AdminAddr, adminHandler, cfg)

	errCh := make(chan error, 2)
	startServer(publicSrv, "public", errCh, logger)
	startServer(adminSrv, "admin", errCh, logger)

	ready.Store(true)
	logger.InfoContext(ctx, "listening",
		"public_addr", cfg.Addr,
		"admin_addr", cfg.AdminAddr,
		"version", version,
		"commit", commit,
	)

	return waitForShutdown(ctx, cfg, logger, &ready, errCh, publicSrv, adminSrv)
}

// drainServers asks both servers to shut down within the configured
// timeout, using a fresh background context so the drain budget
// survives any signal-cancellation of the run-loop ctx.
func drainServers(
	timeout time.Duration,
	publicSrv, adminSrv *http.Server,
) (error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return publicSrv.Shutdown(ctx), adminSrv.Shutdown(ctx)
}

// shutdownTracerProvider flushes the tracer provider with a fresh
// background context so the run-loop's signal-canceled ctx cannot
// short-circuit the exporter flush.
func shutdownTracerProvider(
	tp interface {
		Shutdown(context.Context) error
	},
	timeout time.Duration,
	logger *slog.Logger,
) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := tp.Shutdown(ctx); err != nil {
		logger.ErrorContext(ctx, "tracer provider shutdown",
			"err", err.Error())
	}
}

// buildPublicHandler composes the public mux and middleware chain. The
// middleware order matters; see internal/httpx for the rationale.
func buildPublicHandler(
	cfg *config.Config,
	logger *slog.Logger,
	metrics *observability.Metrics,
) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /webhook/{provider}", webhook.NewHandler(
		webhook.HandlerConfig{
			SigningSecret:   cfg.SigningSecret,
			MaxBodyBytes:    cfg.MaxBodyBytes,
			SignatureHeader: cfg.SignatureHeader,
			TimestampHeader: cfg.TimestampHeader,
			TimestampSkew:   cfg.TimestampSkew,
		},
		logger,
		metrics,
	))
	return httpx.Chain(
		mux,
		httpx.Recover(logger, metrics),
		httpx.OTel(cfg.ServiceName),
		httpx.RequestID(),
		httpx.RateLimit(httpx.RateLimitConfig{
			RPS:   cfg.RateLimitRPS,
			Burst: cfg.RateLimitBurst,
		}, metrics),
		httpx.SLog(logger),
		httpx.Metrics(metrics),
	)
}

// startServer dispatches a goroutine that runs srv.ListenAndServe and
// reports any non-Closed error on errCh. Naming makes the log line
// distinguishable when both servers race.
func startServer(
	srv *http.Server,
	name string,
	errCh chan<- error,
	logger *slog.Logger,
) {
	go func() {
		if err := srv.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			logger.Error("listener exited", "name", name, "err", err.Error())
			errCh <- fmt.Errorf("%s listener: %w", name, err)
			return
		}
		errCh <- nil
	}()
}

// waitForShutdown blocks until either ctx is canceled (signal) or one
// of the listeners returns. On exit it flips readiness to false (so
// /readyz starts returning 503), then asks both servers to drain
// within the configured timeout.
func waitForShutdown(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	ready *atomic.Bool,
	errCh <-chan error,
	publicSrv, adminSrv *http.Server,
) error {
	var listenErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case listenErr = <-errCh:
		if listenErr != nil {
			logger.Error("listener exited unexpectedly", "err", listenErr.Error())
		}
	}

	ready.Store(false)
	//nolint:contextcheck // shutdown context must outlive signal-canceled parent.
	pubErr, admErr := drainServers(cfg.ShutdownTimeout, publicSrv, adminSrv)
	switch {
	case pubErr != nil:
		return fmt.Errorf("public shutdown: %w", pubErr)
	case admErr != nil:
		return fmt.Errorf("admin shutdown: %w", admErr)
	case listenErr != nil:
		return listenErr
	}
	logger.Info("shutdown complete", "duration_budget", cfg.ShutdownTimeout)
	return nil
}
