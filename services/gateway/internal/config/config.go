// Package config loads and validates the service's configuration. Everything
// comes from the environment; no secrets are ever compiled in or committed
// (NFR-5.1, AGENTS.md §2 rule 12).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// MinSecretLength must match the Auth service's. A gateway that would accept a
// weaker secret than Auth will mint with is a gateway that fails open.
const MinSecretLength = 32

// Config is the gateway's runtime configuration.
type Config struct {
	HTTPPort string

	AuthAddr    string
	BookingAddr string
	// UpstreamTimeout bounds every outbound gRPC call.
	UpstreamTimeout time.Duration

	// JWTSecret is the same secret Auth signs with. Verification is local: the
	// gateway never calls Auth to check an access token (ARCHITECTURE.md §3.3).
	JWTSecret   []byte
	JWTIssuer   string
	JWTAudience string

	// RateLimitRPS and RateLimitBurst apply to each client IP and, separately,
	// to each authenticated user, per gateway replica.
	RateLimitRPS   float64
	RateLimitBurst int
	// RateLimitIdle is how long an idle bucket is kept before eviction.
	RateLimitIdle time.Duration
}

// Load reads configuration from the environment. It fails fast: a gateway that
// starts without a verification secret would have to either reject everything
// or accept everything, and one of those is a catastrophe.
func Load() (Config, error) {
	cfg := Config{
		HTTPPort:        env("GATEWAY_HTTP_PORT", "8000"),
		AuthAddr:        env("GATEWAY_AUTH_ADDR", "auth:9091"),
		BookingAddr:     env("GATEWAY_BOOKING_ADDR", "booking:9092"),
		UpstreamTimeout: 5 * time.Second,
		JWTSecret:       []byte(os.Getenv("JWT_SECRET")),
		JWTIssuer:       env("JWT_ISSUER", "concert-auth"),
		JWTAudience:     env("JWT_AUDIENCE", "concert-api"),
		RateLimitRPS:    20,
		RateLimitBurst:  40,
		RateLimitIdle:   5 * time.Minute,
	}
	if len(cfg.JWTSecret) < MinSecretLength {
		return Config{}, fmt.Errorf("JWT_SECRET is required and must be at least %d bytes", MinSecretLength)
	}
	var err error
	if cfg.UpstreamTimeout, err = duration("GATEWAY_UPSTREAM_TIMEOUT", cfg.UpstreamTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitIdle, err = duration("GATEWAY_RATE_LIMIT_IDLE", cfg.RateLimitIdle); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitRPS, err = positiveFloat("GATEWAY_RATE_LIMIT_RPS", cfg.RateLimitRPS); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitBurst, err = positiveInt("GATEWAY_RATE_LIMIT_BURST", cfg.RateLimitBurst); err != nil {
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

func positiveFloat(key string, fallback float64) (float64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %v", key, v)
	}
	return v, nil
}

func positiveInt(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %d", key, v)
	}
	return v, nil
}
