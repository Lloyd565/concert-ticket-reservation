// Command payment is the Payment service: charges, refunds, and the provider
// integration (ARCHITECTURE.md §3.1).
//
// It starts and stops independently of every other service. Nothing here dials
// Booking, Auth or the gateway - Payment is called, it does not call back, and
// that one-directionality is what keeps the single synchronous inter-service
// hop in the system from becoming a cycle.
//
// This file does wiring, configuration and graceful shutdown only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	paymentv1 "github.com/lloyd565/concert-ticket-reservation/proto/payment/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/config"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/events"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/logging"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/provider"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/repository/postgres"
	paymentgrpc "github.com/lloyd565/concert-ticket-reservation/services/payment/internal/transport/grpc"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/usecase"
)

func main() {
	log := slog.New(logging.Handler{Handler: slog.NewJSONHandler(os.Stdout, nil)})

	if err := run(log); err != nil {
		log.Error("payment service stopped", "error", err)
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

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	repo := postgres.NewRepo(pool)
	prov := provider.NewMock(cfg.ProviderMode, cfg.ProviderLatency)
	charger := usecase.NewCharger(repo, prov, cfg.ProviderTimeout, cfg.ProviderMaxAttempts, log)
	refunder := usecase.NewRefunder(repo, repo, prov, cfg.ProviderTimeout, log)

	// Settling a charge or a refund writes its event to the outbox in the same
	// statement (D9); the relay drains it. Lazily connected, so Payment keeps
	// answering Booking with RabbitMQ down, and its events wait (§3.3).
	relay := events.NewRelay(cfg.AMQPURL, repo, log)
	go relay.Run(ctx)

	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(logging.UnaryServerInterceptor()))
	paymentv1.RegisterPaymentServiceServer(grpcSrv, paymentgrpc.NewServer(charger, refunder, log))

	lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		return err
	}

	// Liveness and readiness (NFR-4.4). Readiness includes the database check:
	// without it this service cannot record a charge before calling the
	// provider, and a charge it cannot record is a charge it must not make.
	httpSrv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           healthMux(func() error { return pool.Ping(ctx) }),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 2)
	go func() {
		log.Info("payment grpc listening",
			"addr", lis.Addr().String(),
			"provider_mode", string(cfg.ProviderMode),
			"provider_timeout", cfg.ProviderTimeout.String(),
			"provider_max_attempts", cfg.ProviderMaxAttempts)
		if err := grpcSrv.Serve(lis); err != nil {
			errc <- err
		}
	}()
	go func() {
		log.Info("payment health listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Drain in-flight calls before exiting. A charge that has settled but not
	// yet responded must still reach its caller - if it does not, Booking is
	// left with an unknown outcome it has to reconcile for no reason.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	drained := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(drained)
	}()
	select {
	case <-drained:
	case <-shutdownCtx.Done():
		// Refused to drain in time; drop the remaining connections rather than
		// hang the shutdown forever.
		grpcSrv.Stop()
	}
	return httpSrv.Shutdown(shutdownCtx)
}

func healthMux(ready func() error) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if err := ready(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
