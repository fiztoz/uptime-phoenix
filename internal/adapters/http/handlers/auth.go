// Package handlers contains Echo HTTP handlers for the Phoenix REST API.
//
// Handlers are thin: they parse the request, call the appropriate service
// method, and translate domain errors into HTTP status codes. All business
// logic lives in internal/core/services.
package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// AuthHandlers groups the auth-related HTTP endpoints behind a single
// receiver. All methods are safe for concurrent use because the underlying
// AuthService is stateless.
type AuthHandlers struct {
	svc *services.AuthService
}

// NewAuthHandlers creates auth handlers bound to the supplied service.
func NewAuthHandlers(svc *services.AuthService) *AuthHandlers {
	return &AuthHandlers{svc: svc}
}

// ContextUserIDKey is the Echo context key under which AuthMiddleware
// stores the verified user ID. It is exported so the middleware and
// the handlers (and any future code that needs to read the user ID)
// can use a single, compile-checked key.
const ContextUserIDKey = "userID"

// --- Request / response DTOs ---------------------------------------------

// RegisterRequest is the body of POST /api/auth/register.
type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginRequest is the body of POST /api/auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse is returned for both successful password-only login and
// the "totp required" intermediate state. The presence of Requires2FA
// tells the client which follow-up step is needed.
type LoginResponse struct {
	Token       string    `json:"token,omitempty"`
	User        *UserView `json:"user,omitempty"`
	Requires2FA bool      `json:"requires_2fa,omitempty"`
	Ticket      string    `json:"ticket,omitempty"`
}

// Verify2FARequest is the body of POST /api/auth/verify-2fa.
type Verify2FARequest struct {
	Ticket string `json:"ticket"`
	Token  string `json:"token"`
}

// Setup2FAResponse is the body of POST /api/auth/setup-2fa. The secret
// is returned so the user can manually paste it into an authenticator
// app that cannot scan a QR code.
type Setup2FAResponse struct {
	Secret string `json:"secret"`
	QRURL  string `json:"qr_url"`
}

// Enable2FARequest is the body of POST /api/auth/enable-2fa.
type Enable2FARequest struct {
	Token string `json:"token"`
}

// Disable2FARequest is the body of POST /api/auth/disable-2fa. The
// current password is required as a safety measure to prevent a stolen
// session from silently disabling 2FA.
type Disable2FARequest struct {
	Password string `json:"password"`
}

// UserView is the wire shape of domain.User. We project the internal
// struct to a public DTO so we never accidentally leak the credential
// fields (TOTP secret, hashed password) to the client.
type UserView struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Active   bool   `json:"active"`
	IsAdmin  bool   `json:"is_admin"`
	// The Can* fields are the RAW flags stored on the user, NOT the effective
	// permission. An admin has is_admin=true and every flag false, yet may do
	// everything — the frontend must gate on `is_admin || can_x`, exactly as
	// services.AccessService does on the server. Reporting the raw flags
	// (rather than pre-OR-ing them) keeps the admin edit form able to round-trip
	// a user's actual settings.
	CanManageNotifications bool `json:"can_manage_notifications"`
	CanManageMaintenance   bool `json:"can_manage_maintenance"`
	// CanCreateMonitors / CanCreateGroups gate creating those resources. They do
	// NOT tell the frontend whether this user may edit a monitor already on
	// screen: that is per-resource ownership (monitor.user_id == this user, or
	// is_admin), and no user-level flag can answer it. Gating an edit button on
	// can_create_monitors would show it on every monitor the user can see,
	// including other people's, and the server would then 403 the save.
	// CanCreateTopLevelMonitors additionally allows group_id null placement when
	// creating (or moving) a monitor; it is useless without can_create_monitors.
	CanCreateMonitors         bool `json:"can_create_monitors"`
	CanCreateTopLevelMonitors bool `json:"can_create_top_level_monitors"`
	CanCreateGroups           bool `json:"can_create_groups"`
	// CanEditGroupMetadata allows non-structural edits on visible groups.
	CanEditGroupMetadata bool   `json:"can_edit_group_metadata"`
	Timezone             string `json:"timezone"`
	TOTPEnabled          bool   `json:"totp_enabled"`
	TwoFactorEnabled     bool   `json:"two_factor_enabled"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

// toUserView projects a domain.User to the public DTO. Sensitive
// fields are intentionally dropped — see UserView's docstring.
//
// IsAdmin and the two capability flags are included so the frontend can hide the
// things the user cannot do (monitor/group editing, notification and maintenance
// management, the admin-only pages) straight from /api/auth/me and the
// login/register response, without a follow-up call.
func toUserView(u *domain.User) *UserView {
	if u == nil {
		return nil
	}
	return &UserView{
		ID:                        u.ID,
		Username:                  u.Username,
		Active:                    u.Active,
		IsAdmin:                   u.IsAdmin,
		CanManageNotifications:    u.CanManageNotifications,
		CanManageMaintenance:      u.CanManageMaintenance,
		CanCreateMonitors:         u.CanCreateMonitors,
		CanCreateTopLevelMonitors: u.CanCreateTopLevelMonitors,
		CanCreateGroups:           u.CanCreateGroups,
		CanEditGroupMetadata:      u.CanEditGroupMetadata,
		Timezone:                  u.Timezone,
		TOTPEnabled:               u.TOTPEnabled,
		TwoFactorEnabled:          u.TOTPEnabled,
		CreatedAt:                 u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:                 u.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// --- Handlers -----------------------------------------------------------

// Register handles POST /api/auth/register. On success it returns 201
// with {token, user} so the client can land on the dashboard without a
// follow-up login. The first registered user of an install is implicitly
// "active" — the role system is scheduled for a later sprint, so
// Active=true is the only privilege flag we set today.
//
// Self-registration is disabled once the install has at least one user:
// every subsequent account must be created by an authenticated admin via
// POST /api/users (see UserHandlers.Create). This keeps the bootstrap flow
// (empty install → first user) working while closing off open signup.
func (h *AuthHandlers) Register(c echo.Context) error {
	hasUsers, err := h.svc.HasUsers(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
	if hasUsers {
		return c.JSON(http.StatusForbidden, errorBody("registration is disabled"))
	}

	var req RegisterRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if strings.TrimSpace(req.Username) == "" || req.Password == "" {
		return badRequest(c, "username and password are required")
	}
	user, err := h.svc.Register(c.Request().Context(), req.Username, req.Password)
	if err != nil {
		return mapAuthError(c, err)
	}

	// Auto-login the newly created user. We deliberately reuse the
	// password the client just sent — it is not stored on the server
	// after Register, and re-reading the hash would let a timing
	// attacker distinguish "registered then logged in" from
	// "registered and not logged in".
	sessionToken, loginErr := h.svc.Login(c.Request().Context(), user.Username, req.Password)
	if loginErr != nil {
		// Registration succeeded but auto-login failed. Still respond
		// 201 so the client knows the account exists, but force them
		// to call /login.
		return c.JSON(http.StatusCreated, map[string]any{
			"user":  toUserView(user),
			"token": "",
		})
	}
	return c.JSON(http.StatusCreated, LoginResponse{
		Token: sessionToken,
		User:  toUserView(user),
	})
}

// Login handles POST /api/auth/login. The response shape is the same
// for both happy paths: {token, user} when 2FA is off, and
// {requires_2fa: true, ticket, user} when 2FA is on.
func (h *AuthHandlers) Login(c echo.Context) error {
	var req LoginRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if strings.TrimSpace(req.Username) == "" || req.Password == "" {
		return badRequest(c, "username and password are required")
	}

	ticket, user, err := h.svc.Begin2FALogin(c.Request().Context(), req.Username, req.Password)
	if err != nil {
		return mapAuthError(c, err)
	}
	if ticket != "" {
		// 2FA required: hand the client a short-lived ticket and stop.
		return c.JSON(http.StatusOK, LoginResponse{
			Requires2FA: true,
			Ticket:      ticket,
			User:        toUserView(user),
		})
	}
	// No 2FA: issue a session token directly.
	token, err := h.svc.Login(c.Request().Context(), req.Username, req.Password)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, LoginResponse{
		Token: token,
		User:  toUserView(user),
	})
}

// Verify2FA handles POST /api/auth/verify-2fa. It exchanges a
// challenge ticket + TOTP token for a session token.
func (h *AuthHandlers) Verify2FA(c echo.Context) error {
	var req Verify2FARequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Ticket == "" || req.Token == "" {
		return badRequest(c, "ticket and token are required")
	}
	token, user, err := h.svc.Complete2FALogin(c.Request().Context(), req.Ticket, req.Token)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, LoginResponse{
		Token: token,
		User:  toUserView(user),
	})
}

// Setup2FA handles POST /api/auth/setup-2fa. Auth is required: the
// userID is read from the Echo context (placed there by the
// AuthMiddleware). TOTPEnabled stays false until Enable2FA is called
// with a valid token.
func (h *AuthHandlers) Setup2FA(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	secret, qrURL, err := h.svc.SetupTOTP(c.Request().Context(), userID)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, Setup2FAResponse{
		Secret: secret,
		QRURL:  qrURL,
	})
}

// Enable2FA handles POST /api/auth/enable-2fa. It validates the token
// against the user's stored secret and, if valid, flips
// TOTPEnabled=true.
func (h *AuthHandlers) Enable2FA(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	var req Enable2FARequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Token == "" {
		return badRequest(c, "token is required")
	}
	if err := h.svc.EnableTOTP(c.Request().Context(), userID, req.Token); err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"enabled": true})
}

// Disable2FA handles POST /api/auth/disable-2fa. The current password
// is required so a stolen session token cannot silently turn off 2FA.
func (h *AuthHandlers) Disable2FA(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	var req Disable2FARequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Password == "" {
		return badRequest(c, "password is required")
	}

	user, err := h.svc.GetUser(c.Request().Context(), userID)
	if err != nil {
		return mapAuthError(c, err)
	}

	// Verify the supplied password. We attempt Login and treat
	// ErrTOTPRequired as proof the password was correct — that
	// branch happens exactly when the password check passed but
	// TOTP is still required. This avoids forcing the user to
	// re-enter a TOTP code just to disable 2FA.
	if _, loginErr := h.svc.Login(c.Request().Context(), user.Username, req.Password); loginErr != nil {
		switch {
		case errors.Is(loginErr, services.ErrInvalidCredentials):
			return c.JSON(http.StatusUnauthorized, errorBody("invalid password"))
		case errors.Is(loginErr, services.ErrTOTPRequired):
			// Password was correct; TOTP is the only remaining check.
		default:
			return mapAuthError(c, loginErr)
		}
	}

	if err := h.svc.DisableTOTP(c.Request().Context(), userID); err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"disabled": true})
}

// Me handles GET /api/auth/me. It returns the currently authenticated
// user's profile.
func (h *AuthHandlers) Me(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	user, err := h.svc.GetUser(c.Request().Context(), userID)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"user": toUserView(user)})
}

// HasUsers handles GET /api/auth/has-users. It returns whether at least
// one user exists in the system. This is a public endpoint used by the
// frontend to decide whether to show the register form or only the login
// form (first-user flow).
func (h *AuthHandlers) HasUsers(c echo.Context) error {
	has, err := h.svc.HasUsers(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
	return c.JSON(http.StatusOK, map[string]bool{"has_users": has})
}

// --- WebAuthn (passkey) DTOs ---------------------------------------------

// WebAuthnBeginResponse is returned by the register/login "begin" endpoints.
// PublicKey carries the verbatim creation/request options the browser passes
// to navigator.credentials.create()/.get(); SessionID is the opaque challenge
// handle the client echoes back to the matching "finish" endpoint.
type WebAuthnBeginResponse struct {
	SessionID string          `json:"session_id"`
	PublicKey json.RawMessage `json:"publicKey"`
}

// browserPublicKeyOptions removes the envelope used by go-webauthn's
// CredentialCreation/CredentialAssertion structs. The HTTP response already
// names this field "publicKey", so forwarding the library envelope verbatim
// would produce publicKey.publicKey and leave the browser without a challenge.
func browserPublicKeyOptions(options json.RawMessage) (json.RawMessage, error) {
	var envelope struct {
		PublicKey json.RawMessage `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &envelope); err != nil {
		return nil, fmt.Errorf("decode WebAuthn options: %w", err)
	}
	if len(envelope.PublicKey) == 0 || string(envelope.PublicKey) == "null" {
		return nil, fmt.Errorf("decode WebAuthn options: missing publicKey")
	}
	return envelope.PublicKey, nil
}

// WebAuthnRegisterFinishRequest is the body of register/finish. Credential is
// the raw JSON the browser returns from the authenticator.
type WebAuthnRegisterFinishRequest struct {
	SessionID  string          `json:"session_id"`
	Name       string          `json:"name"`
	Credential json.RawMessage `json:"credential"`
}

// WebAuthnLoginBeginRequest is the body of login/begin.
type WebAuthnLoginBeginRequest struct {
	Username string `json:"username"`
}

// WebAuthnLoginFinishRequest is the body of login/finish.
type WebAuthnLoginFinishRequest struct {
	Username   string          `json:"username"`
	SessionID  string          `json:"session_id"`
	Credential json.RawMessage `json:"credential"`
}

// PasskeyView is the wire shape of a registered passkey. The raw credential ID
// is base64url-encoded for display; the public key is never exposed.
type PasskeyView struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	CredID     string   `json:"credential_id"`
	Transports []string `json:"transports"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at,omitempty"`
}

func toPasskeyView(c *domain.WebAuthnCredential) *PasskeyView {
	v := &PasskeyView{
		ID:         c.ID,
		Name:       c.Name,
		CredID:     base64.RawURLEncoding.EncodeToString(c.CredentialID),
		Transports: c.Transports,
		CreatedAt:  c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if c.Transports == nil {
		v.Transports = []string{}
	}
	if c.LastUsedAt != nil {
		s := c.LastUsedAt.UTC().Format("2006-01-02T15:04:05Z")
		v.LastUsedAt = &s
	}
	return v
}

// --- WebAuthn (passkey) handlers -----------------------------------------

// WebAuthnRegisterBegin handles POST /api/auth/webauthn/register/begin. Auth
// required: starts a passkey registration ceremony for the logged-in user.
func (h *AuthHandlers) WebAuthnRegisterBegin(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	options, sessionID, err := h.svc.BeginPasskeyRegistration(c.Request().Context(), userID)
	if err != nil {
		return mapAuthError(c, err)
	}
	publicKey, err := browserPublicKeyOptions(options)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorBody("could not start passkey registration"))
	}
	return c.JSON(http.StatusOK, WebAuthnBeginResponse{SessionID: sessionID, PublicKey: publicKey})
}

// WebAuthnRegisterFinish handles POST /api/auth/webauthn/register/finish. Auth
// required: validates the authenticator response and stores the credential.
func (h *AuthHandlers) WebAuthnRegisterFinish(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	var req WebAuthnRegisterFinishRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.SessionID == "" || len(req.Credential) == 0 {
		return badRequest(c, "session_id and credential are required")
	}
	cred, err := h.svc.FinishPasskeyRegistration(c.Request().Context(), userID, req.SessionID, strings.TrimSpace(req.Name), req.Credential)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusCreated, toPasskeyView(cred))
}

// WebAuthnListCredentials handles GET /api/auth/webauthn/credentials. Auth
// required: lists the logged-in user's passkeys.
func (h *AuthHandlers) WebAuthnListCredentials(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	creds, err := h.svc.ListPasskeys(c.Request().Context(), userID)
	if err != nil {
		return mapAuthError(c, err)
	}
	out := make([]*PasskeyView, len(creds))
	for i, cr := range creds {
		out[i] = toPasskeyView(cr)
	}
	return c.JSON(http.StatusOK, out)
}

// WebAuthnDeleteCredential handles DELETE /api/auth/webauthn/credentials/:id.
func (h *AuthHandlers) WebAuthnDeleteCredential(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	if err := h.svc.DeletePasskey(c.Request().Context(), userID, id); err != nil {
		return mapAuthError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// WebAuthnLoginBegin handles POST /api/auth/webauthn/login/begin (public). It
// starts a passwordless assertion ceremony for the named user.
func (h *AuthHandlers) WebAuthnLoginBegin(c echo.Context) error {
	var req WebAuthnLoginBeginRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if strings.TrimSpace(req.Username) == "" {
		return badRequest(c, "username is required")
	}
	options, sessionID, err := h.svc.BeginPasskeyLogin(c.Request().Context(), req.Username)
	if err != nil {
		return mapAuthError(c, err)
	}
	publicKey, err := browserPublicKeyOptions(options)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorBody("could not start passkey sign-in"))
	}
	return c.JSON(http.StatusOK, WebAuthnBeginResponse{SessionID: sessionID, PublicKey: publicKey})
}

// WebAuthnLoginFinish handles POST /api/auth/webauthn/login/finish (public). It
// validates the assertion and issues a session token on success.
func (h *AuthHandlers) WebAuthnLoginFinish(c echo.Context) error {
	var req WebAuthnLoginFinishRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if strings.TrimSpace(req.Username) == "" || req.SessionID == "" || len(req.Credential) == 0 {
		return badRequest(c, "username, session_id and credential are required")
	}
	token, user, err := h.svc.FinishPasskeyLogin(c.Request().Context(), req.Username, req.SessionID, req.Credential)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, LoginResponse{Token: token, User: toUserView(user)})
}

// --- Error translation helpers -------------------------------------------

// mapAuthError converts a service-layer error into an HTTP response.
//
// The mapping is intentionally explicit: every branch documents which
// domain error maps to which HTTP status. Adding a new sentinel error
// in services means adding a branch here, which is the kind of friction
// we want — it forces a conscious review of the API contract.
func mapAuthError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, services.ErrInvalidCredentials):
		return c.JSON(http.StatusUnauthorized, errorBody("invalid username or password"))
	case errors.Is(err, services.ErrUserNotFound):
		return c.JSON(http.StatusNotFound, errorBody("user not found"))
	case errors.Is(err, services.ErrUserExists):
		return c.JSON(http.StatusConflict, errorBody("username already taken"))
	case errors.Is(err, services.ErrLastUser):
		return c.JSON(http.StatusConflict, errorBody("cannot delete the last user"))
	case errors.Is(err, services.ErrDeleteSelf):
		return c.JSON(http.StatusConflict, errorBody("cannot delete your own account"))
	case errors.Is(err, services.ErrLastAdmin):
		return c.JSON(http.StatusConflict, errorBody("cannot delete the last admin"))
	case errors.Is(err, services.ErrUserInactive):
		return c.JSON(http.StatusForbidden, errorBody("user is inactive"))
	case errors.Is(err, services.ErrTOTPRequired):
		return c.JSON(http.StatusUnauthorized, errorBody("TOTP token required"))
	case errors.Is(err, services.ErrTOTPInvalid):
		return c.JSON(http.StatusUnauthorized, errorBody("invalid TOTP token"))
	case errors.Is(err, services.ErrTOTPAlreadyEnabled):
		return c.JSON(http.StatusConflict, errorBody("TOTP is already enabled"))
	case errors.Is(err, services.ErrTOTPNotEnabled):
		return c.JSON(http.StatusBadRequest, errorBody("TOTP is not enabled"))
	case errors.Is(err, services.ErrWebAuthnNotConfigured):
		return c.JSON(http.StatusNotImplemented, errorBody("passkeys are not enabled on this server"))
	case errors.Is(err, services.ErrWebAuthnNoCredentials):
		return c.JSON(http.StatusNotFound, errorBody("no passkeys registered for this user"))
	case errors.Is(err, services.ErrWebAuthnChallenge):
		return c.JSON(http.StatusUnauthorized, errorBody("passkey challenge is invalid or expired"))
	case errors.Is(err, services.ErrOIDCNotConfigured):
		return c.JSON(http.StatusNotFound, errorBody("OIDC SSO is not enabled"))
	case errors.Is(err, services.ErrOIDCInvalidState):
		return c.JSON(http.StatusBadRequest, errorBody("OIDC login state is invalid or expired"))
	case errors.Is(err, services.ErrOIDCAccessDenied):
		return c.JSON(http.StatusForbidden, errorBody("your account is not permitted to access Phoenix"))
	case errors.Is(err, services.ErrOIDCNoAccount):
		return c.JSON(http.StatusForbidden, errorBody("no Phoenix account is linked to this identity"))
	case errors.Is(err, services.ErrOIDCExchange):
		return c.JSON(http.StatusUnauthorized, errorBody("OIDC authentication failed"))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	case errors.Is(err, domain.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody(err.Error()))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}

func badRequest(c echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, errorBody(msg))
}

func unauthenticated(c echo.Context) error {
	return c.JSON(http.StatusUnauthorized, errorBody("authentication required"))
}

func errorBody(msg string) map[string]string {
	return map[string]string{"error": msg}
}

// userIDFromContext extracts the userID placed by AuthMiddleware. The
// key is kept as a string constant so middleware and handlers cannot
// disagree on it.
func userIDFromContext(c echo.Context) (int64, bool) {
	v := c.Get(ContextUserIDKey)
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	default:
		return 0, false
	}
}
