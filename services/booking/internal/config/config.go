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
	GRPCPort string
	// RedisAddr is the hold store, and in P3 it is the lock: no Redis, no
	// holds. The service still starts without it and still serves seat maps,
	// but every hold request is refused rather than taken unlocked (D12).
	RedisAddr string
	// AMQPURL is the broker the outbox relay publishes to (D9). Unlike Redis it
	// is not a dependency of any request: with RabbitMQ down, events wait in the
	// outbox and nothing a customer does fails. It carries credentials, so it
	// has no default.
	AMQPURL string
	// HoldTTL is the checkout window. In P3 it is a real TTL on a Redis key,
	// so it expires with nobody having to come back and look.
	HoldTTL time.Duration

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
	// stay well under HoldTTL: every pass is also what pushes the Redis holds
	// of the reservations it owns back out, and a pass that arrives after the
	// TTL has lapsed arrives too late to keep them (D8).
	ReconcileInterval time.Duration
	// ReconcileGrace is how long a payment outcome may stay unknown before the
	// job intervenes, and how long after a "no charge" release it waits before
	// asking Payment again. It must exceed PaymentTimeout, or that second look
	// can come before a charge started ahead of the release has landed.
	ReconcileGrace time.Duration
}

// Load reads configuration from the environment. It fails fast: a service that
// starts without a database is a service that reports healthy and serves errors.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("BOOKING_DATABASE_URL"),
		HTTPPort:    env("BOOKING_HTTP_PORT", "8080"),
		GRPCPort:    env("BOOKING_GRPC_PORT", "9092"),
		RedisAddr:   env("BOOKING_REDIS_ADDR", "redis:6379"),
		AMQPURL:     os.Getenv("BOOKING_AMQP_URL"),
		HoldTTL:     10 * time.Minute,

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
	if cfg.AMQPURL == "" {
		// Required even though the broker may be down: a relay with nowhere to
		// publish would let the outbox grow forever while reporting healthy.
		return Config{}, fmt.Errorf("BOOKING_AMQP_URL is required")
	}
	var err error
	if cfg.HoldTTL, err = duration("BOOKING_HOLD_TTL", cfg.HoldTTL); err != nil {
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
		// backstop: it is what keeps the holds of a possibly-paid customer
		// alive, so running it slower than the TTL means those seats lapse
		// before it ever looks at them (D8).
		return Config{}, fmt.Errorf("BOOKING_RECONCILE_INTERVAL (%s) must be shorter than BOOKING_HOLD_TTL (%s)", cfg.ReconcileInterval, cfg.HoldTTL)
	}
	if cfg.ReconcileGrace <= cfg.PaymentTimeout {
		// The grace period is how long reconciliation waits before trusting a
		// "no charge" answer it released seats on. A charge started just before
		// that release takes up to one payment call to reach Payment, and a
		// recheck that looks sooner can miss it: a paid customer, no seat, and
		// nothing left that knows to refund them.
		return Config{}, fmt.Errorf("BOOKING_RECONCILE_GRACE (%s) must be longer than BOOKING_PAYMENT_TIMEOUT (%s)", cfg.ReconcileGrace, cfg.PaymentTimeout)
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
