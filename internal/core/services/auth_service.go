// Package services contains the use-case implementations.
// Services depend ONLY on ports and domain — never on adapters.
package services

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// API key parameters.
const (
	apiKeyBytes        = 32            // 256 bits of entropy
	defaultAPIKeyName  = "Unnamed key" // when caller passes ""
	apiKeyPrefix       = "phx_"        // visible identifier on the wire
	defaultAPIKeyScope = "read"        // assigned when caller passes []
)

// AuthServiceError is returned by AuthService methods that need a typed
// error callers can use errors.Is against. It is intentionally unexported
// except for the sentinel values defined below.
type AuthServiceError struct {
	Code    string
	Message string
	Err     error
}

func (e *AuthServiceError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("auth service: %s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("auth service: %s: %s", e.Code, e.Message)
}

func (e *AuthServiceError) Unwrap() error { return e.Err }

// Sentinel errors returned by AuthService. Callers use errors.Is to map
// them to HTTP status codes (see adapters/http/handlers/auth.go).
var (
	// ErrInvalidCredentials is returned when username/password do not match.
	ErrInvalidCredentials = &AuthServiceError{Code: "invalid_credentials", Message: "invalid username or password"}
	// ErrUserNotFound is returned when a user lookup misses.
	ErrUserNotFound = &AuthServiceError{Code: "user_not_found", Message: "user not found"}
	// ErrUserExists is returned by Register when the username is taken.
	ErrUserExists = &AuthServiceError{Code: "user_exists", Message: "username already taken"}
	// ErrRegistrationClosed is returned by Register once the install has a
	// user, including when a concurrent request created the first one.
	ErrRegistrationClosed = &AuthServiceError{Code: "registration_closed", Message: "registration is disabled"}
	// ErrInvalidSetupToken is returned by RegisterWithSetupToken when the
	// one-time setup token is missing or wrong.
	ErrInvalidSetupToken = &AuthServiceError{Code: "invalid_setup_token", Message: "invalid setup token"}
	// ErrUserInactive is returned by Login for accounts that were disabled.
	ErrUserInactive = &AuthServiceError{Code: "user_inactive", Message: "user is inactive"}
	// ErrTOTPRequired is returned by Login when the user has 2FA enabled
	// and the call did not include a TOTP token. Callers exchange the
	// accompanying ticket at /api/auth/verify-2fa.
	ErrTOTPRequired = &AuthServiceError{Code: "totp_required", Message: "TOTP token required"}
	// ErrTOTPInvalid is returned when the supplied TOTP token does not validate.
	ErrTOTPInvalid = &AuthServiceError{Code: "totp_invalid", Message: "invalid TOTP token"}
	// ErrTOTPUnavailable is returned when a TOTP code could not be checked
	// against the stored replay state (a storage failure). The request fails
	// closed: no session is issued and 2FA is not enabled. It is distinct
	// from ErrTOTPInvalid so clients and operators can tell an outage from a
	// wrong or reused code.
	ErrTOTPUnavailable = &AuthServiceError{Code: "totp_unavailable", Message: "TOTP verification is temporarily unavailable"}
	// ErrTicketInvalid is returned when a 2FA challenge ticket is malformed,
	// wrongly signed, expired, or not a ticket (GitHub #122).
	ErrTicketInvalid = &AuthServiceError{Code: "ticket_invalid", Message: "2FA ticket is invalid or expired"}
	// ErrTOTPNotEnabled is returned by EnableTOTP when 2FA is already off
	// and by DisableTOTP when 2FA is already off.
	ErrTOTPNotEnabled = &AuthServiceError{Code: "totp_not_enabled", Message: "TOTP is not enabled"}
	// ErrTOTPAlreadyEnabled is returned by EnableTOTP when called twice.
	ErrTOTPAlreadyEnabled = &AuthServiceError{Code: "totp_already_enabled", Message: "TOTP is already enabled"}
	// ErrWebAuthnNotConfigured is returned by passkey methods when the WebAuthn
	// authenticator has not been wired (see WithWebAuthn).
	ErrWebAuthnNotConfigured = &AuthServiceError{Code: "webauthn_not_configured", Message: "passkeys are not enabled on this server"}
	// ErrWebAuthnChallenge is returned when a passkey ceremony session is
	// missing, expired, or belongs to a different user.
	ErrWebAuthnChallenge = &AuthServiceError{Code: "webauthn_challenge_invalid", Message: "passkey challenge is invalid or expired"}
	// ErrWebAuthnNoCredentials is returned by BeginPasskeyLogin when the user
	// has no registered passkeys.
	ErrWebAuthnNoCredentials = &AuthServiceError{Code: "webauthn_no_credentials", Message: "no passkeys registered for this user"}
	// ErrLastUser is returned by DeleteUser when the delete would remove the
	// last remaining user in the system.
	ErrLastUser = &AuthServiceError{Code: "last_user", Message: "cannot delete the last user"}
	// ErrDeleteSelf is returned by DeleteUser when the target account is the
	// same as the account performing the delete.
	ErrDeleteSelf = &AuthServiceError{Code: "delete_self", Message: "cannot delete your own account"}
	// ErrLastAdmin is returned by DeleteUser when the target is the last
	// remaining admin — deleting it would leave the install with no
	// account able to reach admin-only endpoints (user management).
	ErrLastAdmin = &AuthServiceError{Code: "last_admin", Message: "cannot delete the last admin"}
)

// AuthService handles authentication, user management, and 2FA workflows.
//
// It depends exclusively on ports. All persistence and crypto operations
// are delegated to injected adapters, so the service can be unit-tested
// with in-memory fakes.
type AuthService struct {
	users     ports.UserRepository
	apiKeys   ports.APIKeyRepository
	auth      ports.Authenticator
	twoFactor ports.TwoFactor
	clock     ports.Clock

	// WebAuthn (passkey) dependencies are optional: they are wired via
	// WithWebAuthn. When unset, the passkey methods return ErrWebAuthnNotConfigured.
	webAuthn      ports.WebAuthnAuthenticator
	webAuthnCreds ports.WebAuthnCredentialRepository
	// webAuthnSessions is the short-TTL, in-memory challenge store keyed by a
	// random session id returned to the client. See the type doc for the
	// rationale behind storing ceremony state server-side here rather than in
	// a transient DB table.
	webAuthnSessions *webAuthnSessionStore

	// OIDC SSO dependencies are optional: wired via WithOIDC. When unset,
	// OIDC endpoints return ErrOIDCNotConfigured and local login is unaffected.
	oidc           ports.OIDCAuthenticator
	oidcIdentities ports.OIDCIdentityRepository
	oidcPerms      ports.UserPermissionRepository
	oidcPolicy     OIDCPolicy

	// Optional. AccessService.InvalidateUser so a flag/password change is not
	// served from the 30s auth cache. Wired in bootstrap after both exist.
	onUserChange func(userID int64)

	// throttle holds per-account login backoff and per-ticket 2FA attempt
	// counts (issue #90).
	throttle *loginThrottle

	// setupToken gates first-user self-registration. Empty disables it.
	setupToken string
}

// AuthServiceOption is a functional option for NewAuthService.
type AuthServiceOption func(*AuthService)

// WithFirstUserSetupToken enables first-user self-registration for callers
// that present token (see FirstUserSetupToken). Without it, the first user
// can only come from BOOTSTRAP_USERNAME/BOOTSTRAP_PASSWORD.
func WithFirstUserSetupToken(token string) AuthServiceOption {
	return func(s *AuthService) { s.setupToken = token }
}

// FirstUserSetupToken derives the one-time setup token from the session
// signing secret. Every replica sharing JWT_SECRET derives the same token, so
// the operator can copy it from any instance's log, and nobody without the
// secret or the logs can claim a fresh install.
func FirstUserSetupToken(signingSecret string) string {
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte("uptime-phoenix first-user setup v1"))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// WithClock injects a clock for tests. Production callers should leave
// this unset and use the real wall clock via ports.Clock implementations
// in adapters/clock.
func WithClock(c ports.Clock) AuthServiceOption {
	return func(s *AuthService) { s.clock = c }
}

// WithWebAuthn wires the passkey (WebAuthn) authenticator and credential
// repository. Passkey registration/login methods return
// ErrWebAuthnNotConfigured until this option is supplied, so the rest of the
// service (and existing tests) work unchanged without it.
func WithWebAuthn(wa ports.WebAuthnAuthenticator, creds ports.WebAuthnCredentialRepository) AuthServiceOption {
	return func(s *AuthService) {
		s.webAuthn = wa
		s.webAuthnCreds = creds
	}
}

// NewAuthService creates a new AuthService. If no clock is provided, the
// service uses the standard library's time package via a small default
// implementation in clockAdapter (see below).
func NewAuthService(
	users ports.UserRepository,
	apiKeys ports.APIKeyRepository,
	auth ports.Authenticator,
	twoFactor ports.TwoFactor,
	opts ...AuthServiceOption,
) *AuthService {
	s := &AuthService{
		users:            users,
		apiKeys:          apiKeys,
		auth:             auth,
		twoFactor:        twoFactor,
		clock:            realClock{},
		webAuthnSessions: newWebAuthnSessionStore(),
		throttle:         newLoginThrottle(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// realClock is the default Clock implementation — thin wrapper over time.Now.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// SetUserChangeHook registers a callback after a user row is written (update,
// delete, TOTP). Used to drop AccessService's in-process user cache.
func (s *AuthService) SetUserChangeHook(fn func(userID int64)) {
	s.onUserChange = fn
}

func (s *AuthService) userChanged(userID int64) {
	if s.onUserChange != nil {
		s.onUserChange(userID)
	}
}

// Login authenticates a user with username + password and returns a JWT.
//
// If the user has TOTP enabled this method returns ErrTOTPRequired. The
// caller (HTTP handler) is expected to detect that error and switch into
// the 2FA challenge flow. Use LoginWith2FA when the TOTP token is already
// available, or Begin2FALogin / Complete2FALogin for the two-step flow.
func (s *AuthService) Login(ctx context.Context, username, password string) (string, error) {
	if username == "" || password == "" {
		return "", fmt.Errorf("%w", ErrInvalidCredentials)
	}
	user, att, err := s.beginLoginAttempt(ctx, username, "login")
	if err != nil {
		return "", err
	}
	defer s.throttle.release(att) // no-op once the attempt is settled below
	if !user.Active {
		return "", fmt.Errorf("%w", ErrUserInactive)
	}
	if err := s.auth.VerifyPassword(user.PasswordHash, password); err != nil {
		s.recordLoginFailure(ctx, att, user.Username, "bad password")
		return "", fmt.Errorf("%w", ErrInvalidCredentials)
	}
	if user.TOTPEnabled {
		return "", fmt.Errorf("%w", ErrTOTPRequired)
	}
	token, err := s.auth.Login(ctx, user.Username, password)
	if err == nil {
		s.throttle.succeed(att)
	}
	return token, err
}

// beginLoginAttempt looks up the account username names and reserves one
// attempt against it before any password is checked. Known users are
// throttled by their stored ID, so every spelling the database collation
// maps to the same row (case, accents, full-width letters, trailing spaces
// on MariaDB) shares one budget; unknown names are throttled by their
// normalized text, so a lockout reveals nothing about which names exist.
// The returned attempt must be settled with recordLoginFailure,
// throttle.succeed or throttle.release.
func (s *AuthService) beginLoginAttempt(ctx context.Context, username, op string) (*domain.User, *loginAttempt, error) {
	user, err := s.users.GetByUsername(ctx, username)
	if err != nil && !isNotFound(err) {
		return nil, nil, fmt.Errorf("auth service: %s: %w", op, err)
	}
	account := unknownAccountKey(username)
	if user != nil && err == nil {
		account = userAccountKey(user.ID)
	} else {
		user = nil
	}
	att, err := s.reserveLoginAttempt(ctx, account, username)
	if err != nil {
		return nil, nil, err
	}
	if user == nil {
		s.recordLoginFailure(ctx, att, username, "unknown user")
		return nil, nil, fmt.Errorf("%w", ErrInvalidCredentials)
	}
	return user, att, nil
}

// reserveLoginAttempt claims one attempt for account from the caller's IP,
// or refuses with ErrTooManyAttempts while either backoff applies.
func (s *AuthService) reserveLoginAttempt(ctx context.Context, account, username string) (*loginAttempt, error) {
	ip := loginClientIP(ctx)
	att, wait, ok := s.throttle.reserve(account, ip, s.clock.Now())
	if !ok {
		slog.Warn("auth: login refused during backoff", "username", username, "client_ip", ip, "retry_after", wait.Round(time.Second))
		return nil, fmt.Errorf("%w", ErrTooManyAttempts)
	}
	return att, nil
}

// recordLoginFailure settles att as a failed password or TOTP attempt and
// logs it for audit. The username and client IP are logged; the password
// and code never are.
func (s *AuthService) recordLoginFailure(ctx context.Context, att *loginAttempt, username, reason string) {
	pairN, accountN := s.throttle.fail(att, s.clock.Now())
	attrs := []any{"username", username, "client_ip", loginClientIP(ctx), "reason", reason,
		"consecutive_failures", pairN, "account_failures", accountN}
	switch {
	case accountN >= LoginAccountFailuresBeforeBackoff:
		slog.Warn("auth: repeated login failures from many clients; account in backoff for every client",
			append(attrs, "backoff", loginBackoff(accountN, LoginAccountFailuresBeforeBackoff))...)
	case pairN >= LoginFailuresBeforeBackoff:
		slog.Warn("auth: repeated login failures; account in backoff for this client",
			append(attrs, "backoff", loginBackoff(pairN, LoginFailuresBeforeBackoff))...)
	default:
		slog.Info("auth: login failed", attrs...)
	}
}

// LoginWith2FA authenticates a user with username + password + TOTP token
// in a single call. Useful for clients that collect the TOTP token at
// login time (e.g. CLI tools, API integrations).
func (s *AuthService) LoginWith2FA(ctx context.Context, username, password, totpToken string) (string, error) {
	if username == "" || password == "" || totpToken == "" {
		return "", fmt.Errorf("%w", ErrInvalidCredentials)
	}
	user, att, err := s.beginLoginAttempt(ctx, username, "login with 2fa")
	if err != nil {
		return "", err
	}
	defer s.throttle.release(att)
	if !user.Active {
		return "", fmt.Errorf("%w", ErrUserInactive)
	}
	if err := s.auth.VerifyPassword(user.PasswordHash, password); err != nil {
		s.recordLoginFailure(ctx, att, user.Username, "bad password")
		return "", fmt.Errorf("%w", ErrInvalidCredentials)
	}
	// Mirror Login's behavior: if 2FA is not required, return the token
	// directly. This makes the call safe to use for both modes.
	if user.TOTPEnabled {
		if err := s.consumeTOTP(ctx, user, totpToken, false); err != nil {
			if errors.Is(err, ErrTOTPInvalid) {
				s.recordLoginFailure(ctx, att, user.Username, "bad totp")
			}
			return "", err
		}
	}
	token, err := s.auth.Login(ctx, user.Username, password)
	if err == nil {
		s.throttle.succeed(att)
	}
	return token, err
}

// Begin2FALogin verifies a username + password pair and, if the user has
// 2FA enabled, returns a short-lived challenge ticket. The HTTP handler
// surfaces that ticket to the client, which then exchanges it for a
// session token at /api/auth/verify-2fa.
//
// If 2FA is not enabled, this method returns an empty ticket and a nil
// error so the caller can fall through to a regular login.
func (s *AuthService) Begin2FALogin(ctx context.Context, username, password string) (string, *domain.User, error) {
	user, att, err := s.beginLoginAttempt(ctx, username, "begin 2fa")
	if err != nil {
		return "", nil, err
	}
	// The password is only half of a 2FA sign-in, and a password-only
	// sign-in completes in Login: neither clears the failure history here.
	defer s.throttle.release(att)
	if !user.Active {
		return "", nil, fmt.Errorf("%w", ErrUserInactive)
	}
	if verifyErr := s.auth.VerifyPassword(user.PasswordHash, password); verifyErr != nil {
		s.recordLoginFailure(ctx, att, user.Username, "bad password")
		return "", nil, fmt.Errorf("%w", ErrInvalidCredentials)
	}
	if !user.TOTPEnabled {
		return "", user, nil
	}
	ticket, err := s.auth.IssuePending2FATicket(ctx, user.ID)
	if err != nil {
		return "", nil, fmt.Errorf("auth service: issue 2fa ticket: %w", err)
	}
	return ticket, user, nil
}

// Complete2FALogin validates a 2FA challenge ticket and a TOTP token,
// then issues a session JWT. It is the second half of the split login
// flow; see Begin2FALogin.
func (s *AuthService) Complete2FALogin(ctx context.Context, ticket, totpToken string) (string, *domain.User, error) {
	if ticket == "" || totpToken == "" {
		return "", nil, fmt.Errorf("%w", ErrTOTPInvalid)
	}
	userID, err := s.auth.VerifyPending2FATicket(ctx, ticket)
	if err != nil {
		// Ticket verification is pure token checking (parse, signature,
		// expiry, purpose), so every failure is the client's.
		return "", nil, fmt.Errorf("auth service: verify 2fa ticket: %w: %w", ErrTicketInvalid, err)
	}
	now := s.clock.Now()
	// Count the attempt on the ticket before checking the code, so parallel
	// guesses on one ticket cannot exceed the cap.
	if !s.throttle.reserveTicket(ticket, now) {
		slog.Warn("auth: 2fa ticket refused after attempt cap", "user_id", userID, "client_ip", loginClientIP(ctx))
		return "", nil, fmt.Errorf("%w", ErrTooManyAttempts)
	}
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return "", nil, fmt.Errorf("auth service: complete 2fa: %w", err)
	}
	att, err := s.reserveLoginAttempt(ctx, userAccountKey(user.ID), user.Username)
	if err != nil {
		return "", nil, err
	}
	defer s.throttle.release(att)
	if !user.TOTPEnabled {
		return "", nil, fmt.Errorf("%w", ErrTOTPNotEnabled)
	}
	if err := s.consumeTOTP(ctx, user, totpToken, false); err != nil {
		if errors.Is(err, ErrTOTPInvalid) {
			s.recordLoginFailure(ctx, att, user.Username, "bad totp")
		}
		return "", nil, err
	}
	s.throttle.spendTicket(ticket, now)
	s.throttle.succeed(att)
	// Ticket + valid TOTP code is the credential at this point. Mint a
	// session token directly via the Authenticator so the password is
	// not required a second time.
	token, err := s.auth.IssueSession(ctx, user.ID)
	if err != nil {
		return "", nil, fmt.Errorf("auth service: issue session: %w", err)
	}
	return token, user, nil
}

// consumeTOTP checks token against user's stored TOTP credential at the
// service clock and atomically consumes the matched time step for that
// credential generation (confirm=true also enables a pending credential).
// It returns ErrTOTPInvalid for a wrong, replayed or superseded code and
// ErrTOTPUnavailable when replay state cannot be read or written. Neither the
// secret nor the code is ever logged.
func (s *AuthService) consumeTOTP(ctx context.Context, user *domain.User, token string, confirm bool) error {
	step, ok := s.twoFactor.MatchToken(user.TOTPSecret, token, s.clock.Now())
	if !ok {
		return fmt.Errorf("%w", ErrTOTPInvalid)
	}
	consume := s.users.ConsumeTOTPStep
	if confirm {
		consume = s.users.ConfirmTOTPCredential
	}
	won, err := consume(ctx, user.ID, user.TOTPGeneration, step)
	if err != nil {
		slog.Error("auth: totp replay state unavailable; refusing code", "user_id", user.ID, "error", err)
		return fmt.Errorf("%w: %w", ErrTOTPUnavailable, err)
	}
	if !won {
		slog.Info("auth: totp code refused as reused or superseded", "user_id", user.ID)
		return fmt.Errorf("%w", ErrTOTPInvalid)
	}
	return nil
}

// VerifyToken validates a session JWT and checks that its principal still exists
// and is active. Account deactivation/deletion takes effect on the next request,
// without waiting for a previously issued token to expire.
func (s *AuthService) VerifyToken(ctx context.Context, token string) (int64, error) {
	id, err := s.auth.VerifyToken(ctx, token)
	if err != nil {
		return 0, err
	}
	if s.users == nil {
		return 0, domain.ErrInternal
	}
	user, err := s.users.GetByID(ctx, id)
	if errors.Is(err, ports.ErrNotFound) {
		return 0, ErrInvalidCredentials
	}
	if err != nil {
		return 0, fmt.Errorf("auth service: verify session principal: %w", err)
	}
	if user == nil || user.ID != id || !user.Active {
		return 0, ErrInvalidCredentials
	}
	return id, nil
}

// Register creates a new user with the supplied credentials.
//
// The plaintext password is hashed via the Authenticator (bcrypt) and the
// new user is marked active=true. Returns ErrUserExists if the username
// is already taken.
//
// Note: the task spec mentions a "first user becomes admin" rule. Phoenix
// does not have a roles table yet, so all users (including the first) are
// created with Active=true and no role flags. When the role system lands,
// this method is the only place that needs to be updated.
//
// Register is only reachable via the HTTP layer when no users exist yet
// (first-user bootstrap) — see handlers.AuthHandlers.Register, which gates
// on HasUsers before calling this. It delegates to CreateUser so both
// entry points share one validation/creation path. The bootstrap user is
// always created as an admin — there is no other way to reach the
// admin-only user management endpoints on a fresh install.
//
// The insert is conditional on the install still having no users, so
// concurrent first registrations (on any replica) create exactly one admin;
// the rest get ErrRegistrationClosed.
func (s *AuthService) Register(ctx context.Context, username, password string) (*domain.User, error) {
	// The bootstrap user is an admin, and admins implicitly hold every capability
	// (see AccessService), so the explicit flags stay false here.
	u, err := s.newUser(username, password, true, true, "UTC", UserCapabilities{})
	if err != nil {
		return nil, err
	}
	if err := s.users.CreateFirst(ctx, u); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return nil, ErrRegistrationClosed
		}
		// A losing SQLite writer can fail with "busy" rather than a conflict;
		// report it as closed once the winner is visible.
		if n, countErr := s.users.Count(ctx); countErr == nil && n > 0 {
			return nil, ErrRegistrationClosed
		}
		return nil, fmt.Errorf("auth service: register: %w", err)
	}
	return u, nil
}

// RegisterWithSetupToken is the self-service first-user path: it requires the
// one-time setup token configured with WithFirstUserSetupToken.
func (s *AuthService) RegisterWithSetupToken(ctx context.Context, setupToken, username, password string) (*domain.User, error) {
	if s.setupToken == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(setupToken)), []byte(s.setupToken)) != 1 {
		return nil, ErrInvalidSetupToken
	}
	return s.Register(ctx, username, password)
}

// UserCapabilities carries the optional account-level powers a NON-admin user
// can be given. They are grouped into a struct rather than bolted on as more
// positional bools so a caller cannot silently transpose them, and so adding a
// capability later does not churn every call site again.
//
// Admins are unaffected by these: domain.User.IsAdmin implies all of them.
type UserCapabilities struct {
	CanManageNotifications bool
	CanManageMaintenance   bool
	// CanViewExtensions grants discovery and launching through the Phoenix UI.
	// Extension services still own authorization on their direct Ingress paths.
	CanViewExtensions bool
	// CanViewAllMonitors grants install-wide read-only visibility of every
	// monitor and group. It does not grant write — that remains admin or owner.
	CanViewAllMonitors bool
	// CanCreateMonitors / CanCreateGroups let a non-admin create that resource
	// type and then edit and delete what they created. Note the asymmetry with
	// the two above: those are install-wide powers (a notification manager may
	// touch every notification), while these are owner-scoped — the capability
	// gates making NEW things, and ownership gates touching existing ones. See
	// AccessService.CanEditMonitor.
	CanCreateMonitors bool
	// CanCreateTopLevelMonitors widens monitor placement to group_id null. It is
	// only meaningful together with CanCreateMonitors (the create route still
	// requires that flag).
	CanCreateTopLevelMonitors bool
	CanCreateGroups           bool
	// CanEditGroupMetadata lets a non-admin change folder metadata on groups
	// they can view — not name, parent, or delete. See AccessService.CanEditGroupMetadata.
	CanEditGroupMetadata bool
}

// CapabilityUpdate is the partial-update twin of UserCapabilities: a nil field
// leaves that capability unchanged, so an older client that does not send a flag
// cannot silently strip it.
//
// This is a struct for the same reason UserCapabilities is, only more so —
// positionally these are interchangeable *bool values, and a transposition would
// hand out the wrong power silently and pass every type check.
type CapabilityUpdate struct {
	CanManageNotifications    *bool
	CanManageMaintenance      *bool
	CanViewExtensions         *bool
	CanViewAllMonitors        *bool
	CanCreateMonitors         *bool
	CanCreateTopLevelMonitors *bool
	CanCreateGroups           *bool
	CanEditGroupMetadata      *bool
}

// CreateUser creates a new user with admin-supplied fields. This is the
// path used by authenticated user management (POST /api/users) as well as
// by Register for the first-user bootstrap flow.
//
// username is trimmed and must be non-empty; password must be at least 8
// characters; an empty timezone defaults to "UTC". Returns ErrUserExists
// if the username is already taken.
func (s *AuthService) CreateUser(ctx context.Context, username, password string, active, isAdmin bool, timezone string, caps UserCapabilities) (*domain.User, error) {
	u, err := s.newUser(username, password, active, isAdmin, timezone, caps)
	if err != nil {
		return nil, err
	}
	if err := s.users.Create(ctx, u); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return nil, fmt.Errorf("%w", ErrUserExists)
		}
		return nil, fmt.Errorf("auth service: create user: create: %w", err)
	}
	return u, nil
}

// newUser validates the fields and hashes the password.
func (s *AuthService) newUser(username, password string, active, isAdmin bool, timezone string, caps UserCapabilities) (*domain.User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("auth service: create user: %w: username is required", domain.ErrValidation)
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("auth service: create user: %w: password must be at least 8 characters", domain.ErrValidation)
	}
	if timezone == "" {
		timezone = "UTC"
	}
	hash, err := s.auth.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("auth service: create user: hash password: %w", err)
	}
	u := &domain.User{
		Username:                  username,
		PasswordHash:              hash,
		Active:                    active,
		IsAdmin:                   isAdmin,
		CanManageNotifications:    caps.CanManageNotifications,
		CanManageMaintenance:      caps.CanManageMaintenance,
		CanViewExtensions:         caps.CanViewExtensions,
		CanViewAllMonitors:        caps.CanViewAllMonitors,
		CanCreateMonitors:         caps.CanCreateMonitors,
		CanCreateTopLevelMonitors: caps.CanCreateTopLevelMonitors,
		CanCreateGroups:           caps.CanCreateGroups,
		CanEditGroupMetadata:      caps.CanEditGroupMetadata,
		Timezone:                  timezone,
	}
	return u, nil
}

// ListUsers returns every user in the system, ordered by id ascending.
func (s *AuthService) ListUsers(ctx context.Context) ([]*domain.User, error) {
	users, err := s.users.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth service: list users: %w", err)
	}
	return users, nil
}

// UpdateUser applies a partial update to a user record. A nil pointer field
// leaves the corresponding value unchanged. When password is non-nil it
// must be at least 8 characters and is hashed before being persisted.
// Returns ErrUserNotFound if id does not exist and ErrUserExists if the
// new username collides with another account.
//
// Note: UpdateUser intentionally does not itself guard against an admin
// demoting the last remaining admin (unlike DeleteUser's equivalent
// guard) — locking the is_admin flag down is a narrower, more contained
// class of change than deleting the account outright.
//
// Every field in caps follows the same nil-means-unchanged rule as the rest, so
// an older client that does not send a capability cannot silently strip it.
//
// Note what demoting an admin (isAdmin false) does NOT do: it does not touch the
// capability flags, which may all be false, and it does not touch anything the
// user created. A demoted admin keeps ownership of their monitors and groups —
// AccessService.CanEditMonitor reads Monitor.UserID, not the capability — so
// they can still edit those, and only those. If the intent is to strip them
// completely, clear the flags in the same request and reassign or delete what
// they own; demotion alone does not orphan it.
func (s *AuthService) UpdateUser(ctx context.Context, id int64, username *string, active, isAdmin *bool, timezone *string, password *string, caps CapabilityUpdate) (*domain.User, error) {
	user, err := s.users.GetByID(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %d", ErrUserNotFound, id)
		}
		return nil, fmt.Errorf("auth service: update user: %w", err)
	}
	if username != nil {
		trimmed := strings.TrimSpace(*username)
		if trimmed == "" {
			return nil, fmt.Errorf("auth service: update user: %w: username is required", domain.ErrValidation)
		}
		user.Username = trimmed
	}
	if active != nil {
		user.Active = *active
	}
	if isAdmin != nil {
		user.IsAdmin = *isAdmin
	}
	if caps.CanManageNotifications != nil {
		user.CanManageNotifications = *caps.CanManageNotifications
	}
	if caps.CanManageMaintenance != nil {
		user.CanManageMaintenance = *caps.CanManageMaintenance
	}
	if caps.CanViewExtensions != nil {
		user.CanViewExtensions = *caps.CanViewExtensions
	}
	if caps.CanViewAllMonitors != nil {
		user.CanViewAllMonitors = *caps.CanViewAllMonitors
	}
	if caps.CanCreateMonitors != nil {
		user.CanCreateMonitors = *caps.CanCreateMonitors
	}
	if caps.CanCreateTopLevelMonitors != nil {
		user.CanCreateTopLevelMonitors = *caps.CanCreateTopLevelMonitors
	}
	if caps.CanCreateGroups != nil {
		user.CanCreateGroups = *caps.CanCreateGroups
	}
	if caps.CanEditGroupMetadata != nil {
		user.CanEditGroupMetadata = *caps.CanEditGroupMetadata
	}
	if timezone != nil {
		tz := *timezone
		if tz == "" {
			tz = "UTC"
		}
		user.Timezone = tz
	}
	if password != nil {
		if len(*password) < 8 {
			return nil, fmt.Errorf("auth service: update user: %w: password must be at least 8 characters", domain.ErrValidation)
		}
		hash, err := s.auth.HashPassword(*password)
		if err != nil {
			return nil, fmt.Errorf("auth service: update user: hash password: %w", err)
		}
		user.PasswordHash = hash
	}
	if err := s.users.Update(ctx, user); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return nil, fmt.Errorf("%w", ErrUserExists)
		}
		return nil, fmt.Errorf("auth service: update user: save: %w", err)
	}
	if password != nil || (active != nil && *active) {
		// An admin setting the password or (re)activating the account is the
		// unlock path for a sign-in lockout (docs/RUNBOOK.md).
		s.throttle.clearAccount(user.ID)
	}
	s.userChanged(user.ID)
	return user, nil
}

// DeleteUser removes a user, guarding against three destructive edge
// cases: a caller deleting their own account, deleting the last remaining
// user (which would lock everyone out, since there is no re-bootstrap path
// once a user exists), and deleting the last remaining admin (which would
// strand the install with users but no one able to reach admin-only
// endpoints). Returns ErrDeleteSelf, ErrLastUser, ErrLastAdmin, or
// ErrUserNotFound as appropriate.
func (s *AuthService) DeleteUser(ctx context.Context, currentUserID, targetID int64) error {
	if targetID == currentUserID {
		return fmt.Errorf("%w", ErrDeleteSelf)
	}
	count, err := s.users.Count(ctx)
	if err != nil {
		return fmt.Errorf("auth service: delete user: count: %w", err)
	}
	if count <= 1 {
		return fmt.Errorf("%w", ErrLastUser)
	}
	target, err := s.users.GetByID(ctx, targetID)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %d", ErrUserNotFound, targetID)
		}
		return fmt.Errorf("auth service: delete user: %w", err)
	}
	if target.IsAdmin {
		users, listErr := s.users.List(ctx)
		if listErr != nil {
			return fmt.Errorf("auth service: delete user: list: %w", listErr)
		}
		admins := 0
		for _, u := range users {
			if u.IsAdmin {
				admins++
			}
		}
		if admins <= 1 {
			return fmt.Errorf("%w", ErrLastAdmin)
		}
	}
	if err := s.users.Delete(ctx, targetID); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %d", ErrUserNotFound, targetID)
		}
		return fmt.Errorf("auth service: delete user: %w", err)
	}
	s.userChanged(targetID)
	return nil
}

// GetUser returns a user by ID.
func (s *AuthService) GetUser(ctx context.Context, userID int64) (*domain.User, error) {
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %d", ErrUserNotFound, userID)
		}
		return nil, fmt.Errorf("auth service: get user: %w", err)
	}
	return u, nil
}

// HasUsers returns true if at least one user exists in the system.
// This is used by the frontend to decide whether to show the register
// form or only the login form (first-user flow).
func (s *AuthService) HasUsers(ctx context.Context) (bool, error) {
	count, err := s.users.Count(ctx)
	if err != nil {
		return false, fmt.Errorf("auth service: has users: %w", err)
	}
	return count > 0, nil
}

// SetupTOTP generates a new TOTP secret and provisioning URL, and
// persists the secret on the user record. TOTPEnabled stays false until
// the user confirms the setup by calling EnableTOTP with a valid token.
//
// This split is intentional: it lets the user see the QR code / secret,
// add it to their authenticator, and only commit once they have proven
// they can produce a valid code.
func (s *AuthService) SetupTOTP(ctx context.Context, userID int64) (secret, qrURL string, err error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return "", "", fmt.Errorf("auth service: setup totp: %w", err)
	}
	secret, qrURL, err = s.twoFactor.GenerateSecret("Phoenix", user.Username)
	if err != nil {
		return "", "", fmt.Errorf("auth service: setup totp: %w", err)
	}
	// One statement stores the secret, starts a new credential generation
	// and resets replay state, so no code checked against an older secret
	// (or older snapshot) can be consumed for this one.
	if err := s.users.ReplaceTOTPCredential(ctx, user.ID, secret); err != nil {
		return "", "", fmt.Errorf("auth service: setup totp: save: %w", err)
	}
	s.userChanged(user.ID)
	return secret, qrURL, nil
}

// EnableTOTP verifies a TOTP token against the user's stored secret and,
// if valid, flips TOTPEnabled=true. Returns ErrTOTPAlreadyEnabled if 2FA
// is already on and ErrTOTPInvalid on a bad code.
func (s *AuthService) EnableTOTP(ctx context.Context, userID int64, token string) error {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth service: enable totp: %w", err)
	}
	if user.TOTPEnabled {
		return fmt.Errorf("%w", ErrTOTPAlreadyEnabled)
	}
	if user.TOTPSecret == "" {
		return fmt.Errorf("auth service: enable totp: %w: setup must be called first", domain.ErrValidation)
	}
	// Confirmation consumes the code's step and enables 2FA in one
	// statement, so the setup code cannot also complete a sign-in.
	if err := s.consumeTOTP(ctx, user, token, true); err != nil {
		return err
	}
	s.userChanged(user.ID)
	return nil
}

// DisableTOTP clears the stored TOTP secret and sets TOTPEnabled=false.
// When 2FA is enabled the caller must present a current TOTP token, which is
// matched and atomically consumed exactly like a login code (#123): a code
// that was already used for a login, an enrolment or an earlier disable is
// refused with ErrTOTPInvalid, and of two concurrent requests with the same
// code only one can win. A pending (set up but never confirmed) credential
// protects nothing yet, so it is cleared without a token. The caller is
// responsible for checking the account password first.
func (s *AuthService) DisableTOTP(ctx context.Context, userID int64, token string) error {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth service: disable totp: %w", err)
	}
	if !user.TOTPEnabled && user.TOTPSecret == "" {
		return fmt.Errorf("%w", ErrTOTPNotEnabled)
	}
	if user.TOTPEnabled {
		if err := s.consumeTOTP(ctx, user, token, false); err != nil {
			return err
		}
	}
	if err := s.users.ReplaceTOTPCredential(ctx, user.ID, ""); err != nil {
		return fmt.Errorf("auth service: disable totp: save: %w", err)
	}
	s.userChanged(user.ID)
	return nil
}

// CreateAPIKey mints a new API key for a user. The plaintext key is
// returned exactly once; only its SHA-256 hash is stored.
//
// Format: "phx_" + base64url(32 random bytes) — the prefix lets the
// plaintext format be distinguished from a regular session JWT in logs
// and support tooling.
//
// expiresAt is optional. When non-nil it is stored as UTC; middleware
// rejects the key once clock.Now() is not before that instant.
func (s *AuthService) CreateAPIKey(ctx context.Context, userID int64, name string, scopes []string, expiresAt *time.Time) (string, *domain.APIKey, error) {
	if _, err := s.users.GetByID(ctx, userID); err != nil {
		return "", nil, fmt.Errorf("auth service: create api key: %w", err)
	}
	if strings.TrimSpace(name) == "" {
		name = defaultAPIKeyName
	}
	if len(scopes) == 0 {
		scopes = []string{defaultAPIKeyScope}
	}
	plaintext, err := generateAPIKey()
	if err != nil {
		return "", nil, fmt.Errorf("auth service: create api key: %w", err)
	}
	hash := hashAPIKey(plaintext)
	ak := &domain.APIKey{
		UserID:    userID,
		Name:      name,
		KeyHash:   hash,
		Active:    true,
		Scopes:    scopes,
		CreatedAt: s.clock.Now(),
	}
	if expiresAt != nil && !expiresAt.IsZero() {
		utc := expiresAt.UTC()
		ak.ExpiresAt = &utc
	}
	if err := s.apiKeys.Create(ctx, ak); err != nil {
		return "", nil, fmt.Errorf("auth service: create api key: save: %w", err)
	}
	return plaintext, ak, nil
}

// APIKeyExpired reports whether ak has a non-nil ExpiresAt at or before now.
// Used by auth middleware; exported so both session-or-apikey and apikey
// middleware share one definition.
func APIKeyExpired(ak *domain.APIKey, now time.Time) bool {
	if ak == nil || ak.ExpiresAt == nil || ak.ExpiresAt.IsZero() {
		return false
	}
	return !now.UTC().Before(ak.ExpiresAt.UTC())
}

// ListAPIKeys returns the API keys belonging to the user (hashes omitted).
func (s *AuthService) ListAPIKeys(ctx context.Context, userID int64) ([]*domain.APIKey, error) {
	return s.apiKeys.List(ctx, userID)
}

// RevokeAPIKey deletes the API key if it belongs to the user.
func (s *AuthService) RevokeAPIKey(ctx context.Context, userID, id int64) error {
	keys, err := s.apiKeys.List(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth service: revoke api key: %w", err)
	}
	for _, k := range keys {
		if k.ID == id {
			if err := s.apiKeys.Delete(ctx, id); err != nil {
				return fmt.Errorf("auth service: revoke api key: %w", err)
			}
			return nil
		}
	}
	return ports.ErrNotFound
}

// generateAPIKey returns a fresh "phx_" prefixed API key with 256 bits
// of entropy drawn from crypto/rand.
func generateAPIKey() (string, error) {
	buf := make([]byte, apiKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return apiKeyPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// FingerprintAPIKey returns the lowercase hex SHA-256 of a high-entropy API
// token for exact-match storage lookup. This is not password hashing: API keys
// are generated with 256 bits of crypto/rand entropy (see generateAPIKey).
// User passwords use bcrypt in adapters/auth/password.go.
func FingerprintAPIKey(token string) string {
	// codeql[go/weak-sensitive-data-hashing]: API token fingerprint (256-bit entropy), not password hashing — passwords use bcrypt
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// hashAPIKey is the internal alias used by AuthService create/lookup paths.
func hashAPIKey(plaintext string) string {
	return FingerprintAPIKey(plaintext)
}

// isNotFound reports whether err originates from a "not found" condition.
// Adapters may return either the domain-level or port-level sentinel;
// we accept both so the service stays decoupled from the storage choice.
func isNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound)
}
