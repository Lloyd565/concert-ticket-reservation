// Package domain holds the Auth service's entities and rules. It imports the
// standard library only (AGENTS.md §4): no drivers, no transport, no crypto
// library. IDs are plain strings; parsing happens at the boundaries.
package domain

import (
	"strings"
	"time"
)

// Role is a user's authorization level. It is the one claim the gateway acts on
// without asking anybody, so its set of legal values is fixed here and mirrored
// by a CHECK constraint in the schema.
type Role string

const (
	RoleAttendee  Role = "attendee"
	RoleOrganizer Role = "organizer"
	RoleAdmin     Role = "admin"
)

// ParseRole validates a role string. An empty value defaults to attendee: the
// least privilege, so a caller that omits the field cannot accidentally gain
// one.
func ParseRole(s string) (Role, error) {
	switch r := Role(strings.ToLower(strings.TrimSpace(s))); r {
	case "":
		return RoleAttendee, nil
	case RoleAttendee, RoleOrganizer, RoleAdmin:
		return r, nil
	default:
		return "", ErrInvalidRole
	}
}

// User is a registered account.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	Role         Role
	CreatedAt    time.Time
}

// NormalizeEmail lowercases and trims an address so that the UNIQUE index
// actually prevents duplicate accounts. Without it "A@x.com" and "a@x.com"
// are two accounts as far as Postgres is concerned.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidEmail is a deliberately shallow check: the only authority on whether an
// address exists is a delivered message, so anything stricter than "has a local
// part, an @, and a dotted domain" rejects real addresses for no gain.
func ValidEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.Count(email, "@") != 1 {
		return false
	}
	domain := email[at+1:]
	dot := strings.IndexByte(domain, '.')
	return dot > 0 && dot < len(domain)-1 && !strings.ContainsAny(email, " \t\r\n")
}

// MinPasswordLength is the only password rule enforced. Composition rules
// ("one digit, one symbol") measurably push users toward weaker, more
// predictable passwords; length is the property that actually costs an
// attacker work.
const MinPasswordLength = 12

// RefreshToken is a long-lived credential that can be exchanged for a new
// access token. Only its hash is ever persisted.
type RefreshToken struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Live reports whether the token can still be exchanged at instant now.
func (t RefreshToken) Live(now time.Time) bool {
	return t.RevokedAt == nil && now.Before(t.ExpiresAt)
}
