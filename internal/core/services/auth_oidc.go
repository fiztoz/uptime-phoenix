package services

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// OIDC-related sentinel errors. Handlers map these to HTTP status codes.
var (
	// ErrOIDCNotConfigured is returned when OIDC endpoints are hit but OIDC is off.
	ErrOIDCNotConfigured = &AuthServiceError{Code: "oidc_not_configured", Message: "OIDC SSO is not enabled on this server"}
	// ErrOIDCInvalidState is returned when the callback state is missing, forged, or expired.
	ErrOIDCInvalidState = &AuthServiceError{Code: "oidc_invalid_state", Message: "OIDC login state is invalid or expired"}
	// ErrOIDCAccessDenied is returned when the user is not in OIDC_ALLOWED_GROUPS.
	ErrOIDCAccessDenied = &AuthServiceError{Code: "oidc_access_denied", Message: "your account is not permitted to access Phoenix"}
	// ErrOIDCNoAccount is returned when JIT is disabled and no linked user exists.
	ErrOIDCNoAccount = &AuthServiceError{Code: "oidc_no_account", Message: "no Phoenix account is linked to this identity"}
	// ErrOIDCExchange is returned when the code exchange or ID-token verification fails.
	ErrOIDCExchange = &AuthServiceError{Code: "oidc_exchange_failed", Message: "OIDC authentication failed"}
)

// OIDCPolicy is the operator-controlled provisioning and group-mapping policy.
// It is pure configuration — no I/O.
type OIDCPolicy struct {
	// JITEnabled creates a Phoenix user on first successful OIDC login.
	JITEnabled bool
	// LinkByEmail links an unlinked existing user when the IdP asserts a
	// verified email that matches the username (case-insensitive).
	LinkByEmail bool
	// AllowedGroups, when non-empty, requires membership in at least one group.
	AllowedGroups []string
	// AdminGroups grant is_admin on every successful login while a member.
	AdminGroups []string
	// Cap*Groups map IdP groups onto the capability flags
	// (notifications, maintenance, monitor/group creation, group metadata,
	// extension visibility).
	CapNotificationsGroups          []string
	CapMaintenanceGroups            []string
	CapCreateMonitorsGroups         []string
	CapCreateTopLevelMonitorsGroups []string
	CapCreateGroupsGroups           []string
	CapEditGroupMetadataGroups      []string
	CapViewExtensionsGroups         []string
	// GrantMap maps IdP group names onto scoped view grants. Keys are IdP
	// group names; values describe the Phoenix resource.
	GrantMap []OIDCGrantMapping
	// StateTTL is how long a signed login state remains valid. Default 10m.
	StateTTL time.Duration
	// StateSecret is the HMAC key for signing state (typically JWT_SECRET).
	StateSecret string
	// FrontendRedirect is the absolute or relative SPA origin used after callback
	// (PUBLIC_URL, may be empty → relative /login).
	FrontendRedirect string
}

// OIDCGrantMapping is one IdP-group → Phoenix scoped-grant rule.
type OIDCGrantMapping struct {
	// IDPGroup is the group name from the IdP groups claim.
	IDPGroup string
	// ResourceType is "group" (monitor group) or "monitor".
	ResourceType string
	// ResourceID is the Phoenix monitor or monitor-group id.
	ResourceID int64
	// IncludeDescendants applies only to group grants (default true).
	IncludeDescendants bool
}

// oidcStatePayload is the HMAC-signed blob carried in the OAuth state parameter.
// No server-side store is required, so any API pod can complete the callback.
// CodeVerifier is the PKCE verifier (RFC 7636) — carried in the signed blob so
// multi-pod API deployments can finish the exchange without sticky sessions.
type oidcStatePayload struct {
	Nonce        string `json:"n"`
	Exp          int64  `json:"e"`
	CodeVerifier string `json:"v"`
}

// WithOIDC wires the OIDC authenticator, identity repository, permission
// repository (for scoped grant sync), and operator policy.
func WithOIDC(
	provider ports.OIDCAuthenticator,
	identities ports.OIDCIdentityRepository,
	perms ports.UserPermissionRepository,
	policy OIDCPolicy,
) AuthServiceOption {
	return func(s *AuthService) {
		s.oidc = provider
		s.oidcIdentities = identities
		s.oidcPerms = perms
		if policy.StateTTL <= 0 {
			policy.StateTTL = 10 * time.Minute
		}
		s.oidcPolicy = policy
	}
}

// OIDCEnabled reports whether SSO is configured and ready.
func (s *AuthService) OIDCEnabled() bool {
	return s.oidc != nil && s.oidc.Enabled()
}

// BeginOIDCLogin mints a signed state (nonce + PKCE code_verifier) and returns
// the IdP authorize URL with code_challenge (S256).
func (s *AuthService) BeginOIDCLogin(_ context.Context) (authURL, state string, err error) {
	if !s.OIDCEnabled() {
		return "", "", fmt.Errorf("%w", ErrOIDCNotConfigured)
	}
	nonce, err := randomToken(32)
	if err != nil {
		return "", "", fmt.Errorf("auth service: oidc begin: nonce: %w", err)
	}
	verifier, err := generateCodeVerifier()
	if err != nil {
		return "", "", fmt.Errorf("auth service: oidc begin: pkce verifier: %w", err)
	}
	challenge := s256CodeChallenge(verifier)
	state, err = s.signOIDCState(oidcStatePayload{
		Nonce:        nonce,
		Exp:          s.clock.Now().Add(s.oidcPolicy.StateTTL).Unix(),
		CodeVerifier: verifier,
	})
	if err != nil {
		return "", "", fmt.Errorf("auth service: oidc begin: state: %w", err)
	}
	return s.oidc.AuthCodeURL(state, nonce, challenge), state, nil
}

// CompleteOIDCLogin validates state, exchanges the code, links or provisions
// the user, syncs group-derived permissions, and returns a session JWT.
func (s *AuthService) CompleteOIDCLogin(ctx context.Context, code, state string) (token string, user *domain.User, err error) {
	if !s.OIDCEnabled() {
		return "", nil, fmt.Errorf("%w", ErrOIDCNotConfigured)
	}
	payload, err := s.verifyOIDCState(state)
	if err != nil {
		return "", nil, fmt.Errorf("%w", ErrOIDCInvalidState)
	}
	claims, err := s.oidc.Exchange(ctx, code, payload.Nonce, payload.CodeVerifier)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrOIDCExchange, err)
	}
	if claims == nil || claims.Subject == "" || claims.Issuer == "" {
		return "", nil, fmt.Errorf("%w: missing issuer or subject", ErrOIDCExchange)
	}

	if !s.groupsAllowed(claims.Groups) {
		return "", nil, fmt.Errorf("%w", ErrOIDCAccessDenied)
	}

	user, identity, err := s.resolveOIDCUser(ctx, claims)
	if err != nil {
		return "", nil, err
	}

	if !user.Active {
		return "", nil, fmt.Errorf("%w", ErrUserInactive)
	}

	// Sync admin/capabilities and mapped scoped grants from IdP groups.
	if err := s.syncOIDCPermissions(ctx, user, claims.Groups); err != nil {
		return "", nil, fmt.Errorf("auth service: oidc sync permissions: %w", err)
	}
	// Re-read after flag updates so the returned view is current.
	user, err = s.users.GetByID(ctx, user.ID)
	if err != nil {
		return "", nil, fmt.Errorf("auth service: oidc reload user: %w", err)
	}

	now := s.clock.Now()
	if err := s.oidcIdentities.TouchLogin(ctx, identity.ID, claims.Email, now); err != nil {
		return "", nil, fmt.Errorf("auth service: oidc touch login: %w", err)
	}

	token, err = s.auth.IssueSession(ctx, user.ID)
	if err != nil {
		return "", nil, fmt.Errorf("auth service: oidc issue session: %w", err)
	}
	return token, user, nil
}

// OIDCLogoutURL returns an optional IdP end-session URL.
//
// postLogoutRedirect is operator/browser-supplied and must not be an open
// redirect: only same-origin absolute URLs matching FrontendRedirect, or
// host-less relative paths, are forwarded to the IdP.
func (s *AuthService) OIDCLogoutURL(postLogoutRedirect string) string {
	if !s.OIDCEnabled() {
		return ""
	}
	return s.oidc.EndSessionURL(s.sanitizePostLogoutRedirect(postLogoutRedirect))
}

// sanitizePostLogoutRedirect returns a safe post_logout_redirect_uri or "".
// Rejects scheme-relative and absolute URLs that do not match FrontendRedirect.
func (s *AuthService) sanitizePostLogoutRedirect(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Some browsers treat backslashes as path separators; normalize first so
	// url.Parse cannot be tricked into treating "\\evil.com" as a host.
	raw = strings.ReplaceAll(raw, "\\", "/")

	target, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	// Relative (no scheme, no host) — allow only absolute paths under our app.
	if target.Scheme == "" && target.Host == "" {
		if !strings.HasPrefix(target.Path, "/") || strings.HasPrefix(target.Path, "//") {
			return ""
		}
		return target.String()
	}
	// Absolute URL must match the configured SPA origin (PUBLIC_URL).
	baseRaw := strings.TrimSpace(s.oidcPolicy.FrontendRedirect)
	if baseRaw == "" {
		return ""
	}
	base, err := url.Parse(baseRaw)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return ""
	}
	if !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return ""
	}
	return target.String()
}

// OIDCFrontendRedirect builds the post-callback SPA URL carrying the session token.
//
// The token is placed in the URL fragment (#…) so it is not sent to the server
// on subsequent navigations and is less likely to appear in access logs than a
// query parameter. The SPA reads the fragment once and clears it.
func (s *AuthService) OIDCFrontendRedirect(token string) string {
	base := strings.TrimRight(strings.TrimSpace(s.oidcPolicy.FrontendRedirect), "/")
	path := "/login#oidc_token=" + urlQueryEscape(token)
	if base == "" {
		return path
	}
	return base + path
}

// OIDCFrontendErrorRedirect builds the post-callback SPA URL carrying an error code.
// Errors use a query parameter so they survive without JS and can be bookmarked
// for support; they do not contain credentials.
func (s *AuthService) OIDCFrontendErrorRedirect(code string) string {
	base := strings.TrimRight(strings.TrimSpace(s.oidcPolicy.FrontendRedirect), "/")
	path := "/login?oidc_error=" + urlQueryEscape(code)
	if base == "" {
		return path
	}
	return base + path
}

// resolveOIDCUser finds or creates the Phoenix user for the verified claims.
func (s *AuthService) resolveOIDCUser(ctx context.Context, claims *ports.OIDCClaims) (*domain.User, *domain.OIDCIdentity, error) {
	existing, err := s.oidcIdentities.GetByIssuerSubject(ctx, claims.Issuer, claims.Subject)
	if err == nil && existing != nil {
		user, getErr := s.users.GetByID(ctx, existing.UserID)
		if getErr != nil {
			return nil, nil, fmt.Errorf("auth service: oidc get linked user: %w", getErr)
		}
		return user, existing, nil
	}
	if err != nil && !isNotFound(err) {
		return nil, nil, fmt.Errorf("auth service: oidc lookup identity: %w", err)
	}

	// Optional verified-email link to an existing unlinked account.
	if s.oidcPolicy.LinkByEmail && claims.EmailVerified && claims.Email != "" {
		if user, linkErr := s.tryLinkByVerifiedEmail(ctx, claims); linkErr == nil && user != nil {
			identity := &domain.OIDCIdentity{
				UserID:  user.ID,
				Issuer:  claims.Issuer,
				Subject: claims.Subject,
				Email:   claims.Email,
			}
			if createErr := s.oidcIdentities.Create(ctx, identity); createErr != nil {
				return nil, nil, fmt.Errorf("auth service: oidc link identity: %w", createErr)
			}
			return user, identity, nil
		}
	}

	if !s.oidcPolicy.JITEnabled {
		return nil, nil, fmt.Errorf("%w", ErrOIDCNoAccount)
	}

	username := s.chooseOIDCUsername(ctx, claims)
	// Random unusable password — local login cannot succeed until an admin sets one.
	password, err := randomToken(32)
	if err != nil {
		return nil, nil, fmt.Errorf("auth service: oidc jit password: %w", err)
	}
	user, err := s.CreateUser(ctx, username, password, true, false, "UTC", UserCapabilities{})
	if err != nil {
		return nil, nil, fmt.Errorf("auth service: oidc jit create: %w", err)
	}
	identity := &domain.OIDCIdentity{
		UserID:  user.ID,
		Issuer:  claims.Issuer,
		Subject: claims.Subject,
		Email:   claims.Email,
	}
	if err := s.oidcIdentities.Create(ctx, identity); err != nil {
		return nil, nil, fmt.Errorf("auth service: oidc jit link: %w", err)
	}
	return user, identity, nil
}

// tryLinkByVerifiedEmail finds an existing user whose username equals the
// verified email (case-insensitive). Returns nil user when no match.
func (s *AuthService) tryLinkByVerifiedEmail(ctx context.Context, claims *ports.OIDCClaims) (*domain.User, error) {
	email := strings.TrimSpace(claims.Email)
	if email == "" || !claims.EmailVerified {
		return nil, fmt.Errorf("%w", ErrOIDCNoAccount)
	}
	// Try exact username match first (common when local users were created with email usernames).
	user, err := s.users.GetByUsername(ctx, email)
	if err == nil {
		// Refuse to link if this user already has an identity for this issuer.
		links, listErr := s.oidcIdentities.ListByUser(ctx, user.ID)
		if listErr != nil {
			return nil, listErr
		}
		for _, l := range links {
			if l.Issuer == claims.Issuer {
				return nil, fmt.Errorf("%w", ErrOIDCNoAccount)
			}
		}
		return user, nil
	}
	if !isNotFound(err) {
		return nil, err
	}
	return nil, fmt.Errorf("%w", ErrOIDCNoAccount)
}

func (s *AuthService) chooseOIDCUsername(ctx context.Context, claims *ports.OIDCClaims) string {
	candidates := []string{}
	if claims.PreferredUsername != "" {
		candidates = append(candidates, sanitizeUsername(claims.PreferredUsername))
	}
	if claims.EmailVerified && claims.Email != "" {
		local := claims.Email
		if i := strings.Index(local, "@"); i > 0 {
			local = local[:i]
		}
		candidates = append(candidates, sanitizeUsername(local))
	}
	candidates = append(candidates, sanitizeUsername("oidc_"+shortHash(claims.Subject)))

	suffix := shortHash(claims.Subject + claims.Issuer)
	for _, base := range candidates {
		if base == "" {
			continue
		}
		if _, err := s.users.GetByUsername(ctx, base); isNotFound(err) {
			return base
		}
		// Prefer keeping a human-readable stem when the bare name is taken.
		alt := sanitizeUsername(base + "_" + suffix)
		if alt != "" {
			if _, err := s.users.GetByUsername(ctx, alt); isNotFound(err) {
				return alt
			}
		}
	}
	return sanitizeUsername("oidc_" + suffix)
}

var usernameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._@+-]+`)

func sanitizeUsername(s string) string {
	s = strings.TrimSpace(s)
	s = usernameSanitizer.ReplaceAllString(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

func (s *AuthService) groupsAllowed(groups []string) bool {
	if len(s.oidcPolicy.AllowedGroups) == 0 {
		return true
	}
	return anyGroupMatch(groups, s.oidcPolicy.AllowedGroups)
}

func anyGroupMatch(have, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, g := range have {
		set[strings.ToLower(strings.TrimSpace(g))] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[strings.ToLower(strings.TrimSpace(w))]; ok {
			return true
		}
	}
	return false
}

// syncOIDCPermissions updates is_admin, capability flags, and OIDC-mapped grants.
func (s *AuthService) syncOIDCPermissions(ctx context.Context, user *domain.User, groups []string) error {
	isAdmin := anyGroupMatch(groups, s.oidcPolicy.AdminGroups)
	caps := UserCapabilities{
		CanManageNotifications:    anyGroupMatch(groups, s.oidcPolicy.CapNotificationsGroups),
		CanManageMaintenance:      anyGroupMatch(groups, s.oidcPolicy.CapMaintenanceGroups),
		CanCreateMonitors:         anyGroupMatch(groups, s.oidcPolicy.CapCreateMonitorsGroups),
		CanCreateTopLevelMonitors: anyGroupMatch(groups, s.oidcPolicy.CapCreateTopLevelMonitorsGroups),
		CanCreateGroups:           anyGroupMatch(groups, s.oidcPolicy.CapCreateGroupsGroups),
		CanEditGroupMetadata:      anyGroupMatch(groups, s.oidcPolicy.CapEditGroupMetadataGroups),
		CanViewExtensions:         anyGroupMatch(groups, s.oidcPolicy.CapViewExtensionsGroups),
	}

	changed := user.IsAdmin != isAdmin ||
		user.CanManageNotifications != caps.CanManageNotifications ||
		user.CanManageMaintenance != caps.CanManageMaintenance ||
		user.CanCreateMonitors != caps.CanCreateMonitors ||
		user.CanCreateTopLevelMonitors != caps.CanCreateTopLevelMonitors ||
		user.CanCreateGroups != caps.CanCreateGroups ||
		user.CanEditGroupMetadata != caps.CanEditGroupMetadata ||
		user.CanViewExtensions != caps.CanViewExtensions

	if changed {
		user.IsAdmin = isAdmin
		user.CanManageNotifications = caps.CanManageNotifications
		user.CanManageMaintenance = caps.CanManageMaintenance
		user.CanCreateMonitors = caps.CanCreateMonitors
		user.CanCreateTopLevelMonitors = caps.CanCreateTopLevelMonitors
		user.CanCreateGroups = caps.CanCreateGroups
		user.CanEditGroupMetadata = caps.CanEditGroupMetadata
		user.CanViewExtensions = caps.CanViewExtensions
		if err := s.users.Update(ctx, user); err != nil {
			return fmt.Errorf("update user flags: %w", err)
		}
	}

	if s.oidcPerms == nil || len(s.oidcPolicy.GrantMap) == 0 {
		return nil
	}
	return s.syncOIDCGrants(ctx, user.ID, groups)
}

// syncOIDCGrants applies only the grants listed in GrantMap. Admin-UI grants
// outside the map are never touched.
func (s *AuthService) syncOIDCGrants(ctx context.Context, userID int64, groups []string) error {
	memberOf := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		memberOf[strings.ToLower(strings.TrimSpace(g))] = struct{}{}
	}

	for _, m := range s.oidcPolicy.GrantMap {
		key := strings.ToLower(strings.TrimSpace(m.IDPGroup))
		if key == "" || m.ResourceID <= 0 {
			continue
		}
		_, inGroup := memberOf[key]
		switch strings.ToLower(m.ResourceType) {
		case "group", "monitor_group", "folder":
			if inGroup {
				include := m.IncludeDescendants
				// Default true when the mapping does not set a preference —
				// zero value of bool is false, so we treat unset as true by
				// convention in ParseOIDCGrantMap (always sets explicitly).
				p := &domain.UserPermission{
					UserID:             userID,
					GroupID:            &m.ResourceID,
					IncludeDescendants: include,
				}
				if err := s.oidcPerms.Grant(ctx, p); err != nil {
					return fmt.Errorf("grant group %d: %w", m.ResourceID, err)
				}
			} else {
				if err := s.oidcPerms.RevokeGroup(ctx, userID, m.ResourceID); err != nil {
					return fmt.Errorf("revoke group %d: %w", m.ResourceID, err)
				}
			}
		case "monitor":
			if inGroup {
				p := &domain.UserPermission{
					UserID:    userID,
					MonitorID: &m.ResourceID,
				}
				if err := s.oidcPerms.Grant(ctx, p); err != nil {
					return fmt.Errorf("grant monitor %d: %w", m.ResourceID, err)
				}
			} else {
				if err := s.oidcPerms.RevokeMonitor(ctx, userID, m.ResourceID); err != nil {
					return fmt.Errorf("revoke monitor %d: %w", m.ResourceID, err)
				}
			}
		}
	}
	return nil
}

// --- Signed state helpers -------------------------------------------------

func (s *AuthService) signOIDCState(p oidcStatePayload) (string, error) {
	secret := s.oidcPolicy.StateSecret
	if secret == "" {
		return "", errors.New("oidc state secret is empty")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

func (s *AuthService) verifyOIDCState(state string) (*oidcStatePayload, error) {
	if state == "" {
		return nil, errors.New("empty state")
	}
	parts := strings.Split(state, ".")
	if len(parts) != 2 {
		return nil, errors.New("malformed state")
	}
	payloadB64, sigB64 := parts[0], parts[1]
	secret := s.oidcPolicy.StateSecret
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payloadB64))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sigB64)) {
		return nil, errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, err
	}
	var p oidcStatePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Nonce == "" || p.Exp == 0 || p.CodeVerifier == "" {
		return nil, errors.New("incomplete state")
	}
	if s.clock.Now().Unix() > p.Exp {
		return nil, errors.New("state expired")
	}
	return &p, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceUnreserved is the RFC 7636 unreserved charset for code_verifier:
// ALPHA / DIGIT / "-" / "." / "_" / "~". base64url uses a subset of this.
var pkceUnreserved = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// generateCodeVerifier returns a cryptographically random PKCE code_verifier.
// Uses 32 random octets → 43-character base64url string (RFC 7636 recommended).
func generateCodeVerifier() (string, error) {
	// 32 octets base64url-encoded without padding → 43 characters.
	return randomToken(32)
}

// s256CodeChallenge returns BASE64URL(SHA256(verifier)) without padding (S256).
func s256CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}

// ParseOIDCGrantMap parses "idp-group:group:5,idp-team:monitor:12" mappings.
// Optional fourth field "shallow" forces IncludeDescendants=false for group grants.
func ParseOIDCGrantMap(raw string) ([]OIDCGrantMapping, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]OIDCGrantMapping, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		if len(fields) < 3 {
			return nil, fmt.Errorf("oidc grant map entry %q: want idp-group:type:id", part)
		}
		id, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("oidc grant map entry %q: invalid id", part)
		}
		m := OIDCGrantMapping{
			IDPGroup:           strings.TrimSpace(fields[0]),
			ResourceType:       strings.TrimSpace(fields[1]),
			ResourceID:         id,
			IncludeDescendants: true,
		}
		if len(fields) >= 4 && strings.EqualFold(fields[3], "shallow") {
			m.IncludeDescendants = false
		}
		out = append(out, m)
	}
	return out, nil
}

// SplitCSV trims and splits a comma-separated env value into a string slice.
func SplitCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
