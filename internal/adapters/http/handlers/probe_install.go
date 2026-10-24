package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Bounds frozen by the M8 installer HTTP contract: a 128 KiB request body and
// the domain's 64 KiB transient private key. Cancel takes no payload at all.
const (
	probeInstallMaxBodyBytes       = 128 << 10
	probeInstallCancelMaxBodyBytes = 1 << 10
	// probeInstallRetryAfterSeconds bounds the Busy admission retry hint.
	probeInstallRetryAfterSeconds = "5"
)

// Frozen response discriminator: this surface only ever describes the
// asynchronous SSH probe installer, never enrollment, activation or success.
const probeInstallKind = "ssh_probe_install"

// probeInstallRunner is the durable asynchronous installer service behind the
// administrative routes. Without it the routes stay registered but 503.
type probeInstallRunner interface {
	Submit(context.Context, domain.ProbeInstallSubmission) (*domain.ProbeInstallOperation, error)
	Observe(context.Context, string) (*domain.ProbeInstallOperation, error)
	Cancel(context.Context, string) (*domain.ProbeInstallOperation, error)
}

// ProbeInstallHandlers exposes the frozen M8 asynchronous SSH probe installer
// routes (POST, GET and cancel). Requests are decoded with a strict one-object
// binder that refuses unknown/duplicate/case-variant keys, mistyped values,
// null, trailing JSON and oversized bodies, so the accepted wire shape cannot
// drift from the contract. The actor comes only from authenticated context.
// Receipts are validated and rendered as explicit view DTOs; domain types are
// never serialized and no storage/SSH diagnostic, raw request byte, address,
// fingerprint, attempt or owner identifier or private key reaches a response.
type ProbeInstallHandlers struct {
	svc     probeInstallRunner
	enabled bool
}

// NewProbeInstallHandlers builds the handlers; a disabled surface or a missing
// service answers with a typed 503 instead of fake success. A typed nil
// service pointer is treated as a missing service.
func NewProbeInstallHandlers(svc probeInstallRunner, enabled bool) *ProbeInstallHandlers {
	if svc != nil {
		if value := reflect.ValueOf(svc); value.Kind() == reflect.Pointer && value.IsNil() {
			svc = nil
		}
	}
	return &ProbeInstallHandlers{svc: svc, enabled: enabled}
}

// ProbeInstallView is the exact response shape. Selectors, fingerprints,
// actor/owner/attempt IDs, resource references, private keys and raw output
// are deliberately absent. Status is the journal state; resources_ready means
// every systemd resource applied, never that a probe is installed or running.
type ProbeInstallView struct {
	OperationID string                     `json:"operation_id"`
	Kind        string                     `json:"kind"`
	Mode        string                     `json:"mode"`
	Status      string                     `json:"status"`
	CreatedAt   time.Time                  `json:"created_at"`
	UpdatedAt   time.Time                  `json:"updated_at"`
	DeadlineAt  time.Time                  `json:"deadline_at"`
	Binary      ProbeInstallBinaryView     `json:"binary"`
	Resources   []ProbeInstallResourceView `json:"resources"`
	// FailedPhase is the first resource kind whose action did not apply
	// (not_started or unknown), set only for a failed, canceled or
	// interrupted install; null otherwise.
	FailedPhase *string                `json:"failed_phase"`
	Error       *ProbeInstallErrorView `json:"error"`
}

// ProbeInstallBinaryView names the operator pin without its digest or size.
type ProbeInstallBinaryView struct {
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
}

// ProbeInstallResourceView is one ordered resource report: a fixed kind, its
// state and a nullable allowlisted machine code. References stay server-side.
type ProbeInstallResourceView struct {
	Kind      string  `json:"kind"`
	State     string  `json:"state"`
	ErrorCode *string `json:"error_code"`
	// StartedAt and FinishedAt are the journal's DB-clock attempt times:
	// null until the attempt was reserved, and until it settled.
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// ProbeInstallErrorView carries only a fixed allowlisted machine code and its
// fixed message; it is never an error chain or remote output.
type ProbeInstallErrorView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// probeInstallStoredErrorMessages is the fixed operation error allowlist. It
// covers every code the journal can store for a failed, canceled or
// interrupted operation (domain.ProbeInstallOutcome.Valid plus the storage
// transitions deadline_expired and outcome_unknown). A stored code outside it
// fails closed with 503 instead of serializing free text.
var probeInstallStoredErrorMessages = map[string]string{
	"unsupported_host":   "host is not supported for installation",
	"artifact_rejected":  "pinned probe artifact was rejected",
	"resource_conflict":  "an install resource conflicts with existing host state",
	"execution_failed":   "an install action failed on the host",
	"permission_denied":  "the host denied a required install action",
	"runtime_unverified": "host runtime could not be verified",
	"not_started":        "no host action was started",
	"internal":           "install failed",
	"canceled":           "install canceled",
	"owner_interrupted":  "install interrupted by its owner",
	"deadline_expired":   "install exceeded its deadline",
	"outcome_unknown":    "an install action outcome is unknown",
}

// probeInstallResourceErrorCodes is the allowlist of per-resource machine
// codes the journal can store (see domain.ProbeInstallOperation.Valid).
var probeInstallResourceErrorCodes = map[string]struct{}{
	"not_started":       {},
	"outcome_unknown":   {},
	"deadline_expired":  {},
	"owner_interrupted": {},
}

// probeInstallResourceKinds is the systemd resource set this surface renders.
var probeInstallResourceKinds = map[string]struct{}{
	"data_directory":     {},
	"artifact_directory": {},
	"unit_file":          {},
	"service":            {},
}

// Submit handles POST /api/probe-installs. A persisted nonterminal receipt
// answers 202 only after the service committed it; an exact retry of a settled
// receipt answers 200 with the stored outcome and no new dispatch.
func (h *ProbeInstallHandlers) Submit(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, probeInstallMaxBodyBytes+1))
	defer zeroInstallBuffer(raw)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_install", "invalid install submission")
	}
	if len(raw) > probeInstallMaxBodyBytes {
		return fleetError(c, http.StatusRequestEntityTooLarge, "install_too_large", "request body exceeds the install limit")
	}
	request, err := decodeProbeInstallSubmission(raw)
	if err != nil {
		return fleetError(c, http.StatusBadRequest, "invalid_install", "invalid install submission")
	}
	userID, _ := userIDFromContext(c)
	privateKey := []byte(request.PrivateKey)
	defer zeroInstallBuffer(privateKey)
	submission := domain.ProbeInstallSubmission{
		OperationID: request.OperationID,
		Host: domain.ProbeSSHHost{
			Address: request.SSHAddress,
			User:    request.SSHUser,
		},
		HostKeyFingerprint: request.HostKeyFingerprint,
		PrivateKey:         privateKey,
		Binary: domain.ProbeBinaryPin{
			Version:      request.BinaryVersion,
			Architecture: request.BinaryArchitecture,
			SHA256:       request.BinarySHA256,
			Size:         request.BinarySize,
		},
		RequestedBy: userID,
	}
	// Malformed identity, destination, trust, key or pin input refuses here,
	// before any service call, mirroring the invariants the service re-checks.
	if !submission.Valid() {
		return fleetError(c, http.StatusBadRequest, "invalid_install", "invalid install submission")
	}
	operation, err := h.svc.Submit(c.Request().Context(), submission)
	if err != nil {
		return probeInstallServiceError(c, err)
	}
	view, err := toProbeInstallView(operation, request.OperationID)
	if err != nil {
		return probeInstallUnavailableError(c)
	}
	if probeInstallSettled(operation) {
		return c.JSON(http.StatusOK, view)
	}
	return c.JSON(http.StatusAccepted, view)
}

// Observe handles GET /api/probe-installs/:operation_id. It reconciles
// hard-expired rows server-side and never creates work; an existing receipt
// always answers 200, terminal included.
func (h *ProbeInstallHandlers) Observe(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	operation, err := h.svc.Observe(c.Request().Context(), operationID)
	if err != nil {
		return probeInstallServiceError(c, err)
	}
	view, err := toProbeInstallView(operation, operationID)
	if err != nil {
		return probeInstallUnavailableError(c)
	}
	return c.JSON(http.StatusOK, view)
}

// Cancel handles POST /api/probe-installs/:operation_id/cancel and takes no
// request payload. It persists cancellation intent and never claims remote
// work stopped: 202 for cancel_requested, 200 for an already settled receipt,
// which is returned unchanged.
func (h *ProbeInstallHandlers) Cancel(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	operationID := c.Param("operation_id")
	if !domain.ValidProbeOperationID(operationID) {
		return fleetError(c, http.StatusBadRequest, "invalid_operation_id", "invalid operation id")
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, probeInstallCancelMaxBodyBytes+1))
	if err != nil || len(body) > probeInstallCancelMaxBodyBytes || len(bytes.TrimSpace(body)) > 0 {
		return fleetError(c, http.StatusBadRequest, "invalid_request", "cancel takes no request payload")
	}
	operation, err := h.svc.Cancel(c.Request().Context(), operationID)
	if err != nil {
		return probeInstallServiceError(c, err)
	}
	view, err := toProbeInstallView(operation, operationID)
	if err != nil {
		return probeInstallUnavailableError(c)
	}
	if probeInstallSettled(operation) {
		return c.JSON(http.StatusOK, view)
	}
	if operation.State == domain.ProbeInstallCancelRequested {
		return c.JSON(http.StatusAccepted, view)
	}
	// A queued or running receipt after a cancel request contradicts the
	// cancellation contract; fail closed instead of inventing progress.
	return probeInstallUnavailableError(c)
}

// prepare gates every route: no-store caching, authenticated positive user
// context, the opt-in feature flag, then service availability.
func (h *ProbeInstallHandlers) prepare(c echo.Context) bool {
	c.Response().Header().Set("Cache-Control", "no-store")
	userID, ok := userIDFromContext(c)
	if !ok || userID <= 0 {
		_ = fleetError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return false
	}
	if !h.enabled {
		_ = fleetError(c, http.StatusServiceUnavailable, "installs_disabled", "SSH probe installs are disabled")
		return false
	}
	if h.svc == nil {
		_ = fleetError(c, http.StatusServiceUnavailable, "install_unavailable", "install operations unavailable")
		return false
	}
	return true
}

// probeInstallSettled reports a receipt that will never progress by itself:
// the journal's terminal outcomes plus resources_ready, the best outcome. Only
// work that is still queued, running or awaiting its owner is in progress.
func probeInstallSettled(operation *domain.ProbeInstallOperation) bool {
	return operation.Terminal() || operation.State == domain.ProbeInstallResourcesReady
}

// probeInstallSubmitRequest is the strict decoded request: eight required JSON
// strings, including the transient private key, and one required integer.
type probeInstallSubmitRequest struct {
	OperationID        string
	SSHAddress         string
	SSHUser            string
	HostKeyFingerprint string
	PrivateKey         string
	BinaryVersion      string
	BinaryArchitecture string
	BinarySHA256       string
	BinarySize         int64
}

var errInvalidInstallBody = errors.New("invalid install submission body")

// probeInstallStringFields lists the required string members; binary_size is
// the single required integer member.
var probeInstallStringFields = map[string]struct{}{
	"operation_id":         {},
	"ssh_address":          {},
	"ssh_user":             {},
	"host_key_fingerprint": {},
	"private_key":          {},
	"binary_version":       {},
	"binary_architecture":  {},
	"binary_sha256":        {},
}

const (
	probeInstallSizeField = "binary_size"
	// probeInstallMemberCount is the nine required members.
	probeInstallMemberCount = 9
)

// decodeProbeInstallSubmission accepts exactly one JSON object with exactly the
// nine required members and nothing else. Unknown keys (including case
// variants), duplicate keys, mistyped or null values, missing members, a
// binary_size that is not a plain non-negative base-10 integer literal (sign,
// fraction and exponent forms refuse) and any trailing JSON all refuse. Input
// validity beyond shape is enforced by domain.ProbeInstallSubmission.Valid and
// re-checked by the service before any dispatch.
func decodeProbeInstallSubmission(raw []byte) (probeInstallSubmitRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return probeInstallSubmitRequest{}, errInvalidInstallBody
	}
	values := make(map[string]string, len(probeInstallStringFields))
	seen := make(map[string]bool, probeInstallMemberCount)
	var size int64
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return probeInstallSubmitRequest{}, errInvalidInstallBody
		}
		key, ok := keyToken.(string)
		if !ok {
			return probeInstallSubmitRequest{}, errInvalidInstallBody
		}
		_, isString := probeInstallStringFields[key]
		if !isString && key != probeInstallSizeField {
			// Unknown keys and every case variant of a known key refuse here.
			return probeInstallSubmitRequest{}, errInvalidInstallBody
		}
		if seen[key] {
			return probeInstallSubmitRequest{}, errInvalidInstallBody
		}
		seen[key] = true
		valueToken, err := decoder.Token()
		if err != nil {
			return probeInstallSubmitRequest{}, errInvalidInstallBody
		}
		if isString {
			value, ok := valueToken.(string)
			if !ok {
				// Rejects null, numbers, booleans, objects and arrays.
				return probeInstallSubmitRequest{}, errInvalidInstallBody
			}
			values[key] = value
			continue
		}
		number, ok := valueToken.(json.Number)
		if !ok {
			// Rejects null, strings, booleans, objects and arrays.
			return probeInstallSubmitRequest{}, errInvalidInstallBody
		}
		parsed, ok := parseProbeInstallInteger(number.String())
		if !ok {
			return probeInstallSubmitRequest{}, errInvalidInstallBody
		}
		size = parsed
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return probeInstallSubmitRequest{}, errInvalidInstallBody
	}
	if _, err := decoder.Token(); err != io.EOF {
		// Trailing JSON (or trailing garbage) refuses.
		return probeInstallSubmitRequest{}, errInvalidInstallBody
	}
	if len(seen) != probeInstallMemberCount {
		// The allowlist plus the duplicate check make nine seen keys exactly
		// the nine required members.
		return probeInstallSubmitRequest{}, errInvalidInstallBody
	}
	return probeInstallSubmitRequest{
		OperationID:        values["operation_id"],
		SSHAddress:         values["ssh_address"],
		SSHUser:            values["ssh_user"],
		HostKeyFingerprint: values["host_key_fingerprint"],
		PrivateKey:         values["private_key"],
		BinaryVersion:      values["binary_version"],
		BinaryArchitecture: values["binary_architecture"],
		BinarySHA256:       values["binary_sha256"],
		BinarySize:         size,
	}, nil
}

// parseProbeInstallInteger accepts only a plain base-10 digit run that fits in
// int64: no sign, fraction, exponent or leading zero. Zero itself parses and
// is refused later by the pin invariants.
func parseProbeInstallInteger(literal string) (int64, bool) {
	if literal == "" || len(literal) > 19 || (len(literal) > 1 && literal[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(literal); i++ {
		if literal[i] < '0' || literal[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(literal, 10, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// toProbeInstallView validates a complete stored receipt independently of the
// service, then renders the frozen DTO. A nil, corrupt, mismatched,
// non-systemd or non-allowlisted receipt fails closed; nothing partial is ever
// serialized.
func toProbeInstallView(operation *domain.ProbeInstallOperation, wantOperationID string) (ProbeInstallView, error) {
	var view ProbeInstallView
	if operation == nil || !operation.Valid() || operation.Identity.OperationID != wantOperationID {
		return view, errors.New("invalid install receipt")
	}
	if operation.Identity.Mode != domain.ProbeInstallSystemd {
		// The Docker path is not part of this surface.
		return view, errors.New("unsupported install receipt mode")
	}
	switch operation.State {
	case domain.ProbeInstallQueued, domain.ProbeInstallRunning, domain.ProbeInstallCancelRequested,
		domain.ProbeInstallResourcesReady, domain.ProbeInstallFailed, domain.ProbeInstallCanceled,
		domain.ProbeInstallInterrupted:
	default:
		return view, errors.New("non-allowlisted install state")
	}
	message, ok := probeInstallStoredErrorMessages[operation.ErrorCode]
	if operation.ErrorCode != "" && !ok {
		return view, errors.New("non-allowlisted install error code")
	}
	resources := make([]ProbeInstallResourceView, 0, len(operation.Resources))
	for _, resource := range operation.Resources {
		if _, ok := probeInstallResourceKinds[resource.Spec.Kind]; !ok {
			return view, errors.New("non-allowlisted install resource kind")
		}
		switch resource.State {
		case domain.ProbeInstallResourcePlanned, domain.ProbeInstallResourceInFlight, domain.ProbeInstallResourceApplied,
			domain.ProbeInstallResourceNotStarted, domain.ProbeInstallResourceUnknown:
		default:
			return view, errors.New("non-allowlisted install resource state")
		}
		item := ProbeInstallResourceView{Kind: resource.Spec.Kind, State: resource.State}
		item.StartedAt, item.FinishedAt = probeStepTimes(resource)
		if resource.ErrorCode != "" {
			if _, ok := probeInstallResourceErrorCodes[resource.ErrorCode]; !ok {
				return view, errors.New("non-allowlisted install resource error code")
			}
			code := resource.ErrorCode
			item.ErrorCode = &code
		}
		resources = append(resources, item)
	}
	view = ProbeInstallView{
		OperationID: operation.Identity.OperationID,
		Kind:        probeInstallKind,
		Mode:        operation.Identity.Mode,
		Status:      operation.State,
		CreatedAt:   operation.CreatedAt.UTC(),
		UpdatedAt:   operation.UpdatedAt.UTC(),
		DeadlineAt:  operation.DeadlineAt.UTC(),
		Binary: ProbeInstallBinaryView{
			Version:      operation.Identity.Binary.Version,
			Architecture: operation.Identity.Binary.Architecture,
		},
		Resources: resources,
	}
	switch operation.State {
	case domain.ProbeInstallFailed, domain.ProbeInstallCanceled, domain.ProbeInstallInterrupted:
		view.FailedPhase = probeFailedPhase(operation.Resources)
	}
	if operation.ErrorCode != "" {
		view.Error = &ProbeInstallErrorView{Code: operation.ErrorCode, Message: message}
	}
	return view, nil
}

// probeStepTimes renders a step's DB-clock attempt times, null when unset.
func probeStepTimes(step domain.ProbeInstallResource) (*time.Time, *time.Time) {
	var started, finished *time.Time
	if !step.StartedAt.IsZero() {
		t := step.StartedAt.UTC()
		started = &t
	}
	if !step.FinishedAt.IsZero() {
		t := step.FinishedAt.UTC()
		finished = &t
	}
	return started, finished
}

// probeFailedPhase names the first step, in journal order, whose attempt did
// not apply, or nil when no attempt failed (for example a cancel before any
// step began).
func probeFailedPhase(steps []domain.ProbeInstallResource) *string {
	for _, step := range steps {
		if step.State == domain.ProbeInstallResourceNotStarted || step.State == domain.ProbeInstallResourceUnknown {
			kind := step.Spec.Kind
			return &kind
		}
	}
	return nil
}

// probeInstallServiceError maps the frozen service sentinel set onto the
// frozen HTTP contract. The underlying error text is never echoed: only these
// fixed codes and messages leave the handler.
func probeInstallServiceError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrInternal):
		// Repository validation after validated admission means corrupt
		// server state, not malformed client input (even in a joined cause).
		return probeInstallUnavailableError(c)
	case errors.Is(err, domain.ErrProbeInstallArtifact):
		return fleetError(c, http.StatusUnprocessableEntity, "artifact_rejected", "pinned probe artifact was rejected")
	case errors.Is(err, domain.ErrProbeInstallPreflight):
		var refusal *domain.ProbeInstallPreflightError
		if !errors.As(err, &refusal) || refusal == nil {
			return probeInstallUnavailableError(c)
		}
		message, ok := probeInstallPreflightMessages[refusal.Code]
		if !ok {
			return probeInstallUnavailableError(c)
		}
		return fleetError(c, http.StatusUnprocessableEntity, refusal.Code, message)
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrProbeSSHInput):
		return fleetError(c, http.StatusBadRequest, "invalid_install", "invalid install submission")
	case errors.Is(err, ports.ErrConflict), errors.Is(err, domain.ErrConflict):
		return fleetError(c, http.StatusConflict, "install_conflict", "operation identity conflicts with a different existing request")
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return fleetError(c, http.StatusNotFound, "install_not_found", "install operation not found")
	case errors.Is(err, domain.ErrProbeInstallBusy):
		c.Response().Header().Set(echo.HeaderRetryAfter, probeInstallRetryAfterSeconds)
		return fleetError(c, http.StatusServiceUnavailable, "installs_busy", "SSH install capacity exhausted")
	case errors.Is(err, domain.ErrProbeInstallClosed):
		return fleetError(c, http.StatusServiceUnavailable, "installs_closed", "SSH install executor is stopped")
	default:
		return probeInstallUnavailableError(c)
	}
}

// probeInstallPreflightMessages is the fixed allowlist of host preflight
// refusals. Each is reached before any operation row or host change exists.
var probeInstallPreflightMessages = map[string]string{
	domain.ProbeInstallPreflightTrust:        "the host did not present the pinned host key",
	domain.ProbeInstallPreflightDestination:  "the SSH destination is refused by the endpoint policy",
	domain.ProbeInstallPreflightAuth:         "the host refused the deployment key for root",
	domain.ProbeInstallPreflightUnreachable:  "the SSH host could not be reached",
	domain.ProbeInstallPreflightHelper:       "the host helper did not return a valid install preflight",
	domain.ProbeInstallPreflightOS:           "the host is not Linux",
	domain.ProbeInstallPreflightArchitecture: "the pinned binary architecture does not match the host",
	domain.ProbeInstallPreflightRoot:         "the host helper is not running as root",
	domain.ProbeInstallPreflightSystemd:      "the host is not running systemd",
	domain.ProbeInstallPreflightAuthority:    "the host has no trusted stage-authority key",
	domain.ProbeInstallPreflightExisting:     "the host already has an Uptime Phoenix probe install; clean it up first",
}

// probeInstallUnavailableError is the single closed-fail 503 for disabled
// internals, corrupt receipts and unexpected service failures.
func probeInstallUnavailableError(c echo.Context) error {
	return fleetError(c, http.StatusServiceUnavailable, "install_unavailable", "install operations unavailable")
}

// zeroInstallBuffer clears owned transient buffers (raw body, private key) on a
// best-effort basis once the response is decided. Strings decoded by
// encoding/json are immutable and cannot be cleared; only owned bytes are.
func zeroInstallBuffer(buffer []byte) {
	for i := range buffer {
		buffer[i] = 0
	}
}
