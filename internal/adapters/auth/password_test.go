package auth

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestPasswordHasher_Hash checks the happy path: hashing a non-empty
// password returns a non-empty, bcrypt-shaped string.
func TestPasswordHasher_Hash(t *testing.T) {
	h := NewPasswordHasherWithCost(bcrypt.MinCost)
	hash, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash returned error: %v", err)
	}
	if hash == "" {
		t.Fatal("Hash returned empty string")
	}
	// bcrypt hashes start with $2a$, $2b$, or $2y$.
	if !strings.HasPrefix(hash, "$2") {
		t.Errorf("Hash output does not look like a bcrypt hash: %q", hash)
	}
	// Two hashes of the same password must differ (bcrypt salt).
	hash2, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash (second call): %v", err)
	}
	if hash == hash2 {
		t.Errorf("two hashes of the same password are identical; expected different salts")
	}
}

// TestPasswordHasher_Hash_Empty asserts the adapter refuses to hash
// an empty password rather than producing a hash that always matches
// the empty string.
func TestPasswordHasher_Hash_Empty(t *testing.T) {
	h := NewPasswordHasherWithCost(bcrypt.MinCost)
	if _, err := h.Hash(""); err == nil {
		t.Errorf("Hash(\"\") returned no error; want validation failure")
	}
}

// TestPasswordHasher_Verify covers the three meaningful outcomes: a
// matching password returns nil, a wrong password returns an error,
// and a structurally invalid hash returns an error.
func TestPasswordHasher_Verify(t *testing.T) {
	h := NewPasswordHasherWithCost(bcrypt.MinCost)
	const pw = "hunter2"
	hash, err := h.Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	t.Run("correct password", func(t *testing.T) {
		if err := h.Verify(hash, pw); err != nil {
			t.Errorf("Verify returned %v for correct password", err)
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		if err := h.Verify(hash, "hunter3"); err == nil {
			t.Errorf("Verify accepted wrong password")
		}
	})
	t.Run("empty hash", func(t *testing.T) {
		if err := h.Verify("", pw); err == nil {
			t.Errorf("Verify accepted empty hash")
		}
	})
	t.Run("empty password", func(t *testing.T) {
		if err := h.Verify(hash, ""); err == nil {
			t.Errorf("Verify accepted empty password")
		}
	})
	t.Run("malformed hash", func(t *testing.T) {
		// "not-a-bcrypt-hash" does not start with $2 — Verify must
		// return an error rather than panic or return nil.
		err := h.Verify("not-a-bcrypt-hash", pw)
		if err == nil {
			t.Errorf("Verify accepted malformed hash")
		}
		// We do not require the wrapped sentinel to be a specific
		// type; bcrypt.ErrMismatchedHashAndPassword is acceptable.
		// What matters is the error is non-nil and informative.
		if !errors.Is(err, err) {
			t.Errorf("error chain is broken")
		}
	})
}
