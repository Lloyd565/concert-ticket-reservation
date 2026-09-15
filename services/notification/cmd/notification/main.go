// Command notification is the Notification service: it turns booking events
// into emails (ARCHITECTURE.md §3.1).
//
// It is purely a consumer. No service calls it and it calls no service, so it
// can be stopped at any moment without Booking noticing: events wait in its
// durable queue and drain when it comes back (§3.3).
//
// This file does wiring, configuration and graceful shutdown only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/config"
	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/events"
	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/mailer"
	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/repository/postgres"
	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/usecase"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("notification service stopped", "error", err)
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

	notifier := usecase.NewNotifier(postgres.NewRepo(pool), mailer.NewMock(cfg.MailMode, log), 0, 0, log)
	consumer := events.NewConsumer(cfg.AMQPURL, notifier, log)

	consumed := make(chan struct{})
	go func() {
		consumer.Run(ctx)
		close(consumed)
	}()

	// Readiness means "consuming", not merely "running": compose waits on it, so
	// by the time the stack reports healthy this service's queue exists and
	// events published from then on have somewhere to wait.
	srv := &http.Server{
		Addr: ":" + cfg.HTTPPort,
		Handler: healthMux(func() error {
			if err := pool.Ping(ctx); err != nil {
				return err
			}
			if !consumer.Ready() {
				return errors.New("not consuming")
			}
			return nil
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("notification health listening", "addr", srv.Addr, "mail_mode", string(cfg.MailMode))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Let the consumer settle the message in hand. Anything it has not
	// acknowledged goes back to the queue when its connection closes.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	select {
	case <-consumed:
	case <-shutdownCtx.Done():
	}
	return srv.Shutdown(shutdownCtx)
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
