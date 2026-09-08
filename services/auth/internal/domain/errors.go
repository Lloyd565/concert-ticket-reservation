package domain

import "errors"

// Sentinel errors. Transport maps these to gRPC codes; SQL errors never reach a
// caller (AGENTS.md §5).
var (
	// ErrEmailTaken means registration hit the users.email UNIQUE index.
	ErrEmailTaken = errors.New("email already registered")

	// ErrInvalidCredentials covers both "no such user" and "wrong password",
	// deliberately. Distinguishing them turns the login endpoint into an
	// account-enumeration oracle.
	ErrInvalidCredentials = errors.New("invalid credentials")

	// ErrInvalidRefreshToken covers unknown, expired and revoked tokens - same
	// reasoning as above, and the caller's remedy is identical in every case:
	// log in again.
	ErrInvalidRefreshToken = errors.New("invalid refresh token")

	// ErrInvalidRole means a role outside the fixed set was supplied.
	ErrInvalidRole = errors.New("invalid role")

	// ErrWeakPassword means the password is shorter than MinPasswordLength.
	ErrWeakPassword = errors.New("password too short")

	// ErrInvalidInput marks a caller mistake so the boundary answers
	// InvalidArgument instead of Internal.
	ErrInvalidInput = errors.New("invalid input")

	// ErrUserNotFound is the repository's "no row" signal.
	ErrUserNotFound = errors.New("user not found")
)
