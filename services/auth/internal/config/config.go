// Package config loads and validates the service's configuration. Everything
// comes from the environment; no secrets are ever compiled in or committed
// (NFR-5.1, AGENTS.md §2 rule 12).
package config

import (
	"fmt"
	"os"
	"time"
)

// MinSecretLength is the shortest accepted JWT signing secret. HS256 keys
// shorter than the 256-bit hash output weaken the MAC, and a short secret is
// exactly the kind of thing that gets set to "secret" in a hurry and shipped.
const MinSecretLength = 32

// Config is the Auth service's runtime configuration.
type Config struct {
	DatabaseURL string
	GRPCPort    string
	// HTTPPort serves liveness and readiness only. gRPC has its own health
	// protocol, but no probe binary for it exists in the runtime image, so the
	// container healthcheck uses plain HTTP - same shape as Booking.
	HTTPPort string

	// JWTSecret is shared with the gateway. Symmetric signing means the gateway
	// can verify without a key-fetch round trip; it also means the gateway
	// holds a key that can mint tokens.
	//
	// ponytail: HS256 with a shared secret. Move to RS256/EdDSA with a JWKS
	// endpoint when a service that should only verify must stop being able to
	// sign - i.e. as soon as anything outside this repo validates these tokens.
	JWTSecret   []byte
	JWTIssuer   string
	JWTAudience string

	// AccessTTL bounds how long a revoked session keeps working, because the
	// gateway validates locally and never calls back here (ARCHITECTURE.md §3.3).
	AccessTTL  time.Duration
	RefreshTTL time.Duration

	// BootstrapAdminEmail and BootstrapAdminPassword seed the first admin
	// account at startup, so that `docker compose up` yields a demoable system
	// with no manual database step (PRD §7 criterion 5).
	//
	// Both empty disables the bootstrap entirely. Exactly one set is a
	// configuration mistake and is rejected rather than half-honoured.
	BootstrapAdminEmail    string
	BootstrapAdminPassword string
}

// BootstrapEnabled reports whether an admin bootstrap was configured.
func (c Config) BootstrapEnabled() bool {
	return c.BootstrapAdminEmail != "" && c.BootstrapAdminPassword != ""
}

// Load reads configuration from the environment. It fails fast: a service that
// starts without a database or a signing secret is a service that reports
// healthy and serves errors.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("AUTH_DATABASE_URL"),
		GRPCPort:    env("AUTH_GRPC_PORT", "9091"),
		HTTPPort:    env("AUTH_HTTP_PORT", "8081"),
		JWTSecret:   []byte(os.Getenv("JWT_SECRET")),
		JWTIssuer:   env("JWT_ISSUER", "concert-auth"),
		JWTAudience: env("JWT_AUDIENCE", "concert-api"),
		AccessTTL:   15 * time.Minute,
		RefreshTTL:  7 * 24 * time.Hour,

		BootstrapAdminEmail:    os.Getenv("BOOTSTRAP_ADMIN_EMAIL"),
		BootstrapAdminPassword: os.Getenv("BOOTSTRAP_ADMIN_PASSWORD"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("AUTH_DATABASE_URL is required")
	}
	if len(cfg.JWTSecret) < MinSecretLength {
		return Config{}, fmt.Errorf("JWT_SECRET is required and must be at least %d bytes", MinSecretLength)
	}
	var err error
	if cfg.AccessTTL, err = duration("AUTH_ACCESS_TTL", cfg.AccessTTL); err != nil {
		return Config{}, err
	}
	if cfg.RefreshTTL, err = duration("AUTH_REFRESH_TTL", cfg.RefreshTTL); err != nil {
		return Config{}, err
	}
	// Half-configured means somebody intended a bootstrap and mistyped a
	// variable name. Starting anyway would produce a system with no admin and
	// no error to explain why.
	if (cfg.BootstrapAdminEmail == "") != (cfg.BootstrapAdminPassword == "") {
		return Config{}, fmt.Errorf("BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD must be set together, or neither")
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
