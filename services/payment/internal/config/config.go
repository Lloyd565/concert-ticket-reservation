// Package config loads and validates the service's configuration. Everything
// comes from the environment; no secrets are ever compiled in or committed
// (NFR-5.1, AGENTS.md §2 rule 12). NFR-5.4 makes that stricter here than
// elsewhere: this is the only service that ever holds payment credentials.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/provider"
)

// Config is the Payment service's runtime configuration.
type Config struct {
	DatabaseURL string
	// HTTPPort serves liveness and readiness only (NFR-4.4). Payment has no
	// public REST surface: it is reached over gRPC by Booking, and by nothing
	// else.
	HTTPPort string
	GRPCPort string

	// ProviderMode selects the mock provider's behaviour: succeed, decline or
	// hang. Real money is out of scope (ARCHITECTURE.md §7); this is the knob
	// that makes the saga's failure paths demonstrable in a running stack, not
	// only in tests.
	ProviderMode provider.Mode
	// ProviderLatency simulates a provider that is slow but working.
	ProviderLatency time.Duration
	// ProviderTimeout bounds one provider call. Exceeding it produces an
	// UNKNOWN outcome, never a failed one. It is per attempt: ProviderMaxAttempts
	// of them, plus backoff, must fit inside BOOKING_PAYMENT_TIMEOUT, which gRPC
	// carries into every call. Otherwise the first attempt uses the caller's
	// whole deadline and the retry never runs.
	ProviderTimeout time.Duration
	// ProviderMaxAttempts bounds retries (FR-4.4). Safe only because the
	// idempotency key is passed through to the provider.
	ProviderMaxAttempts int
	// AMQPURL is the broker the outbox relay publishes to (D9). It is never on
	// the path of a charge: with RabbitMQ down, events wait in the outbox. It
	// carries credentials, so it has no default.
	AMQPURL string
}

// Load reads configuration from the environment. It fails fast: a service that
// starts without a database is a service that reports healthy and serves errors.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:         os.Getenv("PAYMENT_DATABASE_URL"),
		AMQPURL:             os.Getenv("PAYMENT_AMQP_URL"),
		HTTPPort:            env("PAYMENT_HTTP_PORT", "8082"),
		GRPCPort:            env("PAYMENT_GRPC_PORT", "9093"),
		ProviderLatency:     0,
		ProviderTimeout:     time.Second,
		ProviderMaxAttempts: 2,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("PAYMENT_DATABASE_URL is required")
	}
	if cfg.AMQPURL == "" {
		// Required even though the broker may be down: a relay with nowhere to
		// publish would let the outbox grow forever while reporting healthy.
		return Config{}, fmt.Errorf("PAYMENT_AMQP_URL is required")
	}

	mode, err := provider.ParseMode(env("PAYMENT_PROVIDER_MODE", string(provider.ModeSucceed)))
	if err != nil {
		return Config{}, err
	}
	cfg.ProviderMode = mode

	if cfg.ProviderLatency, err = duration("PAYMENT_PROVIDER_LATENCY", cfg.ProviderLatency, true); err != nil {
		return Config{}, err
	}
	if cfg.ProviderTimeout, err = duration("PAYMENT_PROVIDER_TIMEOUT", cfg.ProviderTimeout, false); err != nil {
		return Config{}, err
	}
	if cfg.ProviderMaxAttempts, err = positiveInt("PAYMENT_PROVIDER_MAX_ATTEMPTS", cfg.ProviderMaxAttempts); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if d < 0 || (d == 0 && !allowZero) {
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
