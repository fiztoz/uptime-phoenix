package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Bounds frozen by the M8 HTTP contract: a 128 KiB request body and a 64 KiB
// transient private key. Cancel takes no payload at all.
const (
	probePreflightMaxBodyBytes       = 128 << 10
	probePreflightMaxPrivateKeyBytes = 64 << 10
	probePreflightCancelMaxBodyBytes = 1 << 10
	// probePreflightRetryAfterSeconds bounds the Busy admission retry hint.
	probePreflightRetryAfterSeconds = "5"
)

// Frozen response discriminators: this surface only ever describes one kind of
// discovery work, never registration, installation or activation.
const (
	probePreflightKind  = "ssh_host_preflight"
	probePreflightPhase = "host_preflight"
)

// probePreflightRunner is the durable asynchronous preflight service behind
// the administrative routes. Without it the routes stay registered but 503.
type probePreflightRunner interface {
	Submit(context.Context, domain.ProbePreflightSubmission) (*domain.ProbePreflightOperation, error)
	Observe(context.Context, string) (*domain.ProbePreflightOperation, error)
	Cancel(context.Context, string) (*domain.ProbePreflightOperation, error)
}

// ProbePreflightHandlers exposes the frozen M8 asynchronous SSH host preflight
// routes (POST, GET and cancel). Requests are decoded with a strict one-object
// binder that refuses unknown/duplicate/case-variant keys, non-string values,
// null, trailing JSON and oversized bodies, so the accepted wire shape cannot
// drift from the contract and a server-local key path can never be smuggled
// in as an alternative trust input. The actor comes only from authenticated
// context. Receipts are validated and rendered as explicit view DTOs; domain
// types are never serialized and no underlying storage/SSH diagnostic, raw
// request byte or private key ever reaches a response or a log line.
type ProbePreflightHandlers struct {
	svc     probePreflightRunner
	enabled bool
}

// NewProbePreflightHandlers builds the handlers; a disabled surface or a
// missing service answers with a typed 503 instead of fake success.
func NewProbePreflightHandlers(svc probePreflightRunner, enabled bool) *ProbePreflightHandlers {
	return &ProbePreflightHandlers{svc: svc, enabled: enabled}
}

// ProbePreflightView is the exact response shape: operation_id, kind, status,
// phase, UTC created_at/updated_at/deadline_at, nullable error and nullable
// preflight compatibility facts. Selectors, actor/owner IDs, fingerprints,
// private keys and raw output are deliberately absent.
type ProbePreflightView struct {
	OperationID string                    `json:"operation_id"`
	Kind        string                    `json:"kind"`
	Status      string                    `json:"status"`
	Phase       string                    `json:"phase"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
	DeadlineAt  time.Time                 `json:"deadline_at"`
	Error       *ProbePreflightErrorView  `json:"error"`
	Preflight   *ProbePreflightResultView `json:"preflight"`
}

// ProbePreflightErrorView carries only a fixed allowlisted machine code and
// its fixed message; it is never an error chain or remote output.
type ProbePreflightErrorView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ProbePreflightResultView carries only bounded discovery facts. DockerCLI
// means the executable exists, not that a daemon is reachable.
type ProbePreflightResultView struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	DockerCLI    bool   `json:"docker_cli"`
	Systemd      bool   `json:"systemd"`
	Privilege    string `json:"privilege"`
}

// probePreflightStoredErrorMessages is the fixed error allowlist: a stored
// code outside it fails closed with 503 instead of serializing free text.
var probePreflightStoredErrorMessages = map[string]string{
	"ssh_trust":            "host identity verification failed",
	"ssh_destination":      "SSH destination rejected by policy",
	"ssh_connect":          "SSH connection or authentication failed",
	"ssh_discovery":        "SSH host discovery failed",
	"unsupported_platform": "unsupported host OS or architecture",
	"missing_privilege":    "host requires root or noninteractive sudo",
	"missing_runtime":      "host requires systemd or an existing Docker CLI",
	"timeout":              "preflight exceeded its deadline",
	"internal":             "preflight failed",
	"canceled":             "preflight canceled",
	"interrupted":          "preflight interrupted",
}

// Submit handles POST /api/probe-host-preflights. A persisted nonterminal
// receipt answers 202 only after the service committed it; an exact terminal
// retry answers 200 with the original immutable outcome and no new dispatch.
func (h *ProbePreflightHandlers) Submit(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, probePreflightMaxBodyBytes+1))
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_preflight", "invalid preflight submission")
	}
	defer zeroPreflightBuffer(raw)
	if len(raw) > probePreflightMaxBodyBytes {
		return fleetError(c, http.StatusRequestEntityTooLarge, "preflight_too_large", "request body exceeds the preflight limit")
	}
	request, err := decodeProbePreflightSubmission(raw)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_preflight", "invalid preflight submission")
	}
	userID, _ := userIDFromContext(c)
	privateKey := []byte(request.PrivateKey)
	defer zeroPreflightBuffer(privateKey)
	operation, err := h.svc.Submit(c.Request().Context(), domain.ProbePreflightSubmission{
		OperationID: request.OperationID,
		Host: domain.ProbeSSHHost{
			Address: request.SSHAddress,
			User:    request.SSHUser,
		},
		HostKeyFingerprint: request.HostKeyFingerprint,
		RequestedBy:        userID,
		PrivateKey:         privateKey,
	})
	if err != nil {
		return probePreflightServiceError(c, err)
	}
	view, err := toProbePreflightView(operation)
	if err != nil {
		return probePreflightUnavailableError(c)
	}
	if operation.Terminal() {
		return c.JSON(http.StatusOK, view)
	}
	return c.JSON(http.StatusAccepted, view)
}

// Observe handles GET /api/probe-host-preflights/:operation_id. It atomically
// reconciles hard-expired rows to interrupted server-side and never creates
// work; an existing receipt always answers 200, terminal included.
func (h *ProbePreflightHandlers) Observe(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	operation, err := h.svc.Observe(c.Request().Context(), operationID)
	if err != nil {
		return probePreflightServiceError(c, err)
	}
	view, err := toProbePreflightView(operation)
	if err != nil {
		return probePreflightUnavailableError(c)
	}
	return c.JSON(http.StatusOK, view)
}

// Cancel handles POST /api/probe-host-preflights/:operation_id/cancel and
// takes no request payload. It persists cancellation intent: 202 for
// cancel_requested, 200 for an already terminal receipt, which is returned
// unchanged — a completed preflight is never relabeled canceled.
func (h *ProbePreflightHandlers) Cancel(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, probePreflightCancelMaxBodyBytes))
	if err != nil || len(bytes.TrimSpace(body)) > 0 {
		return fleetError(c, http.StatusBadRequest, "invalid_request", "cancel takes no request payload")
	}
	operation, err := h.svc.Cancel(c.Request().Context(), operationID)
	if err != nil {
		return probePreflightServiceError(c, err)
	}
	view, err := toProbePreflightView(operation)
	if err != nil {
		return probePreflightUnavailableError(c)
	}
	if operation.Terminal() {
		return c.JSON(http.StatusOK, view)
	}
	if operation.State == domain.ProbePreflightCancelRequested {
		return c.JSON(http.StatusAccepted, view)
	}
	// A nonterminal receipt that is not cancel_requested contradicts the
	// cancellation contract; fail closed instead of inventing progress.
	return probePreflightUnavailableError(c)
}

// prepare gates every route: no-store caching, authenticated positive user
// context, the opt-in feature flag, then service availability.
func (h *ProbePreflightHandlers) prepare(c echo.Context) bool {
	c.Response().Header().Set("Cache-Control", "no-store")
	userID, ok := userIDFromContext(c)
	if !ok || userID <= 0 {
		_ = fleetError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return false
	}
	if !h.enabled {
		_ = fleetError(c, http.StatusServiceUnavailable, "preflights_disabled", "SSH host preflights are disabled")
		return false
	}
	if h.svc == nil {
		_ = fleetError(c, http.StatusServiceUnavailable, "preflight_unavailable", "preflight operations unavailable")
		return false
	}
	return true
}

// probePreflightSubmitRequest is the strict decoded request; every field is a
// required JSON string, including the transient private key.
type probePreflightSubmitRequest struct {
	OperationID        string
	SSHAddress         string
	SSHUser            string
	HostKeyFingerprint string
	PrivateKey         string
}

var errInvalidPreflightBody = errors.New("invalid preflight submission body")

// decodeProbePreflightSubmission accepts exactly one JSON object with exactly
// the five required string members and nothing else. Unknown keys (including
// case variants), duplicate keys, non-string or null values, missing members
// and any trailing JSON all refuse. Input validity beyond shape (host syntax,
// fingerprint canonical form, key parseability) is enforced by the handler's
// domain checks and re-checked by the service before any dispatch.
func decodeProbePreflightSubmission(raw []byte) (probePreflightSubmitRequest, error) {
	var request probePreflightSubmitRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return request, errInvalidPreflightBody
	}
	values := make(map[string]string, 5)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return request, errInvalidPreflightBody
		}
		key, ok := keyToken.(string)
		if !ok {
			return request, errInvalidPreflightBody
		}
		switch key {
		case "operation_id", "ssh_address", "ssh_user", "host_key_fingerprint", "private_key":
		default:
			// Unknown keys and every case variant of a known key refuse here.
			return request, errInvalidPreflightBody
		}
		if _, duplicate := values[key]; duplicate {
			return request, errInvalidPreflightBody
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return request, errInvalidPreflightBody
		}
		value, ok := valueToken.(string)
		if !ok {
			// Rejects null, numbers, booleans, objects and arrays.
			return request, errInvalidPreflightBody
		}
		values[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return request, errInvalidPreflightBody
	}
	if _, err := decoder.Token(); err != io.EOF {
		// Trailing JSON (or trailing garbage) refuses.
		return request, errInvalidPreflightBody
	}
	if len(values) != 5 {
		// A missing required member cannot survive the allowlist plus the
		// duplicate check, so five seen keys means all five are present.
		return request, errInvalidPreflightBody
	}
	request = probePreflightSubmitRequest{
		OperationID:        values["operation_id"],
		SSHAddress:         values["ssh_address"],
		SSHUser:            values["ssh_user"],
		HostKeyFingerprint: values["host_key_fingerprint"],
		PrivateKey:         values["private_key"],
	}
	// Malformed identity, destination or trust input refuses here, before any
	// service call, mirroring the domain invariants the service re-checks.
	if !domain.ValidProbeOperationID(request.OperationID) ||
		!(domain.ProbeSSHHost{Address: request.SSHAddress, User: request.SSHUser}).Valid() ||
		!domain.ValidSSHKeyFingerprint(request.HostKeyFingerprint) ||
		len(request.PrivateKey) == 0 || len(request.PrivateKey) > probePreflightMaxPrivateKeyBytes {
		return probePreflightSubmitRequest{}, errInvalidPreflightBody
	}
	return request, nil
}

// toProbePreflightView validates a complete stored receipt independently of
// the service, then renders the frozen DTO. A nil, corrupt or non-allowlisted
// receipt fails closed; nothing partial is ever serialized.
func toProbePreflightView(operation *domain.ProbePreflightOperation) (ProbePreflightView, error) {
	var view ProbePreflightView
	if operation == nil || !operation.Valid() {
		return view, errors.New("invalid preflight receipt")
	}
	message, ok := probePreflightStoredErrorMessages[operation.ErrorCode]
	if operation.ErrorCode != "" && !ok {
		return view, errors.New("non-allowlisted preflight error code")
	}
	view = ProbePreflightView{
		OperationID: operation.Identity.OperationID,
		Kind:        probePreflightKind,
		Status:      operation.State,
		Phase:       probePreflightPhase,
		CreatedAt:   operation.CreatedAt.UTC(),
		UpdatedAt:   operation.UpdatedAt.UTC(),
		DeadlineAt:  operation.DeadlineAt.UTC(),
	}
	if operation.ErrorCode != "" {
		view.Error = &ProbePreflightErrorView{Code: operation.ErrorCode, Message: message}
	}
	if operation.Result != nil {
		view.Preflight = &ProbePreflightResultView{
			OS:           operation.Result.OS,
			Architecture: operation.Result.Architecture,
			DockerCLI:    operation.Result.DockerCLI,
			Systemd:      operation.Result.Systemd,
			Privilege:    operation.Result.Privilege,
		}
	}
	return view, nil
}

// probePreflightServiceError maps the frozen service sentinel set onto the
// frozen HTTP contract. The underlying error text is never echoed: only these
// fixed codes and messages leave the handler.
func probePreflightServiceError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrInternal):
		// Repository validation after validated admission means corrupt
		// server state, not malformed client input (even in a joined cause).
		return probePreflightUnavailableError(c)
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrProbeSSHInput):
		return fleetError(c, http.StatusBadRequest, "invalid_preflight", "invalid preflight submission")
	case errors.Is(err, ports.ErrConflict), errors.Is(err, domain.ErrConflict):
		return fleetError(c, http.StatusConflict, "preflight_conflict", "operation identity conflicts with a different existing request")
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return fleetError(c, http.StatusNotFound, "preflight_not_found", "preflight operation not found")
	case errors.Is(err, domain.ErrProbePreflightBusy):
		c.Response().Header().Set(echo.HeaderRetryAfter, probePreflightRetryAfterSeconds)
		return fleetError(c, http.StatusServiceUnavailable, "preflights_busy", "SSH preflight capacity exhausted")
	case errors.Is(err, domain.ErrProbePreflightClosed):
		return fleetError(c, http.StatusServiceUnavailable, "preflights_closed", "SSH preflight executor is stopped")
	default:
		return probePreflightUnavailableError(c)
	}
}

// probePreflightUnavailableError is the single closed-fail 503 for disabled
// internals, corrupt receipts and unexpected service failures.
func probePreflightUnavailableError(c echo.Context) error {
	return fleetError(c, http.StatusServiceUnavailable, "preflight_unavailable", "preflight operations unavailable")
}

// zeroPreflightBuffer clears owned transient buffers (raw body, private key)
// on a best-effort basis once the response is decided. Strings decoded by
// encoding/json are immutable and cannot be cleared; only owned bytes are.
func zeroPreflightBuffer(buffer []byte) {
	for i := range buffer {
		buffer[i] = 0
	}
}
