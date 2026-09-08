// Command booking is the Booking service: the seat-state authority.
//
// P1 adds a gRPC door beside the existing REST one so the gateway can route to
// it (ARCHITECTURE.md §4). Payment, Redis and the broker still arrive in later
// phases. This service starts and stops independently of Auth and the gateway:
// nothing here dials either of them.
//
// This file does wiring, configuration and graceful shutdown only.
package main

import (
	"context"
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

	bookingv1 "github.com/lloyd565/concert-ticket-reservation/proto/booking/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/breaker"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/config"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/logging"
	paymentclient "github.com/lloyd565/concert-ticket-reservation/services/booking/internal/repository/payment"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/repository/postgres"
	bookinggrpc "github.com/lloyd565/concert-ticket-reservation/services/booking/internal/transport/grpc"
	bookinghttp "github.com/lloyd565/concert-ticket-reservation/services/booking/internal/transport/http"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/usecase"
)

func main() {
	// Every line the request produces carries the gateway's correlation ID
	// (ARCHITECTURE.md §7), without each call site having to remember it.
	log := slog.New(logging.Handler{Handler: slog.NewJSONHandler(os.Stdout, nil)})

	if err := run(log); err != nil {
		log.Error("booking service stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Shutdown is driven by a signal-scoped context: the sweeper and the HTTP
	// server both stop from the same cancellation.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	repo := postgres.NewRepo(pool)
	txm := postgres.NewTxManager(pool)

	// The one synchronous service-to-service call in the system. Lazy dial:
	// Booking serves seat maps and holds whether or not Payment is up
	// (ARCHITECTURE.md §3.3), and only the pay step needs it.
	paymentClient, paymentConn, err := paymentclient.Dial(
		cfg.PaymentAddr, cfg.PaymentTimeout, breaker.New(cfg.BreakerThreshold, cfg.BreakerCooldown))
	if err != nil {
		return err
	}
	defer func() { _ = paymentConn.Close() }()

	holder := usecase.NewHolder(txm, repo, repo, cfg.HoldTTL)
	seeder := usecase.NewSeeder(txm, repo)
	sweeper := usecase.NewSweeper(repo, cfg.SweepInterval, log)
	saga := usecase.NewSaga(txm, repo, repo, repo, paymentPort{paymentClient}, log)
	reconciler := usecase.NewReconciler(saga, repo, paymentPort{paymentClient},
		cfg.ReconcileInterval, cfg.ReconcileGrace, 0, log)

	// The P0 expiry mechanism. Redis TTLs replace it in P3; until then an
	// abandoned checkout is released here and nowhere else - except for
	// reservations whose payment outcome is unknown, which the sweeper is
	// forbidden to touch and the reconciler below owns instead (D8).
	go sweeper.Run(ctx)
	go reconciler.Run(ctx)

	handler := bookinghttp.NewHandler(holder, seeder, saga, log)
	// Readiness includes the dependency check (NFR-4.4): without the database
	// this service cannot claim a seat, so it is not ready.
	srv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           handler.Routes(func() error { return pool.Ping(ctx) }),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The internal door. Same holder, same usecase, same locking: the gateway
	// gets no path into seat state that REST does not already have.
	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(logging.UnaryServerInterceptor()))
	bookingv1.RegisterBookingServiceServer(grpcSrv, bookinggrpc.NewServer(holder, seeder, saga, log))
	lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		return err
	}

	errc := make(chan error, 2)
	go func() {
		log.Info("booking listening", "addr", srv.Addr, "hold_ttl", cfg.HoldTTL.String(), "sweep_interval", cfg.SweepInterval.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go func() {
		log.Info("booking grpc listening", "addr", lis.Addr().String())
		if err := grpcSrv.Serve(lis); err != nil {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Drain in-flight requests before exiting. A hold that has committed but
	// not yet responded must still reach its client.
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
	return srv.Shutdown(shutdownCtx)
}

// paymentPort adapts the Payment gRPC client to usecase.PaymentClient.
//
// The two interfaces are identical apart from the request type, which is
// declared separately on each side because an outbound adapter may not import
// usecase (AGENTS.md §4). Converting here, in the wiring, is the price of that
// rule and it is one struct literal.
type paymentPort struct{ c *paymentclient.Client }

func (p paymentPort) Charge(ctx context.Context, req usecase.ChargeRequest) (domain.PaymentResult, error) {
	return p.c.Charge(ctx, paymentclient.ChargeRequest{
		ReservationID:  req.ReservationID,
		UserID:         req.UserID,
		AmountCents:    req.AmountCents,
		IdempotencyKey: req.IdempotencyKey,
	})
}

func (p paymentPort) GetCharge(ctx context.Context, idempotencyKey string) (domain.PaymentResult, error) {
	return p.c.GetCharge(ctx, idempotencyKey)
}

func (p paymentPort) Refund(ctx context.Context, chargeID string, amountCents int64, idempotencyKey string) error {
	return p.c.Refund(ctx, chargeID, amountCents, idempotencyKey)
}
