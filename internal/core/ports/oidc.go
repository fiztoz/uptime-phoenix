package ports

import (
	"context"
)

// OIDCClaims is the verified subset of an ID token the service needs after
// the Authorization Code exchange. It is deliberately free of library types.
type OIDCClaims struct {
	// Issuer is the token's iss claim (must match configured issuer).
	Issuer string
	// Subject is the immutable sub claim — the external identity key together with Issuer.
	Subject string
	// Email is the optional email claim (may be empty).
	Email string
	// EmailVerified is true only when the IdP asserts email_verified.
	EmailVerified bool
	// PreferredUsername is the optional preferred_username claim.
	PreferredUsername string
	// Groups are the IdP group memberships from the configured groups claim.
	Groups []string
}

// OIDCAuthenticator performs OIDC discovery, Authorization Code exchange, and
// ID-token validation. Implementations live in adapters/auth; the service layer
// never imports OIDC libraries.
//
// State, nonce, and the PKCE code_verifier are minted and verified by the
// service (HMAC-signed blob, no server-side store) so multi-pod API
// deployments can complete callbacks. Authorization Code + PKCE S256 is always
// used; IdPs must support S256.
type OIDCAuthenticator interface {
	// Enabled reports whether OIDC is configured and ready.
	Enabled() bool
	// Issuer returns the configured issuer URL.
	Issuer() string
	// AuthCodeURL builds the IdP authorization redirect URL for the given
	// opaque state string (already signed by the service), OIDC nonce, and
	// PKCE S256 code_challenge (BASE64URL(SHA256(verifier)) without padding).
	AuthCodeURL(state, nonce, codeChallenge string) string
	// Exchange validates the authorization code with the PKCE code_verifier,
	// verifies the ID token (including nonce), and returns the claims Phoenix needs.
	Exchange(ctx context.Context, code, nonce, codeVerifier string) (*OIDCClaims, error)
	// EndSessionURL returns an optional IdP logout URL for the given
	// post-logout redirect. Empty string means the IdP does not advertise one
	// or logout redirect is not configured.
	EndSessionURL(postLogoutRedirectURL string) string
}
