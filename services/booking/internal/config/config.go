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
