package handlers

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// OIDCStatusResponse is the public wire shape for GET /api/auth/oidc/status.
// It never includes secrets or client credentials.
type OIDCStatusResponse struct {
	Enabled bool `json:"enabled"`
}

// OIDCStatus handles GET /api/auth/oidc/status.
func (h *AuthHandlers) OIDCStatus(c echo.Context) error {
	return c.JSON(http.StatusOK, OIDCStatusResponse{Enabled: h.svc.OIDCEnabled()})
}

// OIDCLogin handles GET /api/auth/oidc/login — redirects the browser to the IdP.
func (h *AuthHandlers) OIDCLogin(c echo.Context) error {
	authURL, _, err := h.svc.BeginOIDCLogin(c.Request().Context())
	if err != nil {
		return mapOIDCError(c, err)
	}
	return c.Redirect(http.StatusFound, authURL)
}

// OIDCCallback handles GET /api/auth/oidc/callback.
//
// On success it redirects to the SPA with ?oidc_token=<jwt>. On failure it
// redirects to the SPA with ?oidc_error=<code> so the login page can show a
// message without leaving the operator on a bare JSON error.
func (h *AuthHandlers) OIDCCallback(c echo.Context) error {
	if errParam := c.QueryParam("error"); errParam != "" {
		return c.Redirect(http.StatusFound, h.svc.OIDCFrontendErrorRedirect(errParam))
	}
	code := c.QueryParam("code")
	state := c.QueryParam("state")
	if code == "" || state == "" {
		return c.Redirect(http.StatusFound, h.svc.OIDCFrontendErrorRedirect("missing_params"))
	}
	token, _, err := h.svc.CompleteOIDCLogin(c.Request().Context(), code, state)
	if err != nil {
		code := oidcErrorCode(err)
		return c.Redirect(http.StatusFound, h.svc.OIDCFrontendErrorRedirect(code))
	}
	return c.Redirect(http.StatusFound, h.svc.OIDCFrontendRedirect(token))
}

// OIDCLogout handles GET /api/auth/oidc/logout.
//
// Clears nothing server-side (sessions are bearer JWTs). When the IdP advertises
// an end-session endpoint, redirects there; otherwise returns 204 so the SPA
// can finish local logout itself.
func (h *AuthHandlers) OIDCLogout(c echo.Context) error {
	post := c.QueryParam("post_logout_redirect_uri")
	if post == "" {
		post = c.QueryParam("redirect")
	}
	url := h.svc.OIDCLogoutURL(post)
	if url == "" {
		return c.NoContent(http.StatusNoContent)
	}
	return c.Redirect(http.StatusFound, url)
}

func oidcErrorCode(err error) string {
	switch {
	case errors.Is(err, services.ErrOIDCNotConfigured):
		return "not_configured"
	case errors.Is(err, services.ErrOIDCInvalidState):
		return "invalid_state"
	case errors.Is(err, services.ErrOIDCAccessDenied):
		return "access_denied"
	case errors.Is(err, services.ErrOIDCNoAccount):
		return "no_account"
	case errors.Is(err, services.ErrUserInactive):
		return "inactive"
	case errors.Is(err, services.ErrOIDCExchange):
		return "exchange_failed"
	default:
		return "login_failed"
	}
}

func mapOIDCError(c echo.Context, err error) error {
	switch {
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
	case errors.Is(err, services.ErrUserInactive):
		return c.JSON(http.StatusForbidden, errorBody("user is inactive"))
	default:
		return mapAuthError(c, err)
	}
}
