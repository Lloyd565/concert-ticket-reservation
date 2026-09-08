// Package token mints signed access tokens and opaque refresh tokens.
//
// Signing is pure computation over an injected secret - no I/O, no network - so
// it is used directly by usecase rather than hidden behind a port. There is
// exactly one implementation and there is no test that wants to fake it.
//
// The verifying half of this contract lives in the gateway, deliberately
// duplicated rather than shared: the coupling between Auth and the gateway is
// the set of claim names below, which is a contract, not a package. Sharing a
// Go package here would mean a gateway release could not lag an Auth release.
package token

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims is the access token payload. `role` is the claim the gateway
// authorizes on; `sub` is the user ID that Booking records as the reservation
// owner. Changing either name is a breaking change for the gateway.
type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// Signer mints access tokens.
type Signer struct {
	secret   []byte
	issuer   string
	audience string
	ttl      time.Duration
}

// NewSigner returns a Signer. ttl is the access-token lifetime and is the upper
// bound on how long a revoked session keeps working, because the gateway
// validates locally and never asks Auth about an access token
// (ARCHITECTURE.md §3.3).
func NewSigner(secret []byte, issuer, audience string, ttl time.Duration) *Signer {
	return &Signer{secret: secret, issuer: issuer, audience: audience, ttl: ttl}
}

// TTL is the access-token lifetime, reported to clients as expires_in.
func (s *Signer) TTL() time.Duration { return s.ttl }

// Sign returns a signed HS256 access token for the given subject and role.
func (s *Signer) Sign(userID, role string, now time.Time) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    s.issuer,
			Audience:  jwt.ClaimStrings{s.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
			// A unique ID per token, so a future revocation list has something
			// to name a single token by.
			ID: uuid.Must(uuid.NewV7()).String(),
		},
	})
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return signed, nil
}

// NewRefreshToken returns 256 bits of CSPRNG output, base64url-encoded.
//
// It carries no claims on purpose: a refresh token is checked against a table,
// so there is nothing for a client to read out of it and nothing for an
// attacker to tamper with.
func NewRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
