package repository

import (
	"encoding/base64"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// WebAuthnCredentialModel maps the webauthn_credentials table.
//
// CredentialID and PublicKey are raw bytes in the domain, but they are stored
// as base64url TEXT so the same schema works on both SQLite and MariaDB
// without engine-specific BLOB handling. Transports is a JSON array reusing
// the shared StringListField helper (same approach as APIKeyModel.Scopes).
type WebAuthnCredentialModel struct {
	bun.BaseModel `bun:"table:webauthn_credentials"`

	ID           int64           `bun:"id,pk,autoincrement"`
	UserID       int64           `bun:"user_id,notnull"`
	CredentialID string          `bun:"credential_id,notnull"` // base64url(raw credential id)
	PublicKey    string          `bun:"public_key,notnull"`    // base64url(COSE public key)
	SignCount    uint32          `bun:"sign_count,notnull,default:0"`
	Transports   StringListField `bun:"transports,notnull,default:'[]'"`
	Flags        *uint8          `bun:"flags"` // nil marks a pre-012 legacy credential
	CloneWarning bool            `bun:"clone_warning,notnull,default:false"`
	Attachment   string          `bun:"attachment,notnull,default:''"`
	Name         string          `bun:"name,notnull,default:''"`
	CreatedAt    time.Time       `bun:"created_at,notnull"`
	LastUsedAt   *time.Time      `bun:"last_used_at"`
}

// ToDomain converts a WebAuthnCredentialModel to a domain.WebAuthnCredential.
func (m *WebAuthnCredentialModel) ToDomain() *domain.WebAuthnCredential {
	credID, _ := base64.RawURLEncoding.DecodeString(m.CredentialID)
	pubKey, _ := base64.RawURLEncoding.DecodeString(m.PublicKey)
	transports := []string(m.Transports)
	if transports == nil {
		transports = []string{}
	}
	credential := &domain.WebAuthnCredential{
		ID:           m.ID,
		UserID:       m.UserID,
		CredentialID: credID,
		PublicKey:    pubKey,
		SignCount:    m.SignCount,
		Transports:   transports,
		CloneWarning: m.CloneWarning,
		Attachment:   m.Attachment,
		Name:         m.Name,
		CreatedAt:    m.CreatedAt,
		LastUsedAt:   m.LastUsedAt,
	}
	if m.Flags != nil {
		credential.Flags = *m.Flags
		credential.FlagsKnown = true
	}
	return credential
}

// WebAuthnCredentialModelFromDomain converts a domain.WebAuthnCredential to a
// WebAuthnCredentialModel.
func WebAuthnCredentialModelFromDomain(c *domain.WebAuthnCredential) *WebAuthnCredentialModel {
	transports := StringListField(c.Transports)
	if transports == nil {
		transports = StringListField{}
	}
	var flags *uint8
	if c.FlagsKnown {
		value := c.Flags
		flags = &value
	}
	return &WebAuthnCredentialModel{
		ID:           c.ID,
		UserID:       c.UserID,
		CredentialID: base64.RawURLEncoding.EncodeToString(c.CredentialID),
		PublicKey:    base64.RawURLEncoding.EncodeToString(c.PublicKey),
		SignCount:    c.SignCount,
		Transports:   transports,
		Flags:        flags,
		CloneWarning: c.CloneWarning,
		Attachment:   c.Attachment,
		Name:         c.Name,
		CreatedAt:    c.CreatedAt,
		LastUsedAt:   c.LastUsedAt,
	}
}
