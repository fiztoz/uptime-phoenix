package services

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestOIDCPKCE_VerifierAndChallengeShape(t *testing.T) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("generateCodeVerifier: %v", err)
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier length = %d, want 43–128", len(verifier))
	}
	if !pkceUnreserved.MatchString(verifier) {
		t.Fatalf("verifier %q fails RFC 7636 unreserved charset", verifier)
	}

	// Two calls produce distinct verifiers (high entropy).
	v2, err := generateCodeVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if verifier == v2 {
		t.Fatal("expected distinct verifiers")
	}

	// Challenge is deterministic BASE64URL(SHA256(verifier)) without padding.
	challenge := s256CodeChallenge(verifier)
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge = %q, want %q", challenge, want)
	}
	if len(challenge) != 43 {
		t.Fatalf("S256 challenge length = %d, want 43", len(challenge))
	}
	// Unpadded base64url: no '='.
	for _, c := range challenge {
		if c == '=' {
			t.Fatal("challenge must not be padded")
		}
	}
	// Same verifier → same challenge.
	if s256CodeChallenge(verifier) != challenge {
		t.Fatal("challenge not deterministic")
	}
	// Different verifier → different challenge.
	if s256CodeChallenge(v2) == challenge {
		t.Fatal("expected distinct challenges for distinct verifiers")
	}
}
