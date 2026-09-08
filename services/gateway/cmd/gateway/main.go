// Command gateway is the API gateway: the single entry point for clients
// (ARCHITECTURE.md §3.1). It owns routing, per-IP rate limiting, correlation
// IDs and local access-token validation, and it owns no data.
//
// It starts independently of everything it routes to. The gRPC connections
// below are lazy, so the gateway comes up whether Auth and Booking are already
// running, still starting, or down - in any order.
//
// This file does wiring, configuration and graceful shutdown only.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	authv1 "github.com/lloyd565/concert-ticket-reservation/proto/auth/v1"
	bookingv1 "github.com/lloyd565/concert-ticket-reservation/proto/booking/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/config"
	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/middleware"
	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/token"
	gatewayhttp "github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/transport/http"
	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/upstream"
)

func main() {
	// Every line a request produces carries its correlation ID
	// (ARCHITECTURE.md §7).
	log := slog.New(middleware.LogHandler{Handler: slog.NewJSONHandler(os.Stdout, nil)})

	if err := run(log); err != nil {
		log.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	authConn, err := upstream.Dial(cfg.AuthAddr, cfg.UpstreamTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = authConn.Close() }()

	bookingConn, err := upstream.Dial(cfg.BookingAddr, cfg.UpstreamTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = bookingConn.Close() }()

	limiter := middleware.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst, cfg.RateLimitIdle)
	// Evicts idle buckets; without it the visitor map grows without bound.
	go limiter.Run(ctx)

	authn := middleware.NewAuthenticator(token.NewVerifier(cfg.JWTSecret, cfg.JWTIssuer, cfg.JWTAudience), log)
	handler := gatewayhttp.NewHandler(
		authv1.NewAuthServiceClient(authConn),
		bookingv1.NewBookingServiceClient(bookingConn),
		authn, limiter, log,
	)

	srv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "addr", srv.Addr, "auth", cfg.AuthAddr, "booking", cfg.BookingAddr,
			"rate_limit_rps", cfg.RateLimitRPS, "rate_limit_burst", cfg.RateLimitBurst)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Drain in-flight requests before exiting. A hold that has committed
	// downstream but not yet responded must still reach its client.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
