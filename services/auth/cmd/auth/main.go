// Command auth is the Auth service: users, credentials, roles and refresh
// tokens (ARCHITECTURE.md §3.1).
//
// It starts and stops independently of every other service. Nothing here dials
// Booking or the gateway, and nothing here blocks on them being up: the gateway
// validates access tokens locally, so Auth being down or restarting must not
// disturb sessions that were already issued (§3.3).
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

	authv1 "github.com/lloyd565/concert-ticket-reservation/proto/auth/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/config"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/logging"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/repository/postgres"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/token"
	authgrpc "github.com/lloyd565/concert-ticket-reservation/services/auth/internal/transport/grpc"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/usecase"
)

func main() {
	log := slog.New(logging.Handler{Handler: slog.NewJSONHandler(os.Stdout, nil)})

	if err := run(log); err != nil {
		log.Error("auth service stopped", "error", err)
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
	signer := token.NewSigner(cfg.JWTSecret, cfg.JWTIssuer, cfg.JWTAudience, cfg.AccessTTL)
	accounts := usecase.NewAccounts(repo, repo, signer, cfg.RefreshTTL)

	// Seeds the first admin so a fresh stack is demoable without a manual
	// database step (PRD §7 criterion 5). Backgrounded: see runBootstrap.
	if cfg.BootstrapEnabled() {
		go runBootstrap(ctx, accounts, cfg.BootstrapAdminEmail, cfg.BootstrapAdminPassword, log)
	} else {
		log.Info("admin bootstrap disabled; BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD are unset")
	}

	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(logging.UnaryServerInterceptor()))
	authv1.RegisterAuthServiceServer(grpcSrv, authgrpc.NewServer(accounts, log))

	lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		return err
	}

	// Liveness and readiness (NFR-4.4). Readiness includes the database check:
	// without it this service cannot authenticate anybody, so it is not ready.
	httpSrv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           healthMux(func() error { return pool.Ping(ctx) }),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 2)
	go func() {
		log.Info("auth grpc listening", "addr", lis.Addr().String(), "access_ttl", cfg.AccessTTL.String(), "refresh_ttl", cfg.RefreshTTL.String())
		if err := grpcSrv.Serve(lis); err != nil {
			errc <- err
		}
	}()
	go func() {
		log.Info("auth health listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Drain in-flight calls before exiting. A registration that has committed
	// but not yet responded must still reach its caller.
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
