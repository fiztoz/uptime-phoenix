package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// probeHostBootstrapMaxBodyBytes bounds the request: the 64 KiB transient
// deployment key, a ≤1 KiB password and short pins fit with JSON escaping.
const probeHostBootstrapMaxBodyBytes = 128 << 10

// probeHostBootstrapKind is the frozen response discriminator.
const probeHostBootstrapKind = "ssh_probe_host_bootstrap"

// probeHostBootstrapRunner is the synchronous bootstrap service. It consumes
// (clears) the request's password and key buffers.
type probeHostBootstrapRunner interface {
	Bootstrap(context.Context, domain.ProbeHostBootstrapRequest) (domain.ProbeHostBootstrapResult, error)
}

// ProbeHostBootstrapHandlers exposes POST /api/probe-host-bootstraps: the
// one-time SSH-password host preparation for the M8 restricted-key installer.
// The request is decoded with the same strict one-object binder rules as the
// installer. The password and deployment key are used for this one request
// only: they are never stored, logged, echoed or placed in a response, and
// every response is Cache-Control: no-store.
type ProbeHostBootstrapHandlers struct {
	svc     probeHostBootstrapRunner
	enabled bool
}

// NewProbeHostBootstrapHandlers builds the handlers; a disabled surface or a
// missing service answers a typed 503 instead of fake success. A typed nil
// service pointer is treated as missing.
func NewProbeHostBootstrapHandlers(svc probeHostBootstrapRunner, enabled bool) *ProbeHostBootstrapHandlers {
	if svc != nil {
		if value := reflect.ValueOf(svc); value.Kind() == reflect.Pointer && value.IsNil() {
			svc = nil
		}
	}
	return &ProbeHostBootstrapHandlers{svc: svc, enabled: enabled}
}

// ProbeHostBootstrapView is the exact success shape. It names what changed
// and states that the restricted key verified; it never carries the address,
// user, fingerprints, password, key or host output.
type ProbeHostBootstrapView struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
	// Helper is the pinned phoenix-probe-admin release and whether it was
	// installed or already present byte-for-byte.
	Helper ProbeHostBootstrapHelperView `json:"helper"`
	// StageAuthority is installed or unchanged.
	StageAuthority string `json:"stage_authority"`
	// AuthorizedKey is added or unchanged.
	AuthorizedKey string `json:"authorized_key"`
}

// ProbeHostBootstrapHelperView names the helper pin without digest or size.
type ProbeHostBootstrapHelperView struct {
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	State        string `json:"state"`
}

// probeHostBootstrapMessages is the fixed refusal allowlist.
var probeHostBootstrapMessages = map[string]string{
	domain.ProbeHostBootstrapTrust:           "the host did not present the pinned host key; the password was not sent",
	domain.ProbeHostBootstrapDestination:     "the SSH destination is refused by the endpoint policy",
	domain.ProbeHostBootstrapUnreachable:     "the SSH host could not be reached",
	domain.ProbeHostBootstrapOS:              "the host is not Linux",
	domain.ProbeHostBootstrapArchitecture:    "the pinned helper architecture does not match the host",
	domain.ProbeHostBootstrapAuth:            "the host rejected the SSH user or password",
	domain.ProbeHostBootstrapPrivilege:       "could not become root: check the sudo password and sudo rights (sudo must not require a TTY)",
	domain.ProbeHostBootstrapUnsafeLayout:    "a destination directory or file is not root-owned or is group/world writable; nothing was changed",
	domain.ProbeHostBootstrapAuthorityClash:  "the host already trusts a different stage-authority key; nothing was changed",
	domain.ProbeHostBootstrapKeyClash:        "root's authorized_keys already lists the deployment key with other options; nothing was changed",
	domain.ProbeHostBootstrapTransfer:        "the helper did not arrive intact; nothing was changed",
	domain.ProbeHostBootstrapFailed:          "the host bootstrap failed",
	domain.ProbeHostBootstrapVerifyAuth:      "files were installed, but root did not accept the restricted deployment key (check PermitRootLogin and AuthorizedKeysFile)",
	domain.ProbeHostBootstrapVerifyHelper:    "files were installed, but the helper did not return a valid install preflight",
	domain.ProbeHostBootstrapVerifyAuthority: "files were installed, but the helper could not load the stage-authority key (check /etc ownership and modes)",
}

// Submit handles POST /api/probe-host-bootstraps. It answers 200 only after
// the files are in place and the restricted key verified.
func (h *ProbeHostBootstrapHandlers) Submit(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	userID, ok := userIDFromContext(c)
	if !ok || userID <= 0 {
		return fleetError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
	}
	if !h.enabled {
		return fleetError(c, http.StatusServiceUnavailable, "installs_disabled", "SSH probe installs are disabled")
	}
	if h.svc == nil {
		return probeHostBootstrapUnavailable(c)
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, probeHostBootstrapMaxBodyBytes+1))
	defer zeroInstallBuffer(raw)
	if err != nil {
		return probeHostBootstrapInvalid(c)
	}
	if len(raw) > probeHostBootstrapMaxBodyBytes {
		return fleetError(c, http.StatusRequestEntityTooLarge, "bootstrap_too_large", "request body exceeds the bootstrap limit")
	}
	request, err := decodeProbeHostBootstrap(raw)
	if err != nil {
		return probeHostBootstrapInvalid(c)
	}
	// Owned byte copies; the decoded strings are immutable and cannot be
	// cleared (best effort, as for the installer's private key).
	password := []byte(request.Password)
	privateKey := []byte(request.PrivateKey)
	defer zeroInstallBuffer(password)
	defer zeroInstallBuffer(privateKey)
	request.Password, request.PrivateKey = "", ""
	submission := domain.ProbeHostBootstrapRequest{
		Host:               domain.ProbeSSHHost{Address: request.SSHAddress, User: request.SSHUser},
		Password:           password,
		HostKeyFingerprint: request.HostKeyFingerprint,
		PrivateKey:         privateKey,
		Helper: domain.ProbeBinaryPin{
			Version:      request.HelperVersion,
			Architecture: request.HelperArchitecture,
			SHA256:       request.HelperSHA256,
			Size:         request.HelperSize,
		},
		RequestedBy: userID,
	}
	if !submission.Valid() {
		return probeHostBootstrapInvalid(c)
	}
	pin := submission.Helper
	result, err := h.svc.Bootstrap(c.Request().Context(), submission)
	if err != nil {
		return probeHostBootstrapServiceError(c, err)
	}
	if !result.Valid() {
		return probeHostBootstrapUnavailable(c)
	}
	return c.JSON(http.StatusOK, ProbeHostBootstrapView{
		Kind:           probeHostBootstrapKind,
		Status:         "verified",
		Helper:         ProbeHostBootstrapHelperView{Version: pin.Version, Architecture: pin.Architecture, State: result.Helper},
		StageAuthority: result.StageAuthority,
		AuthorizedKey:  result.AuthorizedKey,
	})
}

func probeHostBootstrapServiceError(c echo.Context, err error) error {
	var refusal *domain.ProbeHostBootstrapError
	switch {
	case errors.As(err, &refusal):
		message, ok := probeHostBootstrapMessages[refusal.Code]
		if !ok {
			return probeHostBootstrapUnavailable(c)
		}
		return fleetError(c, http.StatusUnprocessableEntity, refusal.Code, message)
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrProbeSSHInput):
		return probeHostBootstrapInvalid(c)
	case errors.Is(err, domain.ErrProbeInstallArtifact):
		return fleetError(c, http.StatusUnprocessableEntity, "artifact_rejected", "pinned helper artifact was rejected")
	case errors.Is(err, domain.ErrProbeHostBootstrapBusy):
		c.Response().Header().Set(echo.HeaderRetryAfter, probeInstallRetryAfterSeconds)
		return fleetError(c, http.StatusServiceUnavailable, "bootstraps_busy", "host bootstrap capacity exhausted")
	case errors.Is(err, domain.ErrProbeHostBootstrapClosed):
		return fleetError(c, http.StatusServiceUnavailable, "bootstraps_closed", "host bootstrap is stopped")
	case errors.Is(err, context.DeadlineExceeded):
		return fleetError(c, http.StatusGatewayTimeout, "bootstrap_timeout", "the host bootstrap did not finish in time; it is safe to retry")
	}
	return probeHostBootstrapUnavailable(c)
}

func probeHostBootstrapInvalid(c echo.Context) error {
	return fleetError(c, http.StatusBadRequest, "invalid_bootstrap", "invalid host bootstrap request")
}

func probeHostBootstrapUnavailable(c echo.Context) error {
	return fleetError(c, http.StatusServiceUnavailable, "bootstrap_unavailable", "host bootstrap unavailable")
}

// probeHostBootstrapRequest is the strict decoded request.
type probeHostBootstrapRequest struct {
	SSHAddress         string
	SSHUser            string
	Password           string
	HostKeyFingerprint string
	PrivateKey         string
	HelperVersion      string
	HelperArchitecture string
	HelperSHA256       string
	HelperSize         int64
}

// probeHostBootstrapStringFields are the required string members.
var probeHostBootstrapStringFields = []string{
	"ssh_address", "ssh_user", "ssh_password", "host_key_fingerprint", "private_key",
	"helper_version", "helper_architecture", "helper_sha256",
}

// probeHostBootstrapSizeField is the one required integer member.
const probeHostBootstrapSizeField = "helper_size"

var errInvalidBootstrapBody = errors.New("invalid host bootstrap body")

// decodeProbeHostBootstrap accepts exactly one JSON object with exactly the
// required members. Unknown, duplicate or case-variant keys, null or mistyped
// values, a non-plain integer size, missing members and trailing JSON refuse.
func decodeProbeHostBootstrap(raw []byte) (probeHostBootstrapRequest, error) {
	var request probeHostBootstrapRequest
	want := make(map[string]bool, len(probeHostBootstrapStringFields))
	for _, k := range probeHostBootstrapStringFields {
		want[k] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return request, errInvalidBootstrapBody
	}
	values := make(map[string]string, len(want))
	var size int64
	seen := make(map[string]bool, len(want)+1)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return request, errInvalidBootstrapBody
		}
		key, ok := keyToken.(string)
		if !ok || (!want[key] && key != probeHostBootstrapSizeField) || seen[key] {
			return request, errInvalidBootstrapBody
		}
		seen[key] = true
		valueToken, err := decoder.Token()
		if err != nil {
			return request, errInvalidBootstrapBody
		}
		if key == probeHostBootstrapSizeField {
			number, ok := valueToken.(json.Number)
			if !ok {
				return request, errInvalidBootstrapBody
			}
			parsed, ok := parseProbeInstallInteger(number.String())
			if !ok {
				return request, errInvalidBootstrapBody
			}
			size = parsed
			continue
		}
		value, ok := valueToken.(string)
		if !ok {
			return request, errInvalidBootstrapBody
		}
		values[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return request, errInvalidBootstrapBody
	}
	if _, err := decoder.Token(); err != io.EOF {
		return request, errInvalidBootstrapBody
	}
	if len(seen) != len(want)+1 || !seen[probeHostBootstrapSizeField] {
		return request, errInvalidBootstrapBody
	}
	return probeHostBootstrapRequest{
		SSHAddress:         values["ssh_address"],
		SSHUser:            values["ssh_user"],
		Password:           values["ssh_password"],
		HostKeyFingerprint: values["host_key_fingerprint"],
		PrivateKey:         values["private_key"],
		HelperVersion:      values["helper_version"],
		HelperArchitecture: values["helper_architecture"],
		HelperSHA256:       values["helper_sha256"],
		HelperSize:         size,
	}, nil
}
