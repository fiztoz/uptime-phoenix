package services_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// fakeOIDC implements ports.OIDCAuthenticator for service tests.
type fakeOIDC struct {
	enabled bool
	issuer  string
	// exchange returns these claims (or err) when Exchange is called.
	claims  *ports.OIDCClaims
	exchErr error
	// lastAuth records AuthCodeURL args.
	lastState     string
	lastNonce     string
	lastChallenge string
	// lastExchange records Exchange args.
	lastCode          string
	lastExchangeNonce string
	lastVerifier      string
}

func (f *fakeOIDC) Enabled() bool { return f.enabled }
func (f *fakeOIDC) Issuer() string {
	if f.issuer != "" {
		return f.issuer
	}
	return "https://idp.example.com"
}
func (f *fakeOIDC) AuthCodeURL(state, nonce, codeChallenge string) string {
	f.lastState = state
	f.lastNonce = nonce
	f.lastChallenge = codeChallenge
	return "https://idp.example.com/auth?state=" + state + "&nonce=" + nonce + "&code_challenge=" + codeChallenge
}
func (f *fakeOIDC) Exchange(_ context.Context, code, nonce, codeVerifier string) (*ports.OIDCClaims, error) {
	f.lastCode = code
	f.lastExchangeNonce = nonce
	f.lastVerifier = codeVerifier
	if f.exchErr != nil {
		return nil, f.exchErr
	}
	if f.claims == nil {
		return nil, errors.New("no claims")
	}
	// Simulate adapter nonce / PKCE checks.
	if nonce == "" {
		return nil, errors.New("empty nonce")
	}
	if codeVerifier == "" {
		return nil, errors.New("empty code_verifier")
	}
	c := *f.claims
	if c.Issuer == "" {
		c.Issuer = f.Issuer()
	}
	return &c, nil
}
func (f *fakeOIDC) EndSessionURL(post string) string {
	if post == "" {
		return ""
	}
	return "https://idp.example.com/logout?post=" + post
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func newOIDCAuthService(t *testing.T, oidc ports.OIDCAuthenticator, policy services.OIDCPolicy) (
	*services.AuthService,
	*memory.UserRepo,
	*memory.OIDCIdentityRepo,
	*memory.UserPermissionRepo,
) {
	t.Helper()
	users := memory.NewUserRepo()
	apiKeys := memory.NewAPIKeyRepo()
	// Minimal authenticator: reuse real password hasher path via a tiny stub.
	authn := &stubAuthenticator{}
	identities := memory.NewOIDCIdentityRepo()
	perms := memory.NewUserPermissionRepo()
	if policy.StateSecret == "" {
		policy.StateSecret = "test-state-secret"
	}
	if policy.StateTTL == 0 {
		policy.StateTTL = 10 * time.Minute
	}
	svc := services.NewAuthService(
		users, apiKeys, authn, &stubTwoFactor{},
		services.WithClock(fixedClock{t: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)}),
		services.WithOIDC(oidc, identities, perms, policy),
	)
	return svc, users, identities, perms
}

type stubAuthenticator struct{}

func (s *stubAuthenticator) Login(context.Context, string, string) (string, error) {
	return "session-token", nil
}
func (s *stubAuthenticator) VerifyToken(context.Context, string) (int64, error) { return 1, nil }
func (s *stubAuthenticator) HashPassword(password string) (string, error) {
	return "hash:" + password, nil
}
func (s *stubAuthenticator) VerifyPassword(hashed, password string) error {
	if hashed == "hash:"+password {
		return nil
	}
	return errors.New("mismatch")
}
func (s *stubAuthenticator) IssueSession(context.Context, int64) (string, error) {
	return "session-token", nil
}
func (s *stubAuthenticator) IssuePending2FATicket(context.Context, int64) (string, error) {
	return "ticket", nil
}
func (s *stubAuthenticator) VerifyPending2FATicket(context.Context, string) (int64, error) {
	return 1, nil
}

type stubTwoFactor struct{}

func (s *stubTwoFactor) GenerateSecret(string, string) (string, string, error) {
	return "secret", "otpauth://x", nil
}
func (s *stubTwoFactor) VerifyToken(string, string) bool { return true }

func TestOIDCState_RoundTripAndExpiry(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})

	url, _, err := svc.BeginOIDCLogin(context.Background())
	if err != nil {
		t.Fatalf("BeginOIDCLogin: %v", err)
	}
	if !strings.Contains(url, "https://idp.example.com/auth") {
		t.Fatalf("auth URL = %q", url)
	}
	if oidc.lastState == "" || oidc.lastNonce == "" {
		t.Fatal("expected state and nonce to be recorded")
	}
	if oidc.lastChallenge == "" {
		t.Fatal("expected PKCE code_challenge to be recorded")
	}

	// Complete with matching claims — Exchange must receive the PKCE verifier.
	oidc.claims = &ports.OIDCClaims{
		Issuer:  "https://idp.example.com",
		Subject: "sub-1",
		Email:   "alice@example.com",
	}
	token, user, err := svc.CompleteOIDCLogin(context.Background(), "code", oidc.lastState)
	if err != nil {
		t.Fatalf("CompleteOIDCLogin: %v", err)
	}
	if token == "" || user == nil {
		t.Fatal("expected token and user")
	}
	if oidc.lastVerifier == "" {
		t.Fatal("expected code_verifier to be passed to Exchange")
	}
	if oidc.lastExchangeNonce != oidc.lastNonce {
		t.Fatalf("nonce mismatch: exchange=%q begin=%q", oidc.lastExchangeNonce, oidc.lastNonce)
	}

	// Corrupt signature.
	if _, _, err := svc.CompleteOIDCLogin(context.Background(), "code", "not.a.valid.state"); !errors.Is(err, services.ErrOIDCInvalidState) {
		t.Fatalf("bad state: got %v", err)
	}
}

func TestOIDCState_IncludesVerifierAndExpires(t *testing.T) {
	// Fixed clock so we can expire state by constructing a second service
	// whose Now is past the original Exp.
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	oidc := &fakeOIDC{enabled: true}
	users := memory.NewUserRepo()
	idents := memory.NewOIDCIdentityRepo()
	perms := memory.NewUserPermissionRepo()
	policy := services.OIDCPolicy{
		JITEnabled:  true,
		StateSecret: "test-state-secret",
		StateTTL:    time.Minute,
	}
	svc := services.NewAuthService(
		users, memory.NewAPIKeyRepo(), &stubAuthenticator{}, &stubTwoFactor{},
		services.WithClock(fixedClock{t: start}),
		services.WithOIDC(oidc, idents, perms, policy),
	)

	_, state, err := svc.BeginOIDCLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if oidc.lastChallenge == "" {
		t.Fatal("expected non-empty code_challenge on AuthCodeURL")
	}

	// Round-trip succeeds within TTL and carries verifier into Exchange.
	oidc.claims = &ports.OIDCClaims{Issuer: oidc.Issuer(), Subject: "exp-sub"}
	if _, _, err := svc.CompleteOIDCLogin(context.Background(), "c", state); err != nil {
		t.Fatalf("complete within TTL: %v", err)
	}
	if oidc.lastVerifier == "" {
		t.Fatal("verifier missing on Exchange")
	}

	// Same state after TTL → invalid.
	expired := services.NewAuthService(
		users, memory.NewAPIKeyRepo(), &stubAuthenticator{}, &stubTwoFactor{},
		services.WithClock(fixedClock{t: start.Add(2 * time.Minute)}),
		services.WithOIDC(oidc, idents, perms, policy),
	)
	if _, _, err := expired.CompleteOIDCLogin(context.Background(), "c", state); !errors.Is(err, services.ErrOIDCInvalidState) {
		t.Fatalf("expired state: got %v", err)
	}
}

func TestOIDCBegin_RecordsPKCEChallenge(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})
	url, _, err := svc.BeginOIDCLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if oidc.lastChallenge == "" {
		t.Fatal("AuthCodeURL must receive non-empty code_challenge")
	}
	if !strings.Contains(url, "code_challenge="+oidc.lastChallenge) {
		t.Fatalf("auth URL missing challenge: %q", url)
	}
	// Challenge is base64url of a SHA-256 digest → 43 chars, unpadded.
	if len(oidc.lastChallenge) != 43 {
		t.Fatalf("challenge length = %d, want 43", len(oidc.lastChallenge))
	}
}

func TestOIDCComplete_PassesVerifierToExchange(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})
	ctx := context.Background()
	_, state, err := svc.BeginOIDCLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oidc.claims = &ports.OIDCClaims{Issuer: oidc.Issuer(), Subject: "pkce-sub"}
	if _, _, err := svc.CompleteOIDCLogin(ctx, "auth-code", state); err != nil {
		t.Fatal(err)
	}
	if oidc.lastCode != "auth-code" {
		t.Fatalf("code = %q", oidc.lastCode)
	}
	if oidc.lastVerifier == "" {
		t.Fatal("expected non-empty code_verifier on Exchange")
	}
	if oidc.lastExchangeNonce != oidc.lastNonce {
		t.Fatalf("nonce not forwarded: begin=%q exchange=%q", oidc.lastNonce, oidc.lastExchangeNonce)
	}
	// Forged / missing state still rejected without calling Exchange successfully.
	oidc.lastVerifier = ""
	if _, _, err := svc.CompleteOIDCLogin(ctx, "auth-code", "forged.payload"); !errors.Is(err, services.ErrOIDCInvalidState) {
		t.Fatalf("forged: %v", err)
	}
	if oidc.lastVerifier != "" {
		t.Fatal("Exchange must not receive verifier for forged state")
	}
}

func TestOIDCComplete_RejectsBadState(t *testing.T) {
	oidc := &fakeOIDC{enabled: true, claims: &ports.OIDCClaims{Subject: "s", Issuer: "https://idp.example.com"}}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})
	if _, _, err := svc.CompleteOIDCLogin(context.Background(), "code", ""); !errors.Is(err, services.ErrOIDCInvalidState) {
		t.Fatalf("empty state: %v", err)
	}
	if _, _, err := svc.CompleteOIDCLogin(context.Background(), "code", "forged.payload"); !errors.Is(err, services.ErrOIDCInvalidState) {
		t.Fatalf("forged state: %v", err)
	}
}

func TestOIDCComplete_LinksByIssuerSubject(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, users, identities, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})
	ctx := context.Background()

	// First login creates + links.
	_, state, err := svc.BeginOIDCLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oidc.claims = &ports.OIDCClaims{Issuer: oidc.Issuer(), Subject: "stable-sub", PreferredUsername: "alice"}
	_, u1, err := svc.CompleteOIDCLogin(ctx, "code", state)
	if err != nil {
		t.Fatal(err)
	}

	// Second login reuses the same identity → same user id.
	_, state2, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{Issuer: oidc.Issuer(), Subject: "stable-sub", PreferredUsername: "alice-renamed"}
	_, u2, err := svc.CompleteOIDCLogin(ctx, "code", state2)
	if err != nil {
		t.Fatal(err)
	}
	if u1.ID != u2.ID {
		t.Fatalf("expected same user, got %d vs %d", u1.ID, u2.ID)
	}
	// Only one identity row.
	list, _ := identities.ListByUser(ctx, u1.ID)
	if len(list) != 1 {
		t.Fatalf("identities = %d", len(list))
	}
	// Username not changed on re-login (linking key is subject).
	got, _ := users.GetByID(ctx, u1.ID)
	if got.Username != "alice" {
		t.Fatalf("username mutated to %q", got.Username)
	}
}

func TestOIDCComplete_JITCreatesUser(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})
	ctx := context.Background()
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:            oidc.Issuer(),
		Subject:           "jit-1",
		PreferredUsername: "bob",
	}
	_, user, err := svc.CompleteOIDCLogin(ctx, "c", state)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "bob" {
		t.Fatalf("username = %q", user.Username)
	}
	if user.IsAdmin {
		t.Fatal("JIT user must not be admin by default")
	}
}

func TestOIDCComplete_JITDisabledRejectsUnknown(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: false})
	ctx := context.Background()
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{Issuer: oidc.Issuer(), Subject: "unknown"}
	if _, _, err := svc.CompleteOIDCLogin(ctx, "c", state); !errors.Is(err, services.ErrOIDCNoAccount) {
		t.Fatalf("got %v", err)
	}
}

func TestOIDCComplete_JITUsernameCollision(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, users, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})
	ctx := context.Background()
	// Pre-create local user named "carol".
	if _, err := svc.CreateUser(ctx, "carol", "password1", true, false, "UTC", services.UserCapabilities{}); err != nil {
		t.Fatal(err)
	}
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{Issuer: oidc.Issuer(), Subject: "sub-carol", PreferredUsername: "carol"}
	_, user, err := svc.CompleteOIDCLogin(ctx, "c", state)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username == "carol" {
		t.Fatal("expected collision to produce a different username")
	}
	if !strings.HasPrefix(user.Username, "carol") {
		t.Fatalf("username = %q", user.Username)
	}
	// Original local user still intact.
	local, _ := users.GetByUsername(ctx, "carol")
	if local.ID == user.ID {
		t.Fatal("collided with local user")
	}
}

func TestOIDCComplete_RejectsUnverifiedEmailLink(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{
		JITEnabled:  false,
		LinkByEmail: true,
	})
	ctx := context.Background()
	if _, err := svc.CreateUser(ctx, "dave@example.com", "password1", true, false, "UTC", services.UserCapabilities{}); err != nil {
		t.Fatal(err)
	}
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:        oidc.Issuer(),
		Subject:       "dave-sub",
		Email:         "dave@example.com",
		EmailVerified: false, // must not link
	}
	if _, _, err := svc.CompleteOIDCLogin(ctx, "c", state); !errors.Is(err, services.ErrOIDCNoAccount) {
		t.Fatalf("got %v, want no account", err)
	}
}

func TestOIDCComplete_LinkByVerifiedEmailWhenEnabled(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, identities, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{
		JITEnabled:  false,
		LinkByEmail: true,
	})
	ctx := context.Background()
	existing, err := svc.CreateUser(ctx, "eve@example.com", "password1", true, false, "UTC", services.UserCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:        oidc.Issuer(),
		Subject:       "eve-sub",
		Email:         "eve@example.com",
		EmailVerified: true,
	}
	_, user, err := svc.CompleteOIDCLogin(ctx, "c", state)
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != existing.ID {
		t.Fatalf("linked wrong user: %d vs %d", user.ID, existing.ID)
	}
	list, _ := identities.ListByUser(ctx, user.ID)
	if len(list) != 1 || list[0].Subject != "eve-sub" {
		t.Fatalf("identity not linked: %+v", list)
	}
}

func TestOIDCComplete_AllowedGroupsGate(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{
		JITEnabled:    true,
		AllowedGroups: []string{"phoenix-users"},
	})
	ctx := context.Background()
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:  oidc.Issuer(),
		Subject: "nogroup",
		Groups:  []string{"other"},
	}
	if _, _, err := svc.CompleteOIDCLogin(ctx, "c", state); !errors.Is(err, services.ErrOIDCAccessDenied) {
		t.Fatalf("got %v", err)
	}
	_, state2, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:  oidc.Issuer(),
		Subject: "ingrouped",
		Groups:  []string{"phoenix-users"},
	}
	if _, _, err := svc.CompleteOIDCLogin(ctx, "c", state2); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCComplete_SyncsAdminAndCapabilities(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, users, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{
		JITEnabled:               true,
		AdminGroups:              []string{"phoenix-admins"},
		CapNotificationsGroups:   []string{"notify-ops"},
		CapMaintenanceGroups:     []string{"maint-ops"},
		CapCreateMonitorsGroups:  []string{"mon-creators"},
		CapCreateGroupsGroups:    []string{"grp-creators"},
		CapViewAllMonitorsGroups: []string{"phoenix-viewers"},
	})
	ctx := context.Background()
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:  oidc.Issuer(),
		Subject: "admin-1",
		Groups:  []string{"phoenix-admins", "notify-ops"},
	}
	_, user, err := svc.CompleteOIDCLogin(ctx, "c", state)
	if err != nil {
		t.Fatal(err)
	}
	if !user.IsAdmin {
		t.Fatal("expected admin")
	}
	if !user.CanManageNotifications {
		t.Fatal("expected notifications capability")
	}

	// Lose admin group on next login.
	_, state2, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:  oidc.Issuer(),
		Subject: "admin-1",
		Groups:  []string{"notify-ops"},
	}
	_, user2, err := svc.CompleteOIDCLogin(ctx, "c", state2)
	if err != nil {
		t.Fatal(err)
	}
	if user2.IsAdmin {
		t.Fatal("admin should be revoked after group loss")
	}
	got, _ := users.GetByID(ctx, user.ID)
	if got.IsAdmin {
		t.Fatal("persisted is_admin still true")
	}
}

func TestOIDCComplete_SyncsViewAllMonitors(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, users, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{
		JITEnabled:               true,
		CapViewAllMonitorsGroups: []string{"phoenix-viewers"},
	})
	ctx := context.Background()
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:  oidc.Issuer(),
		Subject: "viewer-1",
		Groups:  []string{"phoenix-viewers"},
	}
	_, user, err := svc.CompleteOIDCLogin(ctx, "c", state)
	if err != nil {
		t.Fatal(err)
	}
	if user.IsAdmin {
		t.Fatal("view-all must not imply admin")
	}
	if !user.CanViewAllMonitors {
		t.Fatal("expected can_view_all_monitors from IdP group")
	}

	_, state2, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:  oidc.Issuer(),
		Subject: "viewer-1",
		Groups:  []string{"other"},
	}
	_, user2, err := svc.CompleteOIDCLogin(ctx, "c", state2)
	if err != nil {
		t.Fatal(err)
	}
	if user2.CanViewAllMonitors {
		t.Fatal("view-all should be revoked after group loss")
	}
	got, _ := users.GetByID(ctx, user.ID)
	if got.CanViewAllMonitors {
		t.Fatal("persisted can_view_all_monitors still true")
	}
}

func TestOIDCComplete_SyncsScopedGrantsOnlyFromMap(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	grantMap, err := services.ParseOIDCGrantMap("team-a:group:5,team-b:monitor:12")
	if err != nil {
		t.Fatal(err)
	}
	svc, _, _, perms := newOIDCAuthService(t, oidc, services.OIDCPolicy{
		JITEnabled: true,
		GrantMap:   grantMap,
	})
	ctx := context.Background()

	// Pre-seed an admin-owned grant outside the map (monitor 99) after first login.
	_, state, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:  oidc.Issuer(),
		Subject: "grantee",
		Groups:  []string{"team-a"},
	}
	_, user, err := svc.CompleteOIDCLogin(ctx, "c", state)
	if err != nil {
		t.Fatal(err)
	}
	mon99 := int64(99)
	if err := perms.Grant(ctx, &domain.UserPermission{UserID: user.ID, MonitorID: &mon99}); err != nil {
		t.Fatal(err)
	}

	// In team-a → group 5 granted.
	list, _ := perms.ListByUser(ctx, user.ID)
	var hasGroup5, hasMon99 bool
	for _, p := range list {
		if p.GroupID != nil && *p.GroupID == 5 {
			hasGroup5 = true
		}
		if p.MonitorID != nil && *p.MonitorID == 99 {
			hasMon99 = true
		}
	}
	if !hasGroup5 {
		t.Fatal("expected group 5 grant from map")
	}
	if !hasMon99 {
		t.Fatal("admin grant on monitor 99 must survive")
	}

	// Leave team-a, join team-b → revoke group 5, grant monitor 12; keep mon 99.
	_, state2, _ := svc.BeginOIDCLogin(ctx)
	oidc.claims = &ports.OIDCClaims{
		Issuer:  oidc.Issuer(),
		Subject: "grantee",
		Groups:  []string{"team-b"},
	}
	if _, _, err := svc.CompleteOIDCLogin(ctx, "c", state2); err != nil {
		t.Fatal(err)
	}
	list, _ = perms.ListByUser(ctx, user.ID)
	hasGroup5, hasMon12, hasMon99 := false, false, false
	for _, p := range list {
		if p.GroupID != nil && *p.GroupID == 5 {
			hasGroup5 = true
		}
		if p.MonitorID != nil && *p.MonitorID == 12 {
			hasMon12 = true
		}
		if p.MonitorID != nil && *p.MonitorID == 99 {
			hasMon99 = true
		}
	}
	if hasGroup5 {
		t.Fatal("group 5 should be revoked after leaving team-a")
	}
	if !hasMon12 {
		t.Fatal("expected monitor 12 grant")
	}
	if !hasMon99 {
		t.Fatal("admin grant on monitor 99 must still survive")
	}
}

func TestOIDCEnabled_LocalLoginStillWorks(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})
	ctx := context.Background()
	if _, err := svc.CreateUser(ctx, "localadmin", "password1", true, true, "UTC", services.UserCapabilities{}); err != nil {
		t.Fatal(err)
	}
	token, err := svc.Login(ctx, "localadmin", "password1")
	if err != nil {
		t.Fatalf("local login with OIDC enabled: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
}

func TestOIDCComplete_RejectsNonceMismatch(t *testing.T) {
	// Adapter-level nonce mismatch surfaces as ErrOIDCExchange.
	oidc := &fakeOIDC{
		enabled: true,
		exchErr: errors.New("oidc: nonce mismatch"),
	}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{JITEnabled: true})
	ctx := context.Background()
	_, state, _ := svc.BeginOIDCLogin(ctx)
	if _, _, err := svc.CompleteOIDCLogin(ctx, "c", state); !errors.Is(err, services.ErrOIDCExchange) {
		t.Fatalf("got %v", err)
	}
}

func TestParseOIDCGrantMap(t *testing.T) {
	m, err := services.ParseOIDCGrantMap("a:group:1,b:monitor:2:shallow")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatalf("len = %d", len(m))
	}
	if m[0].ResourceType != "group" || m[0].ResourceID != 1 || !m[0].IncludeDescendants {
		t.Fatalf("m0 = %+v", m[0])
	}
	if m[1].ResourceType != "monitor" || m[1].ResourceID != 2 {
		t.Fatalf("m1 = %+v", m[1])
	}
	if _, err := services.ParseOIDCGrantMap("bad"); err == nil {
		t.Fatal("expected error")
	}
}

func TestOIDCLogoutURL_RejectsOpenRedirect(t *testing.T) {
	oidc := &fakeOIDC{enabled: true}
	svc, _, _, _ := newOIDCAuthService(t, oidc, services.OIDCPolicy{
		FrontendRedirect: "https://app.example.com",
	})

	// Same-origin absolute is allowed.
	got := svc.OIDCLogoutURL("https://app.example.com/login")
	if !strings.Contains(got, "post=https://app.example.com/login") {
		t.Fatalf("same-origin rejected: %q", got)
	}

	// Relative path is allowed.
	got = svc.OIDCLogoutURL("/login")
	if !strings.Contains(got, "post=/login") {
		t.Fatalf("relative path rejected: %q", got)
	}

	// External host must be dropped (open-redirect guard).
	got = svc.OIDCLogoutURL("https://evil.example/phish")
	if strings.Contains(got, "evil.example") {
		t.Fatalf("external redirect leaked into logout URL: %q", got)
	}

	// Scheme-relative absolute-looking forms must not become external hosts.
	got = svc.OIDCLogoutURL("//evil.example/phish")
	if strings.Contains(got, "://evil.example") || strings.Contains(got, "post=//evil") {
		t.Fatalf("scheme-relative redirect leaked as host: %q", got)
	}
	// Backslashes are normalized to slashes and treated as a local path only
	// (not as a different host).
	got = svc.OIDCLogoutURL("\\evil.example/phish")
	if strings.Contains(got, "://evil.example") {
		t.Fatalf("backslash redirect treated as external host: %q", got)
	}
}
