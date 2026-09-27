package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// probeAdminRunner is the durable administrative surface behind the probe
// admin routes. Without it the routes stay registered but unavailable.
type probeAdminRunner interface {
	CreateRegistration(context.Context, services.ProbeRegistrationRequest) (*domain.Probe, error)
	UpdateRegistration(context.Context, string, services.ProbeRegistrationPatch) (*domain.Probe, error)
	Enroll(context.Context, string, string) (*domain.ProbeOperation, error)
	RotateCredential(context.Context, string, int64) (*domain.ProbeOperation, error)
	ResetStream(context.Context, string, string, string) (*domain.ProbeOperation, error)
	Operation(context.Context, string) (*domain.ProbeOperation, error)
}

// ProbeAdminHandlers exposes registration writes and durable administrative
// operations (protocol section 7). Requests are decoded with the frozen M0
// client decoders so the accepted wire shape cannot drift from the contract,
// and receipts are rendered exactly as DecodeOperationReceipt requires.
type ProbeAdminHandlers struct {
	svc     probeAdminRunner
	fleet   probeFleetReader
	enabled bool
	now     func() time.Time
}

// NewProbeAdminHandlers builds handlers; disabled surfaces return a typed 503.
func NewProbeAdminHandlers(svc probeAdminRunner, fleet probeFleetReader, enabled bool) *ProbeAdminHandlers {
	return &ProbeAdminHandlers{svc: svc, fleet: fleet, enabled: enabled, now: time.Now}
}

// ProbeOperationView is the exact operation receipt wire shape: exactly
// operation_id, probe_id, status, phase, created_at, updated_at and error.
// A failed receipt carries the bounded redacted error; everything else is null.
type ProbeOperationView struct {
	OperationID string                   `json:"operation_id"`
	ProbeID     string                   `json:"probe_id"`
	Status      string                   `json:"status"`
	Phase       string                   `json:"phase"`
	CreatedAt   time.Time                `json:"created_at"`
	UpdatedAt   time.Time                `json:"updated_at"`
	Error       *ProbeOperationErrorView `json:"error"`
}

// ProbeOperationErrorView is a bounded redacted diagnostic.
type ProbeOperationErrorView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Create handles POST /api/probes. The response is the standard admin detail
// view, initially unconfigured.
func (h *ProbeAdminHandlers) Create(c echo.Context) error {
	if ok := h.prepare(c); !ok {
		return nil
	}
	body, err := readProbeAdminBody(c)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
	}
	request, err := probe.DecodeProbeCreateRequest(body)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_registration", "invalid registration")
	}
	created, err := h.svc.CreateRegistration(c.Request().Context(), services.ProbeRegistrationRequest{
		Key: request.Key, Name: request.Name, Location: request.Location,
		Endpoint: request.Endpoint, TLSPin: request.TLSFingerprint,
	})
	if err != nil {
		return probeAdminRegistrationError(c, err)
	}
	return h.renderRegistration(c, created.ID, http.StatusCreated)
}

// Update handles PATCH /api/probes/:probe_id: name, location and enabled with
// the mandatory expected revision. Endpoint and pin are immutable identity.
func (h *ProbeAdminHandlers) Update(c echo.Context) error {
	if ok := h.prepare(c); !ok {
		return nil
	}
	body, err := readProbeAdminBody(c)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
	}
	request, err := probe.DecodeProbePatchRequest(body)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_registration", "invalid registration patch")
	}
	updated, err := h.svc.UpdateRegistration(c.Request().Context(), c.Param("probe_id"), services.ProbeRegistrationPatch{
		Name: request.Name, Location: request.Location, Enabled: request.Enabled,
		ExpectedRevision: int64(request.Revision),
	})
	if err != nil {
		// An update conflicts only through its expected revision.
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, domain.ErrConflict) {
			return fleetError(c, http.StatusConflict, "stale_revision", "expected revision is stale")
		}
		return probeAdminRegistrationError(c, err)
	}
	return h.renderRegistration(c, updated.ID, http.StatusOK)
}

// Enroll handles POST /api/probes/:probe_id/enroll. The enrollment token is
// write-only: it is validated, consumed and never echoed in any response,
// receipt error or log line.
func (h *ProbeAdminHandlers) Enroll(c echo.Context) error {
	if ok := h.prepare(c); !ok {
		return nil
	}
	body, err := readProbeAdminBody(c)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
	}
	fields, err := probeAdminJSONFields(body)
	if err != nil || len(fields) != 1 {
		return fleetError(c, http.StatusBadRequest, "invalid_operation", "invalid enrollment request")
	}
	token, ok := fields["enrollment_token"].(string)
	if !ok {
		return fleetError(c, http.StatusBadRequest, "invalid_operation", "invalid enrollment request")
	}
	operation, err := h.svc.Enroll(c.Request().Context(), c.Param("probe_id"), token)
	if err != nil {
		return probeAdminOperationError(c, err)
	}
	return c.JSON(http.StatusAccepted, toProbeOperationView(*operation))
}

// RotateCredential handles POST /api/probes/:probe_id/rotate-credential. No
// plaintext token appears in the request or the response.
func (h *ProbeAdminHandlers) RotateCredential(c echo.Context) error {
	if ok := h.prepare(c); !ok {
		return nil
	}
	body, err := readProbeAdminBody(c)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
	}
	request, err := probe.DecodeRotateCredentialRequest(body)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_operation", "invalid credential rotation request")
	}
	operation, err := h.svc.RotateCredential(c.Request().Context(), c.Param("probe_id"), int64(request.CredentialVersion))
	if err != nil {
		return probeAdminOperationError(c, err)
	}
	return c.JSON(http.StatusAccepted, toProbeOperationView(*operation))
}

// ResetStream handles POST /api/probes/:probe_id/reset-stream. The caller
// supplies the durable operation identity (enrollment_operation_id), retained
// for retries; the receipt is readable at /api/probe-operations/:operation_id.
func (h *ProbeAdminHandlers) ResetStream(c echo.Context) error {
	if ok := h.prepare(c); !ok {
		return nil
	}
	body, err := readProbeAdminBody(c)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
	}
	request, err := probe.DecodeResetStreamRequest(body)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_operation", "invalid stream reset request")
	}
	operation, err := h.svc.ResetStream(c.Request().Context(), c.Param("probe_id"), request.StreamID, request.EnrollmentOperationID)
	if err != nil {
		return probeAdminOperationError(c, err)
	}
	return c.JSON(http.StatusAccepted, toProbeOperationView(*operation))
}

// Operation handles GET /api/probe-operations/:operation_id.
func (h *ProbeAdminHandlers) Operation(c echo.Context) error {
	if ok := h.prepare(c); !ok {
		return nil
	}
	operation, err := h.svc.Operation(c.Request().Context(), c.Param("operation_id"))
	if err != nil {
		if errors.Is(err, services.ErrInvalidOperation) {
			return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
		}
		if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
			return fleetError(c, http.StatusNotFound, "operation_not_found", "operation not found")
		}
		return fleetError(c, http.StatusServiceUnavailable, "operation_unavailable", "operation data unavailable")
	}
	return c.JSON(http.StatusOK, toProbeOperationView(*operation))
}

// ErrInvalidOperationView is a local sentinel for malformed operation identity.
var ErrInvalidOperationView = errors.New("invalid operation view request")

func (h *ProbeAdminHandlers) prepare(c echo.Context) bool {
	c.Response().Header().Set("Cache-Control", "no-store")
	userID, ok := userIDFromContext(c)
	if !ok || userID <= 0 {
		_ = fleetError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return false
	}
	if !h.enabled {
		_ = fleetError(c, http.StatusServiceUnavailable, "probes_disabled", "multi-region probes are disabled")
		return false
	}
	if h.svc == nil {
		_ = fleetError(c, http.StatusServiceUnavailable, "fleet_unavailable", "fleet administration unavailable")
		return false
	}
	return true
}

// renderRegistration answers with the standard admin detail view.
func (h *ProbeAdminHandlers) renderRegistration(c echo.Context, probeID string, status int) error {
	if h.fleet == nil {
		return fleetError(c, http.StatusServiceUnavailable, "fleet_unavailable", "fleet data unavailable")
	}
	entry, err := h.fleet.Detail(c.Request().Context(), probeID, h.now().UTC())
	if err != nil {
		return fleetReadError(c, err)
	}
	return c.JSON(status, toProbeDetailView(*entry))
}

func probeAdminRegistrationError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, services.ErrInvalidRegistration), errors.Is(err, domain.ErrValidation):
		return fleetError(c, http.StatusBadRequest, "invalid_registration", "invalid registration")
	case errors.Is(err, ports.ErrConflict), errors.Is(err, domain.ErrConflict):
		return fleetError(c, http.StatusConflict, "registration_conflict", "registration conflicts with an existing identity")
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return fleetError(c, http.StatusNotFound, "probe_not_found", "probe not found")
	default:
		return fleetError(c, http.StatusServiceUnavailable, "fleet_unavailable", "fleet administration unavailable")
	}
}

func probeAdminOperationError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, services.ErrInvalidOperation), errors.Is(err, services.ErrInvalidRegistration), errors.Is(err, domain.ErrValidation):
		return fleetError(c, http.StatusBadRequest, "invalid_operation", "invalid administrative operation request")
	case errors.Is(err, ports.ErrConflict), errors.Is(err, domain.ErrConflict):
		return fleetError(c, http.StatusConflict, "operation_conflict", "operation conflicts with durable state")
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return fleetError(c, http.StatusNotFound, "probe_not_found", "probe not found")
	default:
		return fleetError(c, http.StatusServiceUnavailable, "operation_unavailable", "operation unavailable")
	}
}

// toProbeOperationView renders the exact receipt shape. Error text is the
// stored bounded redacted diagnostic, never an error chain.
func toProbeOperationView(operation domain.ProbeOperation) ProbeOperationView {
	view := ProbeOperationView{
		OperationID: operation.OperationID, ProbeID: operation.ProbeID,
		Status: operation.Status, Phase: operation.Phase,
		CreatedAt: operation.CreatedAt.UTC(), UpdatedAt: operation.UpdatedAt.UTC(),
	}
	if operation.Error != nil {
		view.Error = &ProbeOperationErrorView{Code: operation.Error.Code, Message: operation.Error.Message}
	}
	return view
}

// readProbeAdminBody bounds request bodies before contract decoding.
func readProbeAdminBody(c echo.Context) ([]byte, error) {
	return io.ReadAll(io.LimitReader(c.Request().Body, 64<<10))
}

// probeAdminJSONFields decodes the body as one JSON object with string keys.
func probeAdminJSONFields(body []byte) (map[string]any, error) {
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("expected json object")
	}
	return fields, nil
}
