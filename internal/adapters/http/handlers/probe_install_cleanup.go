package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Bounds of the M8 cleanup HTTP contract: a 128 KiB request body and the
// domain's 64 KiB transient private key.
const (
	probeCleanupMaxBodyBytes       = 128 << 10
	probeCleanupRetryAfterSeconds  = "5"
	probeCleanupKind               = "probe_install_cleanup"
	probeCleanupFieldCleanupID     = "cleanup_id"
	probeCleanupFieldPrivateKey    = "private_key"
	probeCleanupRequestMemberCount = 2
)

// probeCleanupRunner is the durable asynchronous cleanup service behind the
// administrative routes. Without it the routes stay registered but 503.
type probeCleanupRunner interface {
	Submit(context.Context, services.ProbeCleanupSubmission) (*domain.ProbeCleanup, error)
	Latest(context.Context, string) (*domain.ProbeCleanup, error)
}

// ProbeCleanupHandlers exposes the M8 install cleanup routes (POST cleanup
// and GET cleanup) for one install operation. The install identity comes only
// from the path and the actor only from authenticated context. Receipts are
// validated and rendered as explicit views; no path, address, fingerprint,
// owner/attempt ID, private key or remote output reaches a response.
type ProbeCleanupHandlers struct {
	svc     probeCleanupRunner
	enabled bool
}

// NewProbeCleanupHandlers builds the handlers; a disabled surface or a
// missing (including typed-nil) service answers with a typed 503.
func NewProbeCleanupHandlers(svc probeCleanupRunner, enabled bool) *ProbeCleanupHandlers {
	if svc != nil {
		if value := reflect.ValueOf(svc); value.Kind() == reflect.Pointer && value.IsNil() {
			svc = nil
		}
	}
	return &ProbeCleanupHandlers{svc: svc, enabled: enabled}
}

// ProbeCleanupView is the exact response shape. Status cleaned means the host
// helper verified every resource of the install absent; it does not revoke
// the hub's probe registration.
type ProbeCleanupView struct {
	CleanupID   string                    `json:"cleanup_id"`
	OperationID string                    `json:"operation_id"`
	Kind        string                    `json:"kind"`
	Status      string                    `json:"status"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
	DeadlineAt  time.Time                 `json:"deadline_at"`
	Error       *ProbeActivationErrorView `json:"error"`
}

// probeCleanupErrorMessages is the fixed allowlist of stored cleanup codes.
var probeCleanupErrorMessages = map[string]string{
	"not_started":       "the cleanup action never reached the host",
	"owner_interrupted": "cleanup was abandoned before it began",
	"deadline_expired":  "cleanup exceeded its deadline; resources may be partly removed",
	"outcome_unknown":   "the cleanup outcome is unknown; resources may be partly removed",
}

// Submit handles POST /api/probe-installs/:operation_id/cleanup: 202 for a
// committed nonterminal receipt, 200 for an exact retry of a settled one.
func (h *ProbeCleanupHandlers) Submit(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, probeCleanupMaxBodyBytes+1))
	defer zeroInstallBuffer(raw)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_cleanup", "invalid cleanup submission")
	}
	if len(raw) > probeCleanupMaxBodyBytes {
		return fleetError(c, http.StatusRequestEntityTooLarge, "cleanup_too_large", "request body exceeds the cleanup limit")
	}
	cleanupID, key, err := decodeProbeCleanupSubmission(raw)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_cleanup", "invalid cleanup submission")
	}
	privateKey := []byte(key)
	defer zeroInstallBuffer(privateKey)
	userID, _ := userIDFromContext(c)
	submission := services.ProbeCleanupSubmission{CleanupID: cleanupID, OperationID: operationID, PrivateKey: privateKey, RequestedBy: userID}
	if !submission.Valid() {
		return fleetError(c, http.StatusBadRequest, "invalid_cleanup", "invalid cleanup submission")
	}
	cleanup, err := h.svc.Submit(c.Request().Context(), submission)
	if err != nil {
		return probeCleanupServiceError(c, err)
	}
	view, err := toProbeCleanupView(cleanup, operationID)
	if err != nil {
		return probeCleanupUnavailableError(c)
	}
	if cleanup.Terminal() {
		return c.JSON(http.StatusOK, view)
	}
	return c.JSON(http.StatusAccepted, view)
}

// Latest handles GET /api/probe-installs/:operation_id/cleanup and returns
// the install's newest cleanup, reconciled for hard expiry.
func (h *ProbeCleanupHandlers) Latest(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	cleanup, err := h.svc.Latest(c.Request().Context(), operationID)
	if err != nil {
		return probeCleanupServiceError(c, err)
	}
	view, err := toProbeCleanupView(cleanup, operationID)
	if err != nil {
		return probeCleanupUnavailableError(c)
	}
	return c.JSON(http.StatusOK, view)
}

// prepare gates every route like the install and activation surfaces.
func (h *ProbeCleanupHandlers) prepare(c echo.Context) bool {
	c.Response().Header().Set("Cache-Control", "no-store")
	userID, ok := userIDFromContext(c)
	if !ok || userID <= 0 {
		_ = fleetError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return false
	}
	if !h.enabled {
		_ = fleetError(c, http.StatusServiceUnavailable, "cleanups_disabled", "SSH probe cleanup is disabled")
		return false
	}
	if h.svc == nil {
		_ = fleetError(c, http.StatusServiceUnavailable, "cleanup_unavailable", "cleanup operations unavailable")
		return false
	}
	return true
}

var errInvalidCleanupBody = errors.New("invalid cleanup submission body")

// decodeProbeCleanupSubmission accepts exactly one JSON object with exactly
// the two required string members. Unknown or case-variant keys, duplicates,
// non-string or null values, missing members and trailing JSON all refuse.
func decodeProbeCleanupSubmission(raw []byte) (string, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", "", errInvalidCleanupBody
	}
	values := make(map[string]string, probeCleanupRequestMemberCount)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", "", errInvalidCleanupBody
		}
		key, ok := keyToken.(string)
		if !ok || (key != probeCleanupFieldCleanupID && key != probeCleanupFieldPrivateKey) {
			return "", "", errInvalidCleanupBody
		}
		if _, dup := values[key]; dup {
			return "", "", errInvalidCleanupBody
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return "", "", errInvalidCleanupBody
		}
		value, ok := valueToken.(string)
		if !ok {
			return "", "", errInvalidCleanupBody
		}
		values[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return "", "", errInvalidCleanupBody
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", "", errInvalidCleanupBody
	}
	if len(values) != probeCleanupRequestMemberCount {
		return "", "", errInvalidCleanupBody
	}
	return values[probeCleanupFieldCleanupID], values[probeCleanupFieldPrivateKey], nil
}

// toProbeCleanupView validates a complete stored receipt, then renders it. A
// nil, corrupt, mismatched or non-allowlisted receipt fails closed.
func toProbeCleanupView(cleanup *domain.ProbeCleanup, wantOperationID string) (ProbeCleanupView, error) {
	var view ProbeCleanupView
	if cleanup == nil || !cleanup.Valid() || cleanup.Identity.Install.OperationID != wantOperationID {
		return view, errors.New("invalid cleanup receipt")
	}
	message, ok := probeCleanupErrorMessages[cleanup.ErrorCode]
	if cleanup.ErrorCode != "" && !ok {
		return view, errors.New("non-allowlisted cleanup error code")
	}
	view = ProbeCleanupView{
		CleanupID:   cleanup.Identity.CleanupID,
		OperationID: cleanup.Identity.Install.OperationID,
		Kind:        probeCleanupKind,
		Status:      cleanup.State,
		CreatedAt:   cleanup.CreatedAt.UTC(),
		UpdatedAt:   cleanup.UpdatedAt.UTC(),
		DeadlineAt:  cleanup.DeadlineAt.UTC(),
	}
	if cleanup.ErrorCode != "" {
		view.Error = &ProbeActivationErrorView{Code: cleanup.ErrorCode, Message: message}
	}
	return view, nil
}

// probeCleanupServiceError maps the service sentinels onto fixed codes; the
// underlying error text is never echoed.
func probeCleanupServiceError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrInternal):
		return probeCleanupUnavailableError(c)
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrProbeSSHInput):
		return fleetError(c, http.StatusBadRequest, "invalid_cleanup", "invalid cleanup submission")
	case errors.Is(err, domain.ErrProbeCleanupIneligible):
		return fleetError(c, http.StatusConflict, "cleanup_ineligible", "the install still has installer or activation work, or was already cleaned")
	case errors.Is(err, ports.ErrConflict), errors.Is(err, domain.ErrConflict):
		return fleetError(c, http.StatusConflict, "cleanup_conflict", "cleanup id conflicts with a different existing request")
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return fleetError(c, http.StatusNotFound, "cleanup_not_found", "install or cleanup not found")
	case errors.Is(err, domain.ErrProbeInstallBusy):
		c.Response().Header().Set(echo.HeaderRetryAfter, probeCleanupRetryAfterSeconds)
		return fleetError(c, http.StatusServiceUnavailable, "cleanups_busy", "SSH cleanup capacity exhausted")
	case errors.Is(err, domain.ErrProbeInstallClosed):
		return fleetError(c, http.StatusServiceUnavailable, "cleanups_closed", "SSH cleanup executor is stopped")
	default:
		return probeCleanupUnavailableError(c)
	}
}

func probeCleanupUnavailableError(c echo.Context) error {
	return fleetError(c, http.StatusServiceUnavailable, "cleanup_unavailable", "cleanup operations unavailable")
}
