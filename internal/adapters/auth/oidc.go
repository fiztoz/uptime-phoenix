package auth

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// OIDCConfig configures the OIDC relying party.
//
// All fields except Scopes and GroupsClaim have no useful defaults — OIDC is
// off until Issuer, ClientID, ClientSecret, and RedirectURL are set.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// Scopes defaults to openid profile email when empty.
	Scopes []string
	// GroupsClaim is the ID-token / userinfo claim that holds group membership.
	// Default "groups".
	GroupsClaim string
	// ExtraAuthParams are optional query parameters added to the authorize URL
	// (e.g. resource, audience) for IdPs that need them.
	ExtraAuthParams map[string]string
}

// OIDCProvider implements ports.OIDCAuthenticator using go-oidc discovery and
// Authorization Code + PKCE S256. IdPs must support code_challenge_method=S256.
//
// Construction performs discovery against the issuer. The provider is safe for
// concurrent use after NewOIDCProvider returns.
type OIDCProvider struct {
	cfg           OIDCConfig
	provider      *oidc.Provider
	verifier      *oidc.IDTokenVerifier
	oauth2        oauth2.Config
	endSessionURL string
	groupsClaim   string

	mu sync.RWMutex
}

// NewOIDCProvider discovers the issuer and builds an OIDC authenticator.
// Returns an error if discovery fails or required config is missing.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCProvider, error) {
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	cfg.ClientSecret = strings.TrimSpace(cfg.ClientSecret)
	cfg.RedirectURL = strings.TrimSpace(cfg.RedirectURL)
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, fmt.Errorf("oidc: issuer, client_id, client_secret, and redirect_url are required")
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	groupsClaim := strings.TrimSpace(cfg.GroupsClaim)
	if groupsClaim == "" {
		groupsClaim = "groups"
	}

	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery for %s: %w", cfg.Issuer, err)
	}

	// Capture optional end_session_endpoint from discovery document.
	var disc struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	_ = provider.Claims(&disc)

	oauth2Cfg := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       cfg.Scopes,
	}

	return &OIDCProvider{
		cfg:           cfg,
		provider:      provider,
		verifier:      provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth2:        oauth2Cfg,
		endSessionURL: strings.TrimSpace(disc.EndSessionEndpoint),
		groupsClaim:   groupsClaim,
	}, nil
}

// Enabled always returns true for a successfully constructed provider.
func (p *OIDCProvider) Enabled() bool { return p != nil && p.provider != nil }

// Issuer returns the configured issuer URL.
func (p *OIDCProvider) Issuer() string {
	if p == nil {
		return ""
	}
	return p.cfg.Issuer
}

// AuthCodeURL builds the IdP authorization redirect URL.
//
// nonce is passed as an OIDC auth request parameter so the ID token must
// echo it; state is the opaque CSRF blob minted by the service.
// codeChallenge is the PKCE S256 challenge (BASE64URL(SHA256(verifier))).
// IdPs must support code_challenge_method=S256.
func (p *OIDCProvider) AuthCodeURL(state, nonce, codeChallenge string) string {
	opts := []oauth2.AuthCodeOption{
		oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}
	for k, v := range p.cfg.ExtraAuthParams {
		if k == "" {
			continue
		}
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	return p.oauth2.AuthCodeURL(state, opts...)
}

// Exchange redeems the authorization code (with PKCE code_verifier) and
// verifies the ID token (including nonce).
func (p *OIDCProvider) Exchange(ctx context.Context, code, nonce, codeVerifier string) (*ports.OIDCClaims, error) {
	if code == "" {
		return nil, fmt.Errorf("oidc: empty authorization code")
	}
	if codeVerifier == "" {
		return nil, fmt.Errorf("oidc: empty PKCE code_verifier")
	}
	token, err := p.oauth2.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return nil, fmt.Errorf("oidc: code exchange: %w", err)
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, fmt.Errorf("oidc: response missing id_token")
	}
	idToken, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify id_token: %w", err)
	}
	if idToken.Nonce != nonce {
		return nil, fmt.Errorf("oidc: nonce mismatch")
	}

	var payload map[string]any
	if err := idToken.Claims(&payload); err != nil {
		return nil, fmt.Errorf("oidc: parse claims: %w", err)
	}

	claims := &ports.OIDCClaims{
		Issuer:  idToken.Issuer,
		Subject: idToken.Subject,
	}
	if email, _ := payload["email"].(string); email != "" {
		claims.Email = strings.TrimSpace(email)
	}
	switch v := payload["email_verified"].(type) {
	case bool:
		claims.EmailVerified = v
	case string:
		claims.EmailVerified = strings.EqualFold(v, "true")
	}
	if preferred, _ := payload["preferred_username"].(string); preferred != "" {
		claims.PreferredUsername = strings.TrimSpace(preferred)
	}
	claims.Groups = extractStringSlice(payload[p.groupsClaim])

	// Some IdPs put groups only in userinfo; fill when ID token omitted them.
	if len(claims.Groups) == 0 {
		if ui, err := p.provider.UserInfo(ctx, oauth2.StaticTokenSource(token)); err == nil {
			var uiClaims map[string]any
			if err := ui.Claims(&uiClaims); err == nil {
				if g := extractStringSlice(uiClaims[p.groupsClaim]); len(g) > 0 {
					claims.Groups = g
				}
				if claims.Email == "" {
					if email, _ := uiClaims["email"].(string); email != "" {
						claims.Email = strings.TrimSpace(email)
					}
				}
				if !claims.EmailVerified {
					switch v := uiClaims["email_verified"].(type) {
					case bool:
						claims.EmailVerified = v
					case string:
						claims.EmailVerified = strings.EqualFold(v, "true")
					}
				}
				if claims.PreferredUsername == "" {
					if preferred, _ := uiClaims["preferred_username"].(string); preferred != "" {
						claims.PreferredUsername = strings.TrimSpace(preferred)
					}
				}
			}
		}
	}

	return claims, nil
}

// EndSessionURL returns an IdP logout URL when discovery advertised one.
func (p *OIDCProvider) EndSessionURL(postLogoutRedirectURL string) string {
	p.mu.RLock()
	endpoint := p.endSessionURL
	clientID := p.cfg.ClientID
	p.mu.RUnlock()
	if endpoint == "" {
		return ""
	}
	// Build a minimal RP-initiated logout URL.
	// Spec: https://openid.net/specs/openid-connect-rpinitiated-1_0.html
	u := endpoint
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	u += sep + "client_id=" + url.QueryEscape(clientID)
	if postLogoutRedirectURL != "" {
		u += "&post_logout_redirect_uri=" + url.QueryEscape(postLogoutRedirectURL)
	}
	return u
}

func extractStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		out := make([]string, 0, len(t))
		for _, s := range t {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, _ := item.(string)
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		// Space- or comma-separated single claim (some IdPs).
		raw := strings.FieldsFunc(t, func(r rune) bool {
			return r == ',' || r == ' ' || r == ';'
		})
		out := make([]string, 0, len(raw))
		for _, s := range raw {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// Compile-time assertion.
var _ ports.OIDCAuthenticator = (*OIDCProvider)(nil)
