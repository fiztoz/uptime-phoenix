package auth

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// WebAuthnProvider implements ports.WebAuthnAuthenticator using the
// go-webauthn library.
//
// A single provider is safe for concurrent use: the underlying *webauthn.WebAuthn
// is read-only after construction, and every ceremony allocates its own state.
// The provider is stateless with respect to users and challenges — the service
// layer owns persistence of credentials and of the short-lived ceremony session
// (see ports.WebAuthnAuthenticator). To keep the port free of library types the
// begin/finish calls exchange opaque JSON payloads, which this adapter
// (un)marshals at the boundary.
type WebAuthnProvider struct {
	wa *webauthn.WebAuthn
}

// WebAuthnConfig configures the relying party. Defaults are filled in by
// NewWebAuthnProvider when fields are empty.
type WebAuthnConfig struct {
	RPID          string   // relying party ID — the registrable domain, e.g. "localhost"
	RPDisplayName string   // human-readable RP name shown by authenticators
	RPOrigins     []string // allowed origins, e.g. ["http://localhost:3000"]
}

// NewWebAuthnProvider creates a WebAuthn relying party from the given config.
//
// Sensible defaults are applied for local development when fields are empty:
// RPID defaults to "localhost", the display name to "Phoenix", and the origins
// to the common dev origins. In production all three should be set explicitly
// via env (PHX_WEBAUTHN_* / see bootstrap config) so assertions are scoped to
// the real deployment domain.
func NewWebAuthnProvider(cfg WebAuthnConfig) (*WebAuthnProvider, error) {
	if cfg.RPID == "" {
		cfg.RPID = "localhost"
	}
	if cfg.RPDisplayName == "" {
		cfg.RPDisplayName = "Phoenix"
	}
	if len(cfg.RPOrigins) == 0 {
		cfg.RPOrigins = []string{"http://localhost:3000", "http://localhost:5173"}
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: cfg.RPDisplayName,
		RPOrigins:     cfg.RPOrigins,
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: new relying party: %w", err)
	}
	return &WebAuthnProvider{wa: wa}, nil
}

// webAuthnUser adapts a ports.WebAuthnUser to the webauthn.User interface.
//
// The user handle (WebAuthnID) is derived deterministically from the numeric
// user ID so it is stable across logins; it is never displayed.
type webAuthnUser struct {
	id      int64
	name    string
	display string
	creds   []webauthn.Credential
}

func newWebAuthnUser(u ports.WebAuthnUser) *webAuthnUser {
	display := u.DisplayName
	if display == "" {
		display = u.Username
	}
	creds := make([]webauthn.Credential, 0, len(u.Credentials))
	for _, c := range u.Credentials {
		creds = append(creds, domainCredentialToLibrary(c))
	}
	return &webAuthnUser{id: u.ID, name: u.Username, display: display, creds: creds}
}

func (u *webAuthnUser) WebAuthnID() []byte {
	// 8-byte big-endian encoding of the numeric ID — opaque and stable.
	id := u.id
	return []byte{
		byte(id >> 56), byte(id >> 48), byte(id >> 40), byte(id >> 32),
		byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id),
	}
}

func (u *webAuthnUser) WebAuthnName() string                       { return u.name }
func (u *webAuthnUser) WebAuthnDisplayName() string                { return u.display }
func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// BeginRegistration starts a passkey registration ceremony.
func (p *WebAuthnProvider) BeginRegistration(user ports.WebAuthnUser) ([]byte, []byte, error) {
	wu := newWebAuthnUser(user)
	// Exclude already-registered credentials so a passkey cannot be enrolled
	// twice on the same account.
	options, session, err := p.wa.BeginRegistration(wu, webauthn.WithExclusions(credentialDescriptors(wu.creds)))
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: begin registration: %w", err)
	}
	return marshalCeremony(options, session)
}

// FinishRegistration validates the authenticator response and returns the new
// credential to persist.
func (p *WebAuthnProvider) FinishRegistration(user ports.WebAuthnUser, session []byte, response []byte) (*domain.WebAuthnCredential, error) {
	var sd webauthn.SessionData
	if err := json.Unmarshal(session, &sd); err != nil {
		return nil, fmt.Errorf("webauthn: decode session: %w", err)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return nil, fmt.Errorf("webauthn: parse registration response: %w", err)
	}
	cred, err := p.wa.CreateCredential(newWebAuthnUser(user), sd, parsed)
	if err != nil {
		return nil, fmt.Errorf("webauthn: create credential: %w", err)
	}
	return libraryCredentialToDomain(user.ID, cred), nil
}

// BeginLogin starts a passkey assertion ceremony for a known user.
func (p *WebAuthnProvider) BeginLogin(user ports.WebAuthnUser) ([]byte, []byte, error) {
	wu := newWebAuthnUser(user)
	options, session, err := p.wa.BeginLogin(wu)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: begin login: %w", err)
	}
	return marshalCeremony(options, session)
}

// FinishLogin validates the assertion and returns the matched credential with
// its updated SignCount.
func (p *WebAuthnProvider) FinishLogin(user ports.WebAuthnUser, session []byte, response []byte) (*domain.WebAuthnCredential, error) {
	var sd webauthn.SessionData
	if err := json.Unmarshal(session, &sd); err != nil {
		return nil, fmt.Errorf("webauthn: decode session: %w", err)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return nil, fmt.Errorf("webauthn: parse login response: %w", err)
	}
	// Credentials registered before Phoenix persisted authenticator flags have
	// no trustworthy stored BackupEligible value. Adopt it from this assertion
	// before validation; the assertion (including these flags) is signature-
	// verified below, and the resulting known flags are persisted on success.
	adoptLegacyCredentialFlags(&user, parsed.RawID, parsed.Response.AuthenticatorData.Flags)
	cred, err := p.wa.ValidateLogin(newWebAuthnUser(user), sd, parsed)
	if err != nil {
		return nil, fmt.Errorf("webauthn: validate login: %w", err)
	}
	return libraryCredentialToDomain(user.ID, cred), nil
}

func adoptLegacyCredentialFlags(user *ports.WebAuthnUser, credentialID []byte, flags protocol.AuthenticatorFlags) {
	for _, credential := range user.Credentials {
		if credential.FlagsKnown || !bytes.Equal(credential.CredentialID, credentialID) {
			continue
		}
		credential.Flags = byte(flags)
		credential.FlagsKnown = true
		return
	}
}

// --- translation helpers -------------------------------------------------

// marshalCeremony serializes the creation/assertion options and the session
// data to the opaque JSON byte slices the port exchanges.
func marshalCeremony(options any, session *webauthn.SessionData) ([]byte, []byte, error) {
	optJSON, err := json.Marshal(options)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: marshal options: %w", err)
	}
	sessJSON, err := json.Marshal(session)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: marshal session: %w", err)
	}
	return optJSON, sessJSON, nil
}

func credentialDescriptors(creds []webauthn.Credential) []protocol.CredentialDescriptor {
	out := make([]protocol.CredentialDescriptor, 0, len(creds))
	for i := range creds {
		out = append(out, creds[i].Descriptor())
	}
	return out
}

// domainCredentialToLibrary converts a stored domain credential into the
// library's runtime credential so it can participate in a ceremony.
func domainCredentialToLibrary(c *domain.WebAuthnCredential) webauthn.Credential {
	transports := make([]protocol.AuthenticatorTransport, 0, len(c.Transports))
	for _, t := range c.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(t))
	}
	credential := webauthn.Credential{
		ID:        c.CredentialID,
		PublicKey: c.PublicKey,
		Transport: transports,
		Authenticator: webauthn.Authenticator{
			SignCount:    c.SignCount,
			CloneWarning: c.CloneWarning,
			Attachment:   protocol.AuthenticatorAttachment(c.Attachment),
		},
	}
	if c.FlagsKnown {
		credential.Flags = webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(c.Flags))
	}
	return credential
}

// libraryCredentialToDomain projects a library credential to the domain type
// the repository persists.
func libraryCredentialToDomain(userID int64, c *webauthn.Credential) *domain.WebAuthnCredential {
	transports := make([]string, 0, len(c.Transport))
	for _, t := range c.Transport {
		transports = append(transports, string(t))
	}
	return &domain.WebAuthnCredential{
		UserID:       userID,
		CredentialID: c.ID,
		PublicKey:    c.PublicKey,
		SignCount:    c.Authenticator.SignCount,
		Transports:   transports,
		Flags:        byte(c.Flags.ProtocolValue()),
		FlagsKnown:   true,
		CloneWarning: c.Authenticator.CloneWarning,
		Attachment:   string(c.Authenticator.Attachment),
	}
}

// Compile-time guard.
var _ ports.WebAuthnAuthenticator = (*WebAuthnProvider)(nil)
