package auth

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// otpauthURL matches the canonical otpauth:// provisioning URL produced
// by pquerna/otp. The capture group lets tests extract the label
// (e.g. "Phoenix:alice") for human-readable failure messages.
var otpauthURL = regexp.MustCompile(`^otpauth://totp/([^?]+)\?(.+)$`)

// queryValue returns the URL-decoded value of the named query parameter
// in a raw query string. It exists so tests can assert on percent-encoded
// fields without pulling in a heavier query parsing dependency.
func queryValue(rawQuery, name string) (string, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", err
	}
	return values.Get(name), nil
}

// TestTOTPProvider_GenerateSecret verifies that GenerateSecret returns
// a non-empty secret and a syntactically valid otpauth:// URL.
func TestTOTPProvider_GenerateSecret(t *testing.T) {
	p := NewTOTPProvider("Phoenix")

	tests := []struct {
		name     string
		issuer   string
		username string
	}{
		{"both set", "Phoenix", "alice"},
		{"empty issuer falls back to constructor", "", "bob"},
		{"long username", "Acme Corp", "very.long.email+tag@example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			secret, urlStr, err := p.GenerateSecret(tc.issuer, tc.username)
			if err != nil {
				t.Fatalf("GenerateSecret returned error: %v", err)
			}
			if secret == "" {
				t.Errorf("GenerateSecret returned empty secret")
			}
			if urlStr == "" {
				t.Errorf("GenerateSecret returned empty URL")
			}
			m := otpauthURL.FindStringSubmatch(urlStr)
			if m == nil {
				t.Fatalf("GenerateSecret URL does not match otpauth scheme: %q", urlStr)
			}
			// Label must be "<issuer>:<username>" per the otpauth spec.
			// pquerna/otp percent-encodes the label, so we decode before
			// comparing.
			decodedLabel, err := url.PathUnescape(m[1])
			if err != nil {
				t.Fatalf("decoding label: %v", err)
			}
			issuer := tc.issuer
			if issuer == "" {
				issuer = "Phoenix"
			}
			wantLabel := issuer + ":" + tc.username
			if decodedLabel != wantLabel {
				t.Errorf("label = %q, want %q", decodedLabel, wantLabel)
			}
			// Secret in the URL must equal the returned secret.
			if !strings.Contains(m[2], "secret="+secret) {
				t.Errorf("URL does not embed the returned secret: %q", m[2])
			}
			// Issuer claim must be present in the query string.
			// Query values are percent-encoded, so decode before
			// comparing.
			decodedIssuer, err := queryValue(m[2], "issuer")
			if err != nil {
				t.Fatalf("decoding issuer query value: %v", err)
			}
			if decodedIssuer != issuer {
				t.Errorf("issuer query = %q, want %q", decodedIssuer, issuer)
			}
		})
	}
}

// TestTOTPProvider_GenerateSecret_RequiresIssuer asserts the adapter
// rejects a missing issuer even when the constructor had a default —
// callers that want a default should rely on the constructor fallback,
// not on passing an empty string. This is the current behavior of
// pquerna/otp and we lock it in.
func TestTOTPProvider_GenerateSecret_RequiresUsername(t *testing.T) {
	p := NewTOTPProvider("Phoenix")
	_, _, err := p.GenerateSecret("Phoenix", "")
	if err == nil {
		t.Errorf("GenerateSecret with empty username returned no error")
	}
}

// TestTOTPProvider_VerifyToken_Valid asserts that a freshly generated
// token validates against its secret.
func TestTOTPProvider_VerifyToken_Valid(t *testing.T) {
	p := NewTOTPProvider("Phoenix")
	secret, _, err := p.GenerateSecret("Phoenix", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	// Use the library directly to produce a code for the current
	// period, then verify the adapter accepts it.
	code, err := totp.GenerateCodeCustom(secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom: %v", err)
	}
	if !p.VerifyToken(secret, code) {
		t.Errorf("VerifyToken rejected a freshly generated valid code")
	}
}

// TestTOTPProvider_VerifyToken_Invalid asserts that bad input is
// rejected. Each case is a sub-test so a failure points at the exact
// input that fooled the verifier.
func TestTOTPProvider_VerifyToken_Invalid(t *testing.T) {
	p := NewTOTPProvider("Phoenix")
	secret, _, err := p.GenerateSecret("Phoenix", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	// Generate a code for the previous period so it is definitely
	// outside the ±1 skew window allowed by totp.Validate.
	oldCode, err := totp.GenerateCodeCustom(secret, time.Now().Add(-5*time.Minute).UTC(), totp.ValidateOpts{
		Period:    30,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom (old): %v", err)
	}

	tests := []struct {
		name   string
		secret string
		token  string
	}{
		{"empty secret", "", "123456"},
		{"empty token", secret, ""},
		{"both empty", "", ""},
		{"wrong secret", "JBSWY3DPEHPK3PXP", "123456"},
		{"stale token", secret, oldCode},
		{"non-numeric token", secret, "abcdef"},
		{"short numeric token", secret, "12345"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if p.VerifyToken(tc.secret, tc.token) {
				t.Errorf("VerifyToken(%q, %q) returned true; want false", tc.secret, tc.token)
			}
		})
	}
}
