package domain

import "time"

// WebAuthnCredential represents a registered WebAuthn (passkey) credential
// belonging to a user. It is the persistence-side projection of the
// go-webauthn library's credential record: the library-specific fields are
// flattened to portable types so the domain layer stays free of adapter
// imports.
//
// CredentialID is the raw credential ID bytes (the authenticator's handle for
// the public key). PublicKey is the COSE-encoded public key. SignCount is the
// authenticator's signature counter, updated on every successful assertion to
// detect cloned authenticators. Transports records the transport hints the
// authenticator advertised (e.g. "internal", "usb", "nfc", "ble").
type WebAuthnCredential struct {
	ID           int64
	UserID       int64
	CredentialID []byte
	PublicKey    []byte
	SignCount    uint32
	Transports   []string
	// Flags is the authenticator's raw credential-flags byte. FlagsKnown is
	// false only for credentials created before flags were persisted; the
	// WebAuthn adapter can safely adopt those flags from the first verified
	// assertion and then persist them.
	Flags        byte
	FlagsKnown   bool
	CloneWarning bool
	Attachment   string
	Name         string // human-readable label chosen by the user
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}
