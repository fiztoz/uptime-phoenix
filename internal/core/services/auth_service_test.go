package services

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- Test doubles --------------------------------------------------------

// fakeAuthenticator is a hand-rolled Authenticator for unit tests. It
// stores a per-user hashed password in an in-memory map and issues a
// trivially parseable token "<userID>" so VerifyToken can be checked
// without bringing in the JWT library.
type fakeAuthenticator struct {
	hashes    map[string]string // username -> bcrypt-shaped hash
	sessions  map[string]int64  // token -> userID
	tickets   map[string]int64  // ticket -> userID
	verifyErr error             // override for "expired" tests
}

func newFakeAuthenticator() *fakeAuthenticator {
	return &fakeAuthenticator{
		hashes:   map[string]string{},
		sessions: map[string]int64{},
		tickets:  map[string]int64{},
	}
}

//nolint:unparam // test helper receives the same username in current tests
func (a *fakeAuthenticator) seedHash(username, hash string) {
	a.hashes[username] = hash
}

func (a *fakeAuthenticator) Login(_ context.Context, username, password string) (string, error) {
	h, ok := a.hashes[username]
	if !ok {
		return "", errors.New("login: user not found")
	}
	if !strings.HasPrefix(h, "bcrypt$") {
		return "", errors.New("login: malformed hash")
	}
	if h != "bcrypt$"+password {
		return "", errors.New("login: invalid credentials")
	}
	token := "session:" + username
	a.sessions[token] = int64(len(a.sessions) + 1)
	return token, nil
}

func (a *fakeAuthenticator) VerifyToken(_ context.Context, token string) (int64, error) {
	if a.verifyErr != nil {
		return 0, a.verifyErr
	}
	id, ok := a.sessions[token]
	if !ok {
		return 0, errors.New("verify: unknown token")
	}
	return id, nil
}

func (a *fakeAuthenticator) HashPassword(pw string) (string, error) {
	return "bcrypt$" + pw, nil
}

func (a *fakeAuthenticator) VerifyPassword(hashed, pw string) error {
	if hashed != "bcrypt$"+pw {
		return errors.New("verify: mismatch")
	}
	return nil
}

func (a *fakeAuthenticator) IssueSession(_ context.Context, userID int64) (string, error) {
	tok := "session:u" + intToStr(userID)
	a.sessions[tok] = userID
	return tok, nil
}

func (a *fakeAuthenticator) IssuePending2FATicket(_ context.Context, userID int64) (string, error) {
	t := "ticket:u" + intToStr(userID) + ":" + intToStr(time.Now().UnixNano())
	a.tickets[t] = userID
	return t, nil
}

func (a *fakeAuthenticator) VerifyPending2FATicket(_ context.Context, ticket string) (int64, error) {
	id, ok := a.tickets[ticket]
	if !ok {
		return 0, errors.New("verify ticket: unknown")
	}
	return id, nil
}

func intToStr(i int64) string {
	// Avoid pulling strconv into the test file's imports for a single
	// helper; this is a closed, test-only path.
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// fakeTOTP is a deterministic TwoFactor: GenerateSecret always returns
// a fixed secret, VerifyToken checks against a code we set via SetValidCode.
type fakeTOTP struct {
	secret     string
	validCodes map[string]bool
}

func newFakeTOTP() *fakeTOTP {
	return &fakeTOTP{secret: "TESTSECRET", validCodes: map[string]bool{}}
}

func (t *fakeTOTP) GenerateSecret(issuer, username string) (string, string, error) {
	return t.secret, "otpauth://totp/" + issuer + ":" + username + "?secret=" + t.secret, nil
}

func (t *fakeTOTP) VerifyToken(secret, token string) bool {
	if secret != t.secret {
		return false
	}
	return t.validCodes[token]
}

func (t *fakeTOTP) SetValidCode(code string) { t.validCodes[code] = true }

// fakeClock is a controllable clock for time-sensitive tests.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// --- Tests ---------------------------------------------------------------

// newTestService wires a service with the in-memory repo and the fakes
// above. The repo is a thin shim around maps so each test gets a clean
// slate; it satisfies ports.UserRepository and ports.APIKeyRepository.
func newTestService(t *testing.T) (*AuthService, *fakeAuthenticator, *fakeTOTP) {
	t.Helper()
	// Reuse the in-memory adapter from adapters/repository/memory. We
	// import it as a separate type so the test does not require any
	// adapter wiring.
	repo := newInMemUserRepo()
	akRepo := newInMemAPIKeyRepo()
	auth := newFakeAuthenticator()
	totp := newFakeTOTP()
	svc := NewAuthService(repo, akRepo, auth, totp, WithClock(&fakeClock{now: time.Now().UTC()}))
	return svc, auth, totp
}

func TestAuthService_Register_Success(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	u, err := svc.Register(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if u.ID == 0 {
		t.Errorf("Register did not assign a user ID")
	}
	if !u.Active {
		t.Errorf("Register did not mark the user active")
	}
	if u.Username != "alice" {
		t.Errorf("Username = %q; want %q", u.Username, "alice")
	}
	if u.PasswordHash == "" || u.PasswordHash == "supersecret" {
		t.Errorf("PasswordHash should be a non-plaintext hash, got %q", u.PasswordHash)
	}
}

func TestAuthService_Register_Duplicate(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	_, err := svc.Register(ctx, "alice", "anothersecret")
	if !errors.Is(err, ErrUserExists) {
		t.Errorf("second Register returned %v; want ErrUserExists", err)
	}
}

func TestAuthService_Register_ShortPassword(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Register(context.Background(), "alice", "short")
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Register(short pw) returned %v; want domain.ErrValidation", err)
	}
}

func TestAuthService_Register_BlankUsername(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Register(context.Background(), "   ", "supersecret")
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Register(blank username) returned %v; want domain.ErrValidation", err)
	}
}

func TestAuthService_Login_And_VerifyToken(t *testing.T) {
	svc, auth, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The fake authenticator needs a hash entry to look up. The service
	// is supposed to have populated it via the user repo's stored hash
	// — but our Login path takes the username/password directly through
	// the authenticator. Mirror what the production flow does.
	auth.seedHash("alice", "bcrypt$supersecret")

	tok, err := svc.Login(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok == "" {
		t.Fatal("Login returned empty token")
	}
	uid, err := svc.VerifyToken(ctx, tok)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if uid == 0 {
		t.Errorf("VerifyToken returned zero user ID")
	}
}

func TestAuthService_Login_InvalidPassword(t *testing.T) {
	svc, auth, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	auth.seedHash("alice", "bcrypt$supersecret")

	_, err := svc.Login(ctx, "alice", "wrongpw")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login(wrong pw) = %v; want ErrInvalidCredentials", err)
	}
}

func TestAuthService_Login_UnknownUser(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Login(context.Background(), "nobody", "anything")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login(unknown user) = %v; want ErrInvalidCredentials", err)
	}
}

func TestAuthService_TOTP_FullFlow(t *testing.T) {
	svc, auth, totp := newTestService(t)
	ctx := context.Background()

	// Register and pre-populate the fake authenticator's hash.
	if _, err := svc.Register(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	auth.seedHash("alice", "bcrypt$supersecret")

	// Setup TOTP: this stores a secret on the user.
	secret, qr, err := svc.SetupTOTP(ctx, 1)
	if err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	if secret == "" || qr == "" {
		t.Errorf("SetupTOTP returned empty secret/QR")
	}
	// TOTP must not be enabled yet (the user has to confirm by
	// calling EnableTOTP with a valid code).
	u, err := svc.GetUser(ctx, 1)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.TOTPEnabled {
		t.Errorf("TOTPEnabled should be false right after SetupTOTP")
	}
	if u.TOTPSecret == "" {
		t.Errorf("TOTPSecret should be populated after SetupTOTP")
	}

	// Enable with a wrong code first; should fail.
	if enableErr := svc.EnableTOTP(ctx, 1, "000000"); !errors.Is(enableErr, ErrTOTPInvalid) {
		t.Errorf("EnableTOTP(bad code) = %v; want ErrTOTPInvalid", enableErr)
	}
	// Enable with the right code.
	totp.SetValidCode("123456")
	if enableErr := svc.EnableTOTP(ctx, 1, "123456"); enableErr != nil {
		t.Fatalf("EnableTOTP(good code): %v", enableErr)
	}
	u, _ = svc.GetUser(ctx, 1)
	if !u.TOTPEnabled {
		t.Errorf("TOTPEnabled should be true after successful EnableTOTP")
	}

	// Now Login without a TOTP token must return ErrTOTPRequired.
	_, err = svc.Login(ctx, "alice", "supersecret")
	if !errors.Is(err, ErrTOTPRequired) {
		t.Errorf("Login(2fa user, no token) = %v; want ErrTOTPRequired", err)
	}

	// Begin2FALogin: returns a ticket.
	ticket, user, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	if ticket == "" {
		t.Errorf("Begin2FALogin returned empty ticket")
	}
	if user == nil || user.ID != 1 {
		t.Errorf("Begin2FALogin returned unexpected user: %+v", user)
	}

	// Complete2FALogin with a bad code fails.
	_, _, err = svc.Complete2FALogin(ctx, ticket, "000000")
	if !errors.Is(err, ErrTOTPInvalid) {
		t.Errorf("Complete2FALogin(bad code) = %v; want ErrTOTPInvalid", err)
	}
	// With a good code, we get a session token.
	tok, user, err := svc.Complete2FALogin(ctx, ticket, "123456")
	if err != nil {
		t.Fatalf("Complete2FALogin(good code): %v", err)
	}
	if tok == "" {
		t.Errorf("Complete2FALogin returned empty token")
	}
	if user == nil || user.ID != 1 {
		t.Errorf("Complete2FALogin returned unexpected user: %+v", user)
	}

	// LoginWith2FA also works for the all-in-one case.
	tok, err = svc.LoginWith2FA(ctx, "alice", "supersecret", "123456")
	if err != nil {
		t.Fatalf("LoginWith2FA: %v", err)
	}
	if tok == "" {
		t.Errorf("LoginWith2FA returned empty token")
	}
}

func TestAuthService_DisableTOTP(t *testing.T) {
	svc, auth, totp := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	auth.seedHash("alice", "bcrypt$supersecret")

	// Disable on a fresh account: returns ErrTOTPNotEnabled.
	if err := svc.DisableTOTP(ctx, 1); !errors.Is(err, ErrTOTPNotEnabled) {
		t.Errorf("DisableTOTP(no secret) = %v; want ErrTOTPNotEnabled", err)
	}

	// Run the full enable flow.
	if _, _, err := svc.SetupTOTP(ctx, 1); err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	totp.SetValidCode("123456")
	if err := svc.EnableTOTP(ctx, 1, "123456"); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	if err := svc.DisableTOTP(ctx, 1); err != nil {
		t.Fatalf("DisableTOTP: %v", err)
	}
	u, _ := svc.GetUser(ctx, 1)
	if u.TOTPEnabled || u.TOTPSecret != "" {
		t.Errorf("DisableTOTP did not clear TOTP state: %+v", u)
	}
}

func TestAuthService_CreateAPIKey(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	u, err := svc.Register(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	key, ak, err := svc.CreateAPIKey(ctx, u.ID, "ci-token", []string{"read", "write"}, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if !strings.HasPrefix(key, "phx_") {
		t.Errorf("API key does not have the phx_ prefix: %q", key)
	}
	if ak.KeyHash == "" || ak.KeyHash == key {
		t.Errorf("API key hash should be set and not equal the plaintext: %q", ak.KeyHash)
	}
	if ak.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v; want nil when no expiry passed", ak.ExpiresAt)
	}
	// Two calls must yield different keys.
	key2, _, err := svc.CreateAPIKey(ctx, u.ID, "", nil, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey (second): %v", err)
	}
	if key == key2 {
		t.Errorf("CreateAPIKey returned the same key twice")
	}
}

func TestAuthService_CreateAPIKey_WithExpiry(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	u, err := svc.Register(ctx, "bob", "supersecret")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Local-zoned expiry must be stored as UTC.
	loc := time.FixedZone("UTC+7", 7*3600)
	expLocal := time.Date(2030, 6, 1, 12, 0, 0, 0, loc)
	_, ak, err := svc.CreateAPIKey(ctx, u.ID, "expiring", []string{"read"}, &expLocal)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if ak.ExpiresAt == nil {
		t.Fatal("ExpiresAt is nil; want set")
	}
	if ak.ExpiresAt.Location() != time.UTC {
		t.Errorf("ExpiresAt.Location() = %v; want UTC", ak.ExpiresAt.Location())
	}
	if !ak.ExpiresAt.Equal(expLocal.UTC()) {
		t.Errorf("ExpiresAt = %v; want %v", ak.ExpiresAt, expLocal.UTC())
	}
}

func TestAPIKeyExpired(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	now := time.Now().UTC()

	if APIKeyExpired(&domain.APIKey{ExpiresAt: nil}, now) {
		t.Error("nil ExpiresAt should not be expired")
	}
	if !APIKeyExpired(&domain.APIKey{ExpiresAt: &past}, now) {
		t.Error("past ExpiresAt should be expired")
	}
	if APIKeyExpired(&domain.APIKey{ExpiresAt: &future}, now) {
		t.Error("future ExpiresAt should not be expired")
	}
	// Exactly now counts as expired (not before).
	if !APIKeyExpired(&domain.APIKey{ExpiresAt: &now}, now) {
		t.Error("ExpiresAt == now should be expired")
	}
}

func TestAuthService_GetUser_NotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.GetUser(context.Background(), 999)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("GetUser(unknown) = %v; want ErrUserNotFound", err)
	}
}

// --- Tiny in-memory repo used only by tests in this file --------------

// We deliberately do not import adapters/repository/memory here to
// keep this test file self-contained. The test doubles below are just
// the subset of the port surface that AuthService exercises.

type inMemUserRepo struct {
	users        map[int64]*domain.User
	byName       map[string]int64
	next         int64
	getByIDCalls int
}

func newInMemUserRepo() *inMemUserRepo {
	return &inMemUserRepo{
		users:  map[int64]*domain.User{},
		byName: map[string]int64{},
	}
}

func (r *inMemUserRepo) Create(_ context.Context, u *domain.User) error {
	if _, exists := r.byName[u.Username]; exists {
		return ports.ErrConflict
	}
	r.next++
	u.ID = r.next
	now := time.Now().UTC()
	u.CreatedAt = now
	u.UpdatedAt = now
	r.users[u.ID] = u
	r.byName[u.Username] = u.ID
	return nil
}

func (r *inMemUserRepo) GetByID(_ context.Context, id int64) (*domain.User, error) {
	r.getByIDCalls++
	u, ok := r.users[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	clone := *u
	return &clone, nil
}

func (r *inMemUserRepo) GetByUsername(_ context.Context, username string) (*domain.User, error) {
	id, ok := r.byName[username]
	if !ok {
		return nil, ports.ErrNotFound
	}
	clone := *r.users[id]
	return &clone, nil
}

func (r *inMemUserRepo) Update(_ context.Context, u *domain.User) error {
	if _, ok := r.users[u.ID]; !ok {
		return ports.ErrNotFound
	}
	if r.users[u.ID].Username != u.Username {
		delete(r.byName, r.users[u.ID].Username)
		r.byName[u.Username] = u.ID
	}
	u.UpdatedAt = time.Now().UTC()
	r.users[u.ID] = u
	return nil
}

func (r *inMemUserRepo) Delete(_ context.Context, id int64) error {
	u, ok := r.users[id]
	if !ok {
		return ports.ErrNotFound
	}
	delete(r.byName, u.Username)
	delete(r.users, id)
	return nil
}

func (r *inMemUserRepo) Count(_ context.Context) (int64, error) {
	return int64(len(r.users)), nil
}

func (r *inMemUserRepo) List(_ context.Context) ([]*domain.User, error) {
	out := make([]*domain.User, 0, len(r.users))
	for _, u := range r.users {
		clone := *u
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type inMemAPIKeyRepo struct {
	keys   map[int64]*domain.APIKey
	byHash map[string]int64
	next   int64
}

func newInMemAPIKeyRepo() *inMemAPIKeyRepo {
	return &inMemAPIKeyRepo{
		keys:   map[int64]*domain.APIKey{},
		byHash: map[string]int64{},
	}
}

func (r *inMemAPIKeyRepo) Create(_ context.Context, ak *domain.APIKey) error {
	if _, ok := r.byHash[ak.KeyHash]; ok {
		return ports.ErrConflict
	}
	r.next++
	ak.ID = r.next
	r.keys[ak.ID] = ak
	r.byHash[ak.KeyHash] = ak.ID
	return nil
}

func (r *inMemAPIKeyRepo) GetByHash(_ context.Context, hash string) (*domain.APIKey, error) {
	id, ok := r.byHash[hash]
	if !ok {
		return nil, ports.ErrNotFound
	}
	clone := *r.keys[id]
	return &clone, nil
}

func (r *inMemAPIKeyRepo) List(_ context.Context, userID int64) ([]*domain.APIKey, error) {
	out := []*domain.APIKey{}
	for _, k := range r.keys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (r *inMemAPIKeyRepo) Update(_ context.Context, ak *domain.APIKey) error {
	if _, ok := r.keys[ak.ID]; !ok {
		return ports.ErrNotFound
	}
	r.keys[ak.ID] = ak
	return nil
}

func (r *inMemAPIKeyRepo) Delete(_ context.Context, id int64) error {
	if _, ok := r.keys[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.keys, id)
	return nil
}

// Compile-time guard.
var (
	_ ports.UserRepository   = (*inMemUserRepo)(nil)
	_ ports.APIKeyRepository = (*inMemAPIKeyRepo)(nil)
)
