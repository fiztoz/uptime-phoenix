package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
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

// Bounds of the M8 activation HTTP contract: a 128 KiB request body and the
// domain's 64 KiB transient private key. Cancel takes no payload at all.
const (
	probeActivationMaxBodyBytes       = 128 << 10
	probeActivationCancelMaxBodyBytes = 1 << 10
	// probeActivationRetryAfterSeconds bounds the Busy admission retry hint.
	probeActivationRetryAfterSeconds = "5"
)

// Frozen response discriminator: this surface only ever describes the
// asynchronous activation of an already installed SSH probe.
const probeActivationKind = "probe_activation"

// probeActivationRunner is the durable asynchronous activation service behind
// the administrative routes. Without it the routes stay registered but 503.
type probeActivationRunner interface {
	Submit(context.Context, services.ProbeActivationSubmission) (*domain.ProbeActivation, error)
	Observe(context.Context, string) (*domain.ProbeActivation, error)
	Cancel(context.Context, string) (*domain.ProbeActivation, error)
}

// ProbeActivationHandlers exposes the M8 asynchronous probe activation routes
// (POST activate, GET activation and cancel) for an install that reached
// resources_ready. Requests are decoded with a strict one-object binder that
// refuses unknown/duplicate/case-variant keys, mistyped values, null, trailing
// JSON and oversized bodies. The install identity comes only from the path and
// the actor only from authenticated context. Receipts are validated and
// rendered as explicit view DTOs; domain types are never serialized and no
// storage/SSH diagnostic, raw request byte, address, fingerprint, stream,
// attempt or owner identifier, enrollment token or private key reaches a
// response.
type ProbeActivationHandlers struct {
	svc     probeActivationRunner
	enabled bool
}

// NewProbeActivationHandlers builds the handlers; a disabled surface or a
// missing service answers with a typed 503 instead of fake success. A typed
// nil service pointer is treated as a missing service.
func NewProbeActivationHandlers(svc probeActivationRunner, enabled bool) *ProbeActivationHandlers {
	if svc != nil {
		if value := reflect.ValueOf(svc); value.Kind() == reflect.Pointer && value.IsNil() {
			svc = nil
		}
	}
	return &ProbeActivationHandlers{svc: svc, enabled: enabled}
}

// ProbeActivationView is the exact response shape. Status is the activation
// state; active means the probe was initialized, registered, started and
// enrolled, never that its configuration was applied. ProbeID is the PUBLIC
// probe identity, null until probe_init has applied. Selectors, addresses,
// fingerprints, stream/actor/owner/attempt IDs, step references, the
// enrollment token, private keys and raw output are deliberately absent.
type ProbeActivationView struct {
	OperationID string                    `json:"operation_id"`
	Kind        string                    `json:"kind"`
	Status      string                    `json:"status"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
	DeadlineAt  time.Time                 `json:"deadline_at"`
	ProbeKey    string                    `json:"probe_key"`
	Name        string                    `json:"name"`
	Location    string                    `json:"location"`
	ListenPort  int                       `json:"listen_port"`
	ProbeID     *string                   `json:"probe_id"`
	Steps       []ProbeActivationStepView `json:"steps"`
	// FailedPhase is the first step whose attempt did not apply, set only for
	// a failed, canceled or interrupted activation; null otherwise.
	FailedPhase *string                   `json:"failed_phase"`
	Error       *ProbeActivationErrorView `json:"error"`
}

// ProbeActivationStepView is one ordered step report: a fixed kind, its state
// and a nullable allowlisted machine code. References stay server-side.
type ProbeActivationStepView struct {
	Kind      string  `json:"kind"`
	State     string  `json:"state"`
	ErrorCode *string `json:"error_code"`
	// StartedAt and FinishedAt are the DB-clock attempt times, null until set.
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// ProbeActivationErrorView carries only a fixed allowlisted machine code and
// its fixed message; it is never an error chain or remote output.
type ProbeActivationErrorView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// probeActivationStoredErrorMessages is the fixed operation error allowlist.
// It covers every code the activation journal can store for a failed,
// canceled or interrupted activation: the installer's outcome codes
// (domain.ProbeInstallOutcome.Valid, which FinishActivation shares) plus the
// storage transitions deadline_expired and outcome_unknown. A stored code
// outside it fails closed with 503 instead of serializing free text.
var probeActivationStoredErrorMessages = map[string]string{
	"unsupported_host":   "host is not supported for activation",
	"artifact_rejected":  "installed probe artifact was rejected",
	"resource_conflict":  "an activation step conflicts with existing state",
	"execution_failed":   "an activation step failed",
	"permission_denied":  "a required activation action was denied",
	"runtime_unverified": "probe runtime could not be verified",
	"not_started":        "no activation step was started",
	"internal":           "activation failed",
	"canceled":           "activation canceled",
	"owner_interrupted":  "activation interrupted by its owner",
	"deadline_expired":   "activation exceeded its deadline",
	"outcome_unknown":    "an activation step outcome is unknown",
}

// probeActivationStepErrorCodes is the allowlist of per-step machine codes the
// journal can store (see domain.ProbeActivation.Valid).
var probeActivationStepErrorCodes = map[string]struct{}{
	"not_started":       {},
	"outcome_unknown":   {},
	"deadline_expired":  {},
	"owner_interrupted": {},
}

// probeActivationStepKinds is the activation step set this surface renders.
var probeActivationStepKinds = map[string]struct{}{
	domain.ProbeActivationProbeInit:    {},
	domain.ProbeActivationRegistration: {},
	domain.ProbeActivationServiceStart: {},
	domain.ProbeActivationEnrollment:   {},
}

// Submit handles POST /api/probe-installs/:operation_id/activate. A persisted
// nonterminal receipt answers 202 only after the service committed it; an exact
// retry of a settled receipt answers 200 with the stored outcome and no new
// dispatch.
func (h *ProbeActivationHandlers) Submit(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, probeActivationMaxBodyBytes+1))
	defer zeroInstallBuffer(raw)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_activation", "invalid activation submission")
	}
	if len(raw) > probeActivationMaxBodyBytes {
		return fleetError(c, http.StatusRequestEntityTooLarge, "activation_too_large", "request body exceeds the activation limit")
	}
	request, err := decodeProbeActivationSubmission(raw)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_activation", "invalid activation submission")
	}
	userID, _ := userIDFromContext(c)
	privateKey := []byte(request.PrivateKey)
	defer zeroInstallBuffer(privateKey)
	// Malformed key, identity or port input refuses here, before any service
	// call, mirroring the invariants the service re-checks.
	if len(privateKey) < 1 || len(privateKey) > domain.MaxProbeInstallPrivateKeySize || !probeActivationRequestValid(request, userID) {
		return fleetError(c, http.StatusBadRequest, "invalid_activation", "invalid activation submission")
	}
	// Validity already bounds the port; the explicit range keeps the
	// conversion visibly lossless.
	port := 0
	if request.ListenPort > 0 && request.ListenPort <= 65535 {
		port = int(request.ListenPort)
	}
	activation, err := h.svc.Submit(c.Request().Context(), services.ProbeActivationSubmission{
		OperationID: operationID,
		PrivateKey:  privateKey,
		ProbeKey:    request.ProbeKey,
		Name:        request.Name,
		Location:    request.Location,
		ListenPort:  port,
		RequestedBy: userID,
	})
	if err != nil {
		return probeActivationServiceError(c, err)
	}
	view, err := toProbeActivationView(activation, operationID)
	if err != nil {
		return probeActivationUnavailableError(c)
	}
	if activation.Terminal() {
		return c.JSON(http.StatusOK, view)
	}
	return c.JSON(http.StatusAccepted, view)
}

// Observe handles GET /api/probe-installs/:operation_id/activation. It
// reconciles hard-expired rows server-side and never creates work; an existing
// receipt always answers 200, terminal included.
func (h *ProbeActivationHandlers) Observe(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	activation, err := h.svc.Observe(c.Request().Context(), operationID)
	if err != nil {
		return probeActivationServiceError(c, err)
	}
	view, err := toProbeActivationView(activation, operationID)
	if err != nil {
		return probeActivationUnavailableError(c)
	}
	return c.JSON(http.StatusOK, view)
}

// Cancel handles POST /api/probe-installs/:operation_id/activation/cancel and
// takes no request payload. It persists cancellation intent and never claims
// remote work stopped: 202 for cancel_requested, 200 for an already settled
// receipt, which is returned unchanged.
func (h *ProbeActivationHandlers) Cancel(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, probeActivationCancelMaxBodyBytes+1))
	if err != nil || len(body) > probeActivationCancelMaxBodyBytes || len(bytes.TrimSpace(body)) > 0 {
		return fleetError(c, http.StatusBadRequest, "invalid_request", "cancel takes no request payload")
	}
	activation, err := h.svc.Cancel(c.Request().Context(), operationID)
	if err != nil {
		return probeActivationServiceError(c, err)
	}
	view, err := toProbeActivationView(activation, operationID)
	if err != nil {
		return probeActivationUnavailableError(c)
	}
	if activation.Terminal() {
		return c.JSON(http.StatusOK, view)
	}
	if activation.State == domain.ProbeActivationCancelRequested {
		return c.JSON(http.StatusAccepted, view)
	}
	// A queued or running receipt after a cancel request contradicts the
	// cancellation contract; fail closed instead of inventing progress.
	return probeActivationUnavailableError(c)
}

// prepare gates every route: no-store caching, authenticated positive user
// context, the opt-in feature flag, then service availability.
func (h *ProbeActivationHandlers) prepare(c echo.Context) bool {
	c.Response().Header().Set("Cache-Control", "no-store")
	userID, ok := userIDFromContext(c)
	if !ok || userID <= 0 {
		_ = fleetError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return false
	}
	if !h.enabled {
		_ = fleetError(c, http.StatusServiceUnavailable, "activations_disabled", "SSH probe activation is disabled")
		return false
	}
	if h.svc == nil {
		_ = fleetError(c, http.StatusServiceUnavailable, "activation_unavailable", "activation operations unavailable")
		return false
	}
	return true
}

// probeActivationSubmitRequest is the strict decoded request: four required
// JSON strings, including the transient private key, and one required integer.
// The install identity is deliberately not a member; it comes from the path.
type probeActivationSubmitRequest struct {
	PrivateKey string
	ProbeKey   string
	Name       string
	Location   string
	ListenPort int64
}

var errInvalidActivationBody = errors.New("invalid activation submission body")

// probeActivationStringFields lists the required string members; listen_port
// is the single required integer member. Empty strings are allowed to decode
// (location may be empty); their validity is a domain rule.
var probeActivationStringFields = map[string]struct{}{
	"private_key": {},
	"probe_key":   {},
	"name":        {},
	"location":    {},
}

const (
	probeActivationPortField = "listen_port"
	// probeActivationMemberCount is the five required members.
	probeActivationMemberCount = 5
)

// decodeProbeActivationSubmission accepts exactly one JSON object with exactly
// the five required members and nothing else. Unknown keys (including case
// variants and a body-supplied operation_id or actor), duplicate keys,
// mistyped or null values, missing members, a listen_port that is not a plain
// non-negative base-10 integer literal (sign, fraction, exponent and leading
// zero forms refuse) and any trailing JSON all refuse. Input validity beyond
// shape is enforced by probeActivationRequestValid and re-checked by the
// service before any dispatch.
func decodeProbeActivationSubmission(raw []byte) (probeActivationSubmitRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return probeActivationSubmitRequest{}, errInvalidActivationBody
	}
	values := make(map[string]string, len(probeActivationStringFields))
	seen := make(map[string]bool, probeActivationMemberCount)
	var port int64
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return probeActivationSubmitRequest{}, errInvalidActivationBody
		}
		key, ok := keyToken.(string)
		if !ok {
			return probeActivationSubmitRequest{}, errInvalidActivationBody
		}
		_, isString := probeActivationStringFields[key]
		if !isString && key != probeActivationPortField {
			// Unknown keys and every case variant of a known key refuse here.
			return probeActivationSubmitRequest{}, errInvalidActivationBody
		}
		if seen[key] {
			return probeActivationSubmitRequest{}, errInvalidActivationBody
		}
		seen[key] = true
		valueToken, err := decoder.Token()
		if err != nil {
			return probeActivationSubmitRequest{}, errInvalidActivationBody
		}
		if isString {
			value, ok := valueToken.(string)
			if !ok {
				// Rejects null, numbers, booleans, objects and arrays.
				return probeActivationSubmitRequest{}, errInvalidActivationBody
			}
			values[key] = value
			continue
		}
		number, ok := valueToken.(json.Number)
		if !ok {
			// Rejects null, strings, booleans, objects and arrays.
			return probeActivationSubmitRequest{}, errInvalidActivationBody
		}
		parsed, ok := parseProbeInstallInteger(number.String())
		if !ok {
			return probeActivationSubmitRequest{}, errInvalidActivationBody
		}
		port = parsed
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return probeActivationSubmitRequest{}, errInvalidActivationBody
	}
	if _, err := decoder.Token(); err != io.EOF {
		// Trailing JSON (or trailing garbage) refuses.
		return probeActivationSubmitRequest{}, errInvalidActivationBody
	}
	if len(seen) != probeActivationMemberCount {
		// The allowlist plus the duplicate check make five seen keys exactly
		// the five required members.
		return probeActivationSubmitRequest{}, errInvalidActivationBody
	}
	return probeActivationSubmitRequest{
		PrivateKey: values["private_key"],
		ProbeKey:   values["probe_key"],
		Name:       values["name"],
		Location:   values["location"],
		ListenPort: port,
	}, nil
}

// probeActivationRequestValid checks the request-controlled fields against the
// domain's own ProbeActivationIdentity rules rather than a drifting copy. The
// install half of the identity is a fixed synthetic, nonsecret placeholder
// (the real host, pin and client key come from the stored install), so only
// probe_key, name, location, listen_port and the actor can decide the result.
// Port 0 means the documented default.
func probeActivationRequestValid(request probeActivationSubmitRequest, actor int64) bool {
	if request.ListenPort < 0 || request.ListenPort > 65535 {
		return false
	}
	port := int(request.ListenPort)
	if port == 0 {
		port = domain.ProbeActivationDefaultPort
	}
	identity := probeActivationTemplate()
	identity.ProbeKey = request.ProbeKey
	identity.Name = request.Name
	identity.Location = request.Location
	identity.ListenPort = port
	identity.RequestedBy = actor
	return identity.Valid()
}

// probeActivationTemplate returns a domain-valid placeholder identity whose
// install half never reaches a service or a response.
func probeActivationTemplate() domain.ProbeActivationIdentity {
	pin := "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	return domain.ProbeActivationIdentity{
		Install: domain.ProbeInstallIdentity{
			OperationID:          "00000000-0000-4000-8000-000000000001",
			Host:                 domain.ProbeSSHHost{Address: "placeholder.invalid:22", User: "root"},
			HostKeyFingerprint:   pin,
			ClientKeyFingerprint: pin,
			RequestedBy:          1,
			LayoutVersion:        domain.ProbeInstallLayoutVersion,
			Mode:                 domain.ProbeInstallSystemd,
			Binary: domain.ProbeBinaryPin{
				Version:      "0.0.0",
				Architecture: "amd64",
				SHA256:       "0000000000000000000000000000000000000000000000000000000000000000",
				Size:         1,
			},
		},
		ClientKeyFingerprint: pin,
		RequestedBy:          1,
	}
}

// toProbeActivationView validates a complete stored receipt independently of
// the service, then renders the DTO. A nil, corrupt, mismatched or
// non-allowlisted receipt fails closed; nothing partial is ever serialized.
func toProbeActivationView(activation *domain.ProbeActivation, wantOperationID string) (ProbeActivationView, error) {
	var view ProbeActivationView
	if activation == nil || !activation.Valid() || activation.Identity.Install.OperationID != wantOperationID {
		return view, errors.New("invalid activation receipt")
	}
	switch activation.State {
	case domain.ProbeActivationQueued, domain.ProbeActivationRunning, domain.ProbeActivationCancelRequested,
		domain.ProbeActivationActive, domain.ProbeActivationFailed, domain.ProbeActivationCanceled,
		domain.ProbeActivationInterrupted:
	default:
		return view, errors.New("non-allowlisted activation state")
	}
	message, ok := probeActivationStoredErrorMessages[activation.ErrorCode]
	if activation.ErrorCode != "" && !ok {
		return view, errors.New("non-allowlisted activation error code")
	}
	steps := make([]ProbeActivationStepView, 0, len(activation.Steps))
	for _, step := range activation.Steps {
		if _, ok := probeActivationStepKinds[step.Spec.Kind]; !ok {
			return view, errors.New("non-allowlisted activation step kind")
		}
		switch step.State {
		case domain.ProbeInstallResourcePlanned, domain.ProbeInstallResourceInFlight, domain.ProbeInstallResourceApplied,
			domain.ProbeInstallResourceNotStarted, domain.ProbeInstallResourceUnknown:
		default:
			return view, errors.New("non-allowlisted activation step state")
		}
		item := ProbeActivationStepView{Kind: step.Spec.Kind, State: step.State}
		item.StartedAt, item.FinishedAt = probeStepTimes(step)
		if step.ErrorCode != "" {
			if _, ok := probeActivationStepErrorCodes[step.ErrorCode]; !ok {
				return view, errors.New("non-allowlisted activation step error code")
			}
			code := step.ErrorCode
			item.ErrorCode = &code
		}
		steps = append(steps, item)
	}
	view = ProbeActivationView{
		OperationID: activation.Identity.Install.OperationID,
		Kind:        probeActivationKind,
		Status:      activation.State,
		CreatedAt:   activation.CreatedAt.UTC(),
		UpdatedAt:   activation.UpdatedAt.UTC(),
		DeadlineAt:  activation.DeadlineAt.UTC(),
		ProbeKey:    activation.Identity.ProbeKey,
		Name:        activation.Identity.Name,
		Location:    activation.Identity.Location,
		ListenPort:  activation.Identity.ListenPort,
		Steps:       steps,
	}
	switch activation.State {
	case domain.ProbeActivationFailed, domain.ProbeActivationCanceled, domain.ProbeActivationInterrupted:
		view.FailedPhase = probeFailedPhase(activation.Steps)
	}
	// Valid binds the facts to probe_init applied, so a valid receipt carries
	// the public probe identity exactly once the host reported it.
	if activation.Facts.Valid() {
		probeID := activation.Facts.ProbeID
		view.ProbeID = &probeID
	}
	if activation.ErrorCode != "" {
		view.Error = &ProbeActivationErrorView{Code: activation.ErrorCode, Message: message}
	}
	return view, nil
}

// probeActivationServiceError maps the service sentinel set onto the HTTP
// contract. The underlying error text is never echoed: only these fixed codes
// and messages leave the handler.
func probeActivationServiceError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrInternal):
		// Repository validation after validated admission means corrupt
		// server state, not malformed client input (even in a joined cause).
		return probeActivationUnavailableError(c)
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrProbeSSHInput):
		return fleetError(c, http.StatusBadRequest, "invalid_activation", "invalid activation submission")
	case errors.Is(err, ports.ErrConflict), errors.Is(err, domain.ErrConflict):
		return fleetError(c, http.StatusConflict, "activation_conflict", "activation conflicts with the install state or a different existing request")
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return fleetError(c, http.StatusNotFound, "activation_not_found", "install or activation not found")
	case errors.Is(err, domain.ErrProbeInstallBusy):
		c.Response().Header().Set(echo.HeaderRetryAfter, probeActivationRetryAfterSeconds)
		return fleetError(c, http.StatusServiceUnavailable, "activations_busy", "SSH activation capacity exhausted")
	case errors.Is(err, domain.ErrProbeInstallClosed):
		return fleetError(c, http.StatusServiceUnavailable, "activations_closed", "SSH activation executor is stopped")
	default:
		return probeActivationUnavailableError(c)
	}
}

// probeActivationUnavailableError is the single closed-fail 503 for disabled
// internals, corrupt receipts and unexpected service failures.
func probeActivationUnavailableError(c echo.Context) error {
	return fleetError(c, http.StatusServiceUnavailable, "activation_unavailable", "activation operations unavailable")
}
