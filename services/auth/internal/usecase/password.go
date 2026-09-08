package usecase

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are the RFC 9106 second recommended profile:
// 64 MiB of memory, three passes, four lanes. Memory is the parameter that
// actually hurts a GPU or ASIC attacker, so it is the one worth spending on.
//
// They are constants rather than configuration because a password hash is only
// as strong as its weakest deployment, and an environment variable is an
// invitation to weaken one. Raising them is a code change; existing hashes keep
// verifying because every hash carries the parameters it was made with.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 3
	argonLanes     = 4
	argonSaltLen   = 16
	argonKeyLen    = 32
)

// HashPassword returns a PHC-format Argon2id string: algorithm, version,
// parameters, salt and digest, all in one field. Self-describing hashes are
// what make a future cost increase a migration-free change.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonLanes, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonLanes,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password produced encoded.
//
// The parameters come from encoded, not from the constants above: a hash
// written under an older cost must still verify. A malformed hash returns false
// rather than an error, so a corrupt row cannot become an authentication
// bypass by way of a mishandled error return.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory uint32
	var times uint32
	var lanes uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &times, &lanes); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, times, memory, lanes, uint32(len(want)))
	// Constant time: a byte-by-byte comparison leaks the length of the matching
	// prefix through timing, which is enough to forge a digest one byte at a
	// time given enough samples.
	return subtle.ConstantTimeCompare(got, want) == 1
}
