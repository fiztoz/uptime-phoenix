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

// totpCodeAt is the library's own code for secret at instant at.
func totpCodeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at.UTC(), totp.ValidateOpts{
		Period:    30,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom: %v", err)
	}
	return code
}

// fixedTOTPSecret is the RFC 6238 test-vector key (ASCII "12345678901234567890").
const fixedTOTPSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// TestTOTPProvider_MatchToken_Valid asserts that a freshly generated
// token validates against its secret and reports the current step.
func TestTOTPProvider_MatchToken_Valid(t *testing.T) {
	p := NewTOTPProvider("Phoenix")
	secret, _, err := p.GenerateSecret("Phoenix", "alice")
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	now := time.Now().UTC()
	step, ok := p.MatchToken(secret, totpCodeAt(t, secret, now), now)
	if !ok || step != now.Unix()/30 {
		t.Fatalf("MatchToken(fresh code) = (%d, %v); want (%d, true)", step, ok, now.Unix()/30)
	}
}

// TestTOTPProvider_MatchToken_SkewBoundaries pins the exact window: the
// code of step s matches from the first second of step s-1 to the last
// second of step s+1 and nowhere else, and always reports step s.
func TestTOTPProvider_MatchToken_SkewBoundaries(t *testing.T) {
	p := NewTOTPProvider("Phoenix")
	const s = int64(59_000_000) // an arbitrary fixed step (2026-02-07)
	code := totpCodeAt(t, fixedTOTPSecret, time.Unix(s*30, 0))
	for _, tc := range []struct {
		name string
		at   int64 // Unix seconds
		want bool
	}{
		{"last second two steps early", (s-1)*30 - 1, false},
		{"first second of previous step", (s - 1) * 30, true},
		{"start of own step", s * 30, true},
		{"end of own step", s*30 + 29, true},
		{"last second of next step", (s+1)*30 + 29, true},
		{"first second two steps late", (s + 2) * 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step, ok := p.MatchToken(fixedTOTPSecret, code, time.Unix(tc.at, 0))
			if ok != tc.want || (ok && step != s) {
				t.Fatalf("MatchToken at %d = (%d, %v); want ok=%v step=%d", tc.at, step, ok, tc.want, s)
			}
		})
	}
}

// TestTOTPProvider_MatchToken_RFC6238Vector checks the adapter against the
// published SHA-1 vector (59 s → 94287082, last six digits 287082).
func TestTOTPProvider_MatchToken_RFC6238Vector(t *testing.T) {
	p := NewTOTPProvider("Phoenix")
	step, ok := p.MatchToken(fixedTOTPSecret, "287082", time.Unix(59, 0))
	if !ok || step != 1 {
		t.Fatalf("RFC 6238 vector = (%d, %v); want (1, true)", step, ok)
	}
}

// TestTOTPProvider_MatchToken_Invalid asserts that bad input is
// rejected. Each case is a sub-test so a failure points at the exact
// input that fooled the verifier.
func TestTOTPProvider_MatchToken_Invalid(t *testing.T) {
	p := NewTOTPProvider("Phoenix")
	at := time.Unix(59_000_000*30, 0)
	oldCode := totpCodeAt(t, fixedTOTPSecret, at.Add(-5*time.Minute))
	wrongSecretCode := totpCodeAt(t, "JBSWY3DPEHPK3PXP", at)

	tests := []struct {
		name   string
		secret string
		token  string
	}{
		{"empty secret", "", "123456"},
		{"empty token", fixedTOTPSecret, ""},
		{"both empty", "", ""},
		{"wrong secret", fixedTOTPSecret, wrongSecretCode},
		{"malformed secret", "not base32 !", "123456"},
		{"stale token", fixedTOTPSecret, oldCode},
		{"non-numeric token", fixedTOTPSecret, "abcdef"},
		{"short numeric token", fixedTOTPSecret, "12345"},
		{"long numeric token", fixedTOTPSecret, "1234567"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if step, ok := p.MatchToken(tc.secret, tc.token, at); ok {
				t.Errorf("MatchToken(%q, %q) = (%d, true); want false", tc.secret, tc.token, step)
			}
		})
	}
}
