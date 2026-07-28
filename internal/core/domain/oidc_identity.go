package domain

import "time"

// OIDCIdentity links a Phoenix user to an external OpenID Connect subject.
//
// The primary key for linking is the immutable pair (Issuer, Subject). Email is
// stored for display/audit only and must never be used as a linking key unless
// the operator explicitly enables verified-email linking and the claim is marked
// email_verified by the IdP — see docs/F5-S13-OIDC-CONTRACTS.md.
type OIDCIdentity struct {
	ID          int64
	UserID      int64
	Issuer      string
	Subject     string
	Email       string
	LastLoginAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
