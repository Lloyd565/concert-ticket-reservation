// Package token verifies access tokens locally.
//
// "Locally" is the whole point. Everything the gateway needs in order to accept
// or reject an access token is inside the token and cryptographically bound to
// it, so no request in this system requires a call to Auth (ARCHITECTURE.md
// §3.3). Auth can be down, restarting or migrating and every already-issued
// session keeps working.
//
// What that buys and what it costs:
//
//   - Checked here, on every request: the HS256 signature, exp, nbf, iat, iss,
//     aud, and that `role` is one of the three legal values.
//   - Not knowable here: whether the user was deleted, demoted, or logged out
//     since the token was minted. That window is bounded by the access-token
//     TTL - 15 minutes - and is closed at the next refresh, which is a call to
//     Auth by definition. It is a deliberate trade: 15 minutes of stale
//     authorization in exchange for Auth not being a hard dependency of every
//     request in the system.
//
// This mirrors the signing half in services/auth/internal/token. The two are
// duplicated rather than shared: the contract between them is the set of claim
// names, and a shared Go package would mean a gateway release could not lag an
// Auth release.
package token

import (
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// Verification failures. Both are answered with 401; they are distinguished so
// a client can tell "refresh and retry" from "your token is nonsense".
var (
	// ErrTokenExpired means the signature was good but exp has passed.
	ErrTokenExpired = errors.New("access token expired")
	// ErrInvalidToken covers a bad signature, a wrong algorithm, a wrong
	// issuer or audience, a malformed token and a missing or illegal claim.
	ErrInvalidToken = errors.New("invalid access token")
)

// Claims is what a validated token asserts. These field names track the `sub`
// and `role` claims that Auth mints; renaming either on one side breaks the
// other.
type Claims struct {
	UserID string
	Role   string
}

// legalRoles mirrors the CHECK constraint on auth_db.users.role. A token whose
// role is outside this set is rejected rather than treated as unprivileged: it
// means the two services disagree about the contract, and guessing which one is
// right is how a privilege bug gets shipped.
var legalRoles = map[string]bool{"attendee": true, "organizer": true, "admin": true}

type claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// Verifier validates access tokens against a shared secret.
type Verifier struct {
	secret   []byte
	issuer   string
	audience string
}

// NewVerifier returns a Verifier.
func NewVerifier(secret []byte, issuer, audience string) *Verifier {
	return &Verifier{secret: secret, issuer: issuer, audience: audience}
}

// Verify parses and validates a raw access token.
func (v *Verifier) Verify(raw string) (Claims, error) {
	var c claims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return v.secret, nil },
		// Algorithm pinning, and it is not optional. Without it a token can
		// name its own algorithm: `alg: none` is accepted as unsigned, and an
		// RS256 deployment can be tricked into verifying an HS256 token using
		// its public key as the HMAC secret. Naming the one algorithm we mint
		// with closes both.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		// A token minted by some other system that happens to share this secret
		// is not a token for this API.
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		// A token without exp would never expire, which would make the
		// bounded-staleness argument above false.
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Claims{}, ErrTokenExpired
		}
		// The underlying reason is deliberately not returned to the caller -
		// "signature invalid" versus "audience wrong" is free reconnaissance -
		// but it is wrapped so it can be logged at debug level.
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	subject, err := c.GetSubject()
	if err != nil || subject == "" {
		return Claims{}, fmt.Errorf("%w: missing subject", ErrInvalidToken)
	}
	if !legalRoles[c.Role] {
		return Claims{}, fmt.Errorf("%w: role %q is not recognised", ErrInvalidToken, c.Role)
	}
	return Claims{UserID: subject, Role: c.Role}, nil
}
