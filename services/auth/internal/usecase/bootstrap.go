package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/domain"
)

// BootstrapOutcome says what a call to BootstrapAdmin actually did, so the
// caller can log it accurately instead of guessing.
type BootstrapOutcome int

const (
	// BootstrapCreated means this call created the first admin.
	BootstrapCreated BootstrapOutcome = iota
	// BootstrapAdminPresent means an admin already existed and nothing changed.
	BootstrapAdminPresent
	// BootstrapEmailTaken means no admin existed but the configured address is
	// already registered, so no account was created. Either another replica won
	// the race, or the address belongs to a non-admin user.
	BootstrapEmailTaken
)

// BootstrapAdmin creates the first admin account if there is not one already.
//
// It exists because the system is otherwise unbootstrappable: the gateway lets
// only an admin register a privileged account, so with an empty database there
// is no way to get the first one without reaching into the database by hand.
// That manual step is exactly what PRD §7 criterion 5 rules out.
//
// It is safe to call on every startup. The guard is a read of the admin role,
// so a second call - or a hundredth - is a single SELECT and no write.
//
// It deliberately does not promote an existing account. Reading a role change
// out of an environment variable would turn "who can edit the deployment
// config" into "who can become an admin", which is a larger blast radius than
// this feature is worth. If the configured address is already taken, this
// reports BootstrapEmailTaken and changes nothing.
func (a *Accounts) BootstrapAdmin(ctx context.Context, email, password string) (BootstrapOutcome, error) {
	exists, err := a.users.AdminExists(ctx)
	if err != nil {
		return BootstrapAdminPresent, fmt.Errorf("bootstrap admin: %w", err)
	}
	if exists {
		return BootstrapAdminPresent, nil
	}

	// Register applies the same validation every other account gets: a weak
	// bootstrap password is not more acceptable for being in a config file.
	if _, err := a.Register(ctx, email, password, string(domain.RoleAdmin)); err != nil {
		// Two replicas can start together, both read "no admin", and both try
		// to insert. The email UNIQUE index settles it; the loser has nothing
		// to do and must not fail its own startup over it.
		if errors.Is(err, domain.ErrEmailTaken) {
			return BootstrapEmailTaken, nil
		}
		return BootstrapAdminPresent, fmt.Errorf("bootstrap admin: %w", err)
	}
	return BootstrapCreated, nil
}
