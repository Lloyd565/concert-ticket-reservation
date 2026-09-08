package usecase_test

import (
	"strings"
	"testing"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/usecase"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	const password = "correct-horse-battery-staple"

	encoded, err := usecase.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("hash %q is not PHC-format Argon2id", encoded)
	}
	// The plaintext must not be recoverable from, or present in, the stored form.
	if strings.Contains(encoded, password) {
		t.Fatal("the encoded hash contains the password")
	}
	if !usecase.VerifyPassword(encoded, password) {
		t.Error("the correct password did not verify")
	}
	if usecase.VerifyPassword(encoded, password+"x") {
		t.Error("a wrong password verified")
	}
}

// TestPasswordHashIsSalted: two hashes of the same password must differ, or a
// leaked table tells an attacker which accounts share a password.
func TestPasswordHashIsSalted(t *testing.T) {
	a, err := usecase.HashPassword("same-password-for-both")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := usecase.HashPassword("same-password-for-both")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical - the salt is not random")
	}
}

// TestVerifyRejectsMalformedHashes: a corrupt or truncated row must fail
// closed. Returning true - or panicking into a handler that treats the panic as
// success - would be an authentication bypass.
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	good, err := usecase.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	parts := strings.Split(good, "$")

	for _, tc := range []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"not a hash", "hunter2"},
		{"wrong algorithm", "$argon2i$" + strings.Join(parts[2:], "$")},
		{"truncated", strings.Join(parts[:4], "$")},
		{"unparseable parameters", "$argon2id$v=19$m=x,t=y,p=z$" + parts[4] + "$" + parts[5]},
		{"corrupt salt", "$argon2id$" + parts[2] + "$" + parts[3] + "$!!!!$" + parts[5]},
		{"corrupt digest", "$argon2id$" + parts[2] + "$" + parts[3] + "$" + parts[4] + "$!!!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if usecase.VerifyPassword(tc.encoded, "correct-horse-battery-staple") {
				t.Error("a malformed hash verified")
			}
		})
	}
}

// TestVerifyReadsParametersFromTheHash proves the cost parameters come from the
// stored string rather than from the package constants - which is what makes
// raising the cost later a code change instead of a forced password reset.
func TestVerifyReadsParametersFromTheHash(t *testing.T) {
	encoded, err := usecase.HashPassword("parameters-come-from-the-hash")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	tampered := strings.Replace(encoded, "m=65536", "m=32768", 1)
	if tampered == encoded {
		t.Fatal("hash does not carry m=65536; update this test's substitution")
	}
	// Verification recomputes with m=32768 and gets a different digest, so this
	// must fail. If the constants were used instead, it would pass.
	if usecase.VerifyPassword(tampered, "parameters-come-from-the-hash") {
		t.Error("a hash with tampered parameters verified against the original digest")
	}
}
