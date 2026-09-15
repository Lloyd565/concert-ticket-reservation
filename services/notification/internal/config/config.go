// Package config loads and validates the service's configuration. Everything
// comes from the environment; no secrets are compiled in or committed
// (AGENTS.md §2 rule 12).
package config

import (
	"fmt"
	"os"

	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/mailer"
)

// Config is the Notification service's runtime configuration.
type Config struct {
	DatabaseURL string
	// AMQPURL is the broker. It carries credentials, so it has no default.
	AMQPURL string
	// HTTPPort serves liveness and readiness only.
	HTTPPort string
	// MailMode selects how the mock mail provider answers: succeed or fail.
	MailMode mailer.Mode
}

// Load reads configuration from the environment and fails fast on anything
// missing.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("NOTIFICATION_DATABASE_URL"),
		AMQPURL:     os.Getenv("NOTIFICATION_AMQP_URL"),
		HTTPPort:    env("NOTIFICATION_HTTP_PORT", "8083"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("NOTIFICATION_DATABASE_URL is required")
	}
	if cfg.AMQPURL == "" {
		return Config{}, fmt.Errorf("NOTIFICATION_AMQP_URL is required")
	}
	mode, err := mailer.ParseMode(env("NOTIFICATION_MAIL_MODE", string(mailer.ModeSucceed)))
	if err != nil {
		return Config{}, err
	}
	cfg.MailMode = mode
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
