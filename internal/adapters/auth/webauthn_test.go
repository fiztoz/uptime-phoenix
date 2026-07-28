package auth

import (
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func newTestProvider(t *testing.T) *WebAuthnProvider {
	t.Helper()
	p, err := NewWebAuthnProvider(WebAuthnConfig{
		RPID:          "localhost",
		RPDisplayName: "Phoenix Test",
		RPOrigins:     []string{"http://localhost:3000"},
	})
	if err != nil {
		t.Fatalf("NewWebAuthnProvider: %v", err)
	}
	return p
}

func TestNewWebAuthnProvider_Defaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     WebAuthnConfig
		wantErr bool
	}{
		{
			name: "explicit config",
			cfg:  WebAuthnConfig{RPID: "example.com", RPDisplayName: "Phoenix", RPOrigins: []string{"https://example.com"}},
		},
		{
			name: "empty config falls back to localhost defaults",
			cfg:  WebAuthnConfig{},
		},
		{
			name: "rp id without matching origin still constructs",
			cfg:  WebAuthnConfig{RPID: "example.com", RPOrigins: []string{"https://example.com"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := NewWebAuthnProvider(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p == nil || p.wa == nil {
				t.Fatal("provider not constructed")
			}
		})
	}
}

func TestWebAuthnProvider_BeginRegistration(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t)

	options, session, err := p.BeginRegistration(ports.WebAuthnUser{ID: 7, Username: "alice", DisplayName: "Alice"})
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}

	// Options must be valid JSON containing a publicKey challenge.
	var opt struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RP        struct {
				ID string `json:"id"`
			} `json:"rp"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &opt); err != nil {
		t.Fatalf("options not valid JSON: %v", err)
	}
	if opt.PublicKey.Challenge == "" {
		t.Error("options missing challenge")
	}
	if opt.PublicKey.RP.ID != "localhost" {
		t.Errorf("rp id = %q; want localhost", opt.PublicKey.RP.ID)
	}

	// Session must round-trip into the library's SessionData.
	var sd webauthn.SessionData
	if err := json.Unmarshal(session, &sd); err != nil {
		t.Fatalf("session not valid SessionData JSON: %v", err)
	}
	if sd.Challenge == "" {
		t.Error("session missing challenge")
	}
}

func TestWebAuthnProvider_BeginLogin(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t)

	// A login ceremony for a user with one stored credential should succeed.
	user := ports.WebAuthnUser{
		ID:       3,
		Username: "bob",
		Credentials: []*domain.WebAuthnCredential{
			{
				CredentialID: []byte{1, 2, 3, 4},
				PublicKey:    []byte{5, 6, 7, 8},
				Transports:   []string{"internal"},
			},
		},
	}
	options, session, err := p.BeginLogin(user)
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	if len(options) == 0 || len(session) == 0 {
		t.Fatal("BeginLogin returned empty payloads")
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(session, &sd); err != nil {
		t.Fatalf("session decode: %v", err)
	}
	if len(sd.AllowedCredentialIDs) != 1 {
		t.Errorf("allowed credentials = %d; want 1", len(sd.AllowedCredentialIDs))
	}
}

func TestWebAuthnProvider_BeginLogin_NoCredentials(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t)
	// go-webauthn rejects a login ceremony for a user with no credentials.
	_, _, err := p.BeginLogin(ports.WebAuthnUser{ID: 1, Username: "nocreds"})
	if err == nil {
		t.Fatal("expected error for user with no credentials")
	}
}

func TestWebAuthnProvider_FinishRegistration_BadInputs(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t)
	user := ports.WebAuthnUser{ID: 1, Username: "alice"}

	tests := []struct {
		name     string
		session  []byte
		response []byte
	}{
		{name: "malformed session", session: []byte("{not json"), response: []byte("{}")},
		{name: "malformed response", session: mustValidSession(t, p), response: []byte("{not json")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := p.FinishRegistration(user, tc.session, tc.response); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestWebAuthnProvider_FinishLogin_BadSession(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t)
	user := ports.WebAuthnUser{ID: 1, Username: "alice"}
	if _, err := p.FinishLogin(user, []byte("{bad"), []byte("{}")); err == nil {
		t.Fatal("expected error for malformed session")
	}
}

// TestCredentialTranslation verifies the domain<->library credential mapping
// is lossless for the fields Phoenix persists.
func TestCredentialTranslation(t *testing.T) {
	t.Parallel()
	dc := &domain.WebAuthnCredential{
		CredentialID: []byte{10, 20, 30},
		PublicKey:    []byte{40, 50, 60},
		SignCount:    42,
		Transports:   []string{"usb", "nfc"},
		Flags:        byte(protocol.FlagUserPresent | protocol.FlagUserVerified | protocol.FlagBackupEligible | protocol.FlagBackupState),
		FlagsKnown:   true,
		CloneWarning: true,
		Attachment:   string(protocol.Platform),
	}
	lib := domainCredentialToLibrary(dc)
	if string(lib.ID) != string(dc.CredentialID) {
		t.Error("credential ID mismatch")
	}
	if lib.Authenticator.SignCount != 42 {
		t.Errorf("sign count = %d; want 42", lib.Authenticator.SignCount)
	}
	if len(lib.Transport) != 2 {
		t.Errorf("transports = %d; want 2", len(lib.Transport))
	}
	if !lib.Flags.BackupEligible || !lib.Flags.BackupState {
		t.Errorf("backup flags lost in domain-to-library translation: %+v", lib.Flags)
	}
	if !lib.Authenticator.CloneWarning || lib.Authenticator.Attachment != protocol.Platform {
		t.Errorf("authenticator state lost in domain-to-library translation: %+v", lib.Authenticator)
	}

	back := libraryCredentialToDomain(99, &lib)
	if back.UserID != 99 {
		t.Errorf("user id = %d; want 99", back.UserID)
	}
	if string(back.PublicKey) != string(dc.PublicKey) {
		t.Error("public key mismatch")
	}
	if back.SignCount != 42 {
		t.Errorf("sign count = %d; want 42", back.SignCount)
	}
	if len(back.Transports) != 2 {
		t.Errorf("transports = %d; want 2", len(back.Transports))
	}
	if !back.FlagsKnown || back.Flags != dc.Flags {
		t.Errorf("flags = %#x (known=%v); want %#x (known=true)", back.Flags, back.FlagsKnown, dc.Flags)
	}
	if !back.CloneWarning || back.Attachment != dc.Attachment {
		t.Errorf("authenticator state = clone:%v attachment:%q; want clone:true attachment:%q", back.CloneWarning, back.Attachment, dc.Attachment)
	}
}

func TestAdoptLegacyCredentialFlags(t *testing.T) {
	legacy := &domain.WebAuthnCredential{CredentialID: []byte{1, 2, 3}}
	known := &domain.WebAuthnCredential{
		CredentialID: []byte{4, 5, 6},
		Flags:        byte(protocol.FlagUserPresent),
		FlagsKnown:   true,
	}
	user := ports.WebAuthnUser{Credentials: []*domain.WebAuthnCredential{legacy, known}}
	assertionFlags := protocol.FlagUserPresent | protocol.FlagUserVerified | protocol.FlagBackupEligible

	adoptLegacyCredentialFlags(&user, legacy.CredentialID, assertionFlags)
	if !legacy.FlagsKnown || legacy.Flags != byte(assertionFlags) {
		t.Fatalf("legacy flags = %#x (known=%v); want %#x (known=true)", legacy.Flags, legacy.FlagsKnown, assertionFlags)
	}

	adoptLegacyCredentialFlags(&user, known.CredentialID, protocol.FlagBackupEligible)
	if known.Flags != byte(protocol.FlagUserPresent) {
		t.Fatalf("known credential flags were overwritten: %#x", known.Flags)
	}
}

// mustValidSession returns a real, JSON-encoded SessionData from a begin
// ceremony so "malformed response" cases get past the session-decode step.
func mustValidSession(t *testing.T, p *WebAuthnProvider) []byte {
	t.Helper()
	_, session, err := p.BeginRegistration(ports.WebAuthnUser{ID: 1, Username: "alice"})
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	return session
}
