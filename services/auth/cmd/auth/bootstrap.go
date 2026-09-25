package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/usecase"
)

// bootstrapBackoffCap is the longest gap between bootstrap attempts. The job is
// waiting for a schema to appear, which happens once, so there is no reason to
// poll faster than this after the first few tries.
const bootstrapBackoffCap = 15 * time.Second

// runBootstrap seeds the first admin account, retrying until it succeeds.
//
// It runs in the background rather than before the listeners, on purpose. Auth
// starts independently of every other component, including its own database
// (ARCHITECTURE.md §3.3, and the P1 exit criterion that services start in any
// order). Blocking startup on a database round trip would give that up in
// exchange for nothing: an admin account is not needed to serve a login.
//
// The usual reason for a first failure is ordinary and self-correcting - the
// container is up before `make migrate-up` has created the schema - so the loop
// simply waits for the table to exist. A configuration mistake, by contrast, is
// never going to fix itself, so it is reported once and the loop stops.
func runBootstrap(ctx context.Context, accounts *usecase.Accounts, email, password string, log *slog.Logger) {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		outcome, err := accounts.BootstrapAdmin(ctx, email, password)
		switch {
		case err == nil:
			switch outcome {
			case usecase.BootstrapCreated:
				// The address, never the password.
				log.InfoContext(ctx, "bootstrapped the first admin account", "email", email)
			case usecase.BootstrapEmailTaken:
				log.WarnContext(ctx, "admin bootstrap skipped: the configured address is already registered",
					"email", email, "hint", "either another replica created it, or this address belongs to a non-admin user")
			case usecase.BootstrapAdminPresent:
				log.DebugContext(ctx, "admin bootstrap not needed: an admin already exists")
			}
			return

		case permanentBootstrapFailure(err):
			// A weak password or a malformed address will fail identically
			// forever. Retrying would only bury the message.
			log.ErrorContext(ctx, "admin bootstrap cannot succeed with this configuration; no admin was created",
				"email", email, "error", err)
			return

		default:
			// Almost always "relation \"users\" does not exist" on a first run.
			level := slog.LevelDebug
			if attempt == 1 {
				level = slog.LevelWarn
			}
			log.Log(ctx, level, "admin bootstrap attempt failed; will retry",
				"attempt", attempt, "retry_in", backoff.String(), "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > bootstrapBackoffCap {
			backoff = bootstrapBackoffCap
		}
	}
}

// permanentBootstrapFailure reports whether an error will recur identically on
// every retry. These are all caller mistakes in the deployment config.
func permanentBootstrapFailure(err error) bool {
	return errors.Is(err, domain.ErrWeakPassword) ||
		errors.Is(err, domain.ErrInvalidInput) ||
		errors.Is(err, domain.ErrInvalidRole)
}
