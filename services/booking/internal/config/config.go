// Package config loads and validates the service's configuration. Everything
// comes from the environment; no secrets are ever compiled in or committed
// (NFR-5.1, AGENTS.md §2 rule 12).
package config

import (
	"fmt"
	"os"
	"time"
)

// Config is the Booking service's runtime configuration.
type Config struct {
	DatabaseURL string
	HTTPPort    string
	// GRPCPort serves the internal API the gateway routes to (ARCHITECTURE.md
	// §4). REST stays on HTTPPort: the two transports are separate doors onto
	// the same usecase, not a replacement of one by the other.
	GRPCPort      string
	HoldTTL       time.Duration
	SweepInterval time.Duration

	// PaymentAddr is the Payment service's gRPC address. Dialled lazily:
	// Booking must serve seat maps and holds whether or not Payment is up
	// (ARCHITECTURE.md §3.3).
	PaymentAddr string
	// PaymentTimeout bounds one call to Payment. Exceeding it is an UNKNOWN
	// outcome, never a failed one (D8).
	//
	// It must stay comfortably under the gateway's own upstream deadline
	// (GATEWAY_UPSTREAM_TIMEOUT, 5s by default). Deadlines have to nest: if the
	// gateway gives up first, the customer gets a 504 instead of the 202 that
	// tells them their payment is still settling, for a reservation that is
	// alive and being reconciled.
	PaymentTimeout time.Duration
	// BreakerThreshold is how many consecutive failed Payment calls open the
	// circuit; BreakerCooldown is how long before it probes again.
	BreakerThreshold int
	BreakerCooldown  time.Duration

	// ReconcileInterval is how often the reconciliation job sweeps. It must
	// stay well under HoldTTL: the reservations it owns are the ones the
	// sweeper is forbidden to touch, so nothing else will free their seats.
	ReconcileInterval time.Duration
	// ReconcileGrace is how long a payment outcome may stay unknown before the
	// job intervenes.
	ReconcileGrace time.Duration
}

// Load reads configuration from the environment. It fails fast: a service that
// starts without a database is a service that reports healthy and serves errors.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:   os.Getenv("BOOKING_DATABASE_URL"),
		HTTPPort:      env("BOOKING_HTTP_PORT", "8080"),
		GRPCPort:      env("BOOKING_GRPC_PORT", "9092"),
		HoldTTL:       10 * time.Minute,
		SweepInterval: 5 * time.Second,

		PaymentAddr:       env("BOOKING_PAYMENT_ADDR", "payment:9093"),
		PaymentTimeout:    3 * time.Second,
		BreakerThreshold:  5,
		BreakerCooldown:   30 * time.Second,
		ReconcileInterval: 15 * time.Second,
		ReconcileGrace:    30 * time.Second,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("BOOKING_DATABASE_URL is required")
	}
	var err error
	if cfg.HoldTTL, err = duration("BOOKING_HOLD_TTL", cfg.HoldTTL); err != nil {
		return Config{}, err
	}
	if cfg.SweepInterval, err = duration("BOOKING_SWEEP_INTERVAL", cfg.SweepInterval); err != nil {
		return Config{}, err
	}
	if cfg.PaymentTimeout, err = duration("BOOKING_PAYMENT_TIMEOUT", cfg.PaymentTimeout); err != nil {
		return Config{}, err
	}
	if cfg.BreakerCooldown, err = duration("BOOKING_BREAKER_COOLDOWN", cfg.BreakerCooldown); err != nil {
		return Config{}, err
	}
	if cfg.ReconcileInterval, err = duration("BOOKING_RECONCILE_INTERVAL", cfg.ReconcileInterval); err != nil {
		return Config{}, err
	}
	if cfg.ReconcileGrace, err = duration("BOOKING_RECONCILE_GRACE", cfg.ReconcileGrace); err != nil {
		return Config{}, err
	}
	if cfg.BreakerThreshold, err = positiveInt("BOOKING_BREAKER_THRESHOLD", cfg.BreakerThreshold); err != nil {
		return Config{}, err
	}
	if cfg.ReconcileInterval >= cfg.HoldTTL {
		// A reconciliation job that runs less often than holds expire is not a
		// backstop, it is a seat leak: nothing else will ever release these
		// reservations.
		return Config{}, fmt.Errorf("BOOKING_RECONCILE_INTERVAL (%s) must be shorter than BOOKING_HOLD_TTL (%s)", cfg.ReconcileInterval, cfg.HoldTTL)
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, d)
	}
	return d, nil
}

func positiveInt(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("%s must be at least 1, got %d", key, n)
	}
	return n, nil
}
