package services

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Registration and operation input errors mapped by the HTTP layer to the
// protocol section 7.3 statuses.
var (
	ErrInvalidRegistration = errors.New("invalid probe registration")
	ErrInvalidOperation    = errors.New("invalid administrative operation request")
)

// Operation error codes are bounded machine tokens; messages are fixed redacted
// strings. No underlying error text, token or credential ever reaches a receipt.
const (
	operationCodeEnrollmentFailed   = "enrollment_failed"
	operationCodeEnrollmentConflict = "enrollment_conflict"
	operationCodeRotationFailed     = "rotation_failed"
	operationCodeRotationConflict   = "rotation_conflict"
	operationCodeResetFailed        = "reset_failed"
	operationCodeResetConflict      = "reset_conflict"
)

const (
	operationMessageEnrollmentFailed = "enrollment exchange failed"
	operationMessageRotationFailed   = "credential rotation failed"
	operationMessageResetFailed      = "stream reset preparation failed"
)

// enrollmentTokenPrefix is the write-only operator token prefix. Runtime
// credentials use phx_probe_ and must never appear in an enrollment request.
const enrollmentTokenPrefix = "phx_probe_enroll_"

// minEnrollmentTokenSuffix is the unpadded base64url rendering of 32 random
// bytes (43 characters); tokens are accepted only at or above that entropy.
const minEnrollmentTokenSuffix = 43

// maxEnrollmentTokenLength bounds the write-only token body.
const maxEnrollmentTokenLength = 256

// ProbeEnrollmentRunner prepares a connection from the registration's frozen
// network trust and executes the operator enrollment exchange over the pinned
// endpoint. ProbeConnectorService implements it.
type ProbeEnrollmentRunner interface {
	Prepare(ctx context.Context, probeID, streamID, endpoint, pin string) (*domain.ProbeConnection, error)
	Enroll(ctx context.Context, probeID, enrollmentToken string) error
}

// ProbeCredentialRotationIssuer issues one immutable credential rotation under
// a stable caller-supplied identity. ProbeCredentialRotationService implements
// it; no secret is ever returned.
type ProbeCredentialRotationIssuer interface {
	Issue(ctx context.Context, issue domain.ProbeCredentialRotationIssue) (*domain.ProbeCredentialRotation, error)
}

// ProbeRegistrationRequest is one remote registration create. Endpoint and
// TLSPin are the operator-declared network trust, frozen on the registration.
type ProbeRegistrationRequest struct {
	Key      string
	Name     string
	Location string
	Endpoint string
	TLSPin   string
}

// ProbeRegistrationPatch updates only the mutable display and scheduling
// fields. Endpoint and pin are immutable identity: changing them requires the
// explicit identity workflow, never an ordinary update.
type ProbeRegistrationPatch struct {
	Name             string
	Location         string
	Enabled          bool
	ExpectedRevision int64
}

// ProbeAdminService wraps the existing installation, rotation and reset
// services with durable administrative operations. Every acknowledged
// operation is persisted before the caller sees it; a 202 without a stored row
// is impossible by construction. Work runs synchronously to its hub-side
// terminal state; a remote peer may confirm later (that is separate evidence,
// never implied by a succeeded receipt).
type ProbeAdminService struct {
	operations   ports.ProbeOperationRepository
	registry     ports.ProbeRegistryRepository
	installation ports.ProbeInstallationRepository
	connections  ports.ProbeConnectionRepository
	enroll       ProbeEnrollmentRunner
	rotations    ProbeCredentialRotationIssuer
	resets       ports.ProbeStreamResetRepository
	now          func() time.Time
}

// NewProbeAdminService binds the durable operation store, the registration
// reader and the existing installation/rotation/reset services.
func NewProbeAdminService(
	operations ports.ProbeOperationRepository,
	registry ports.ProbeRegistryRepository,
	installation ports.ProbeInstallationRepository,
	connections ports.ProbeConnectionRepository,
	enroll ProbeEnrollmentRunner,
	rotations ProbeCredentialRotationIssuer,
	resets ports.ProbeStreamResetRepository,
) *ProbeAdminService {
	return &ProbeAdminService{
		operations: operations, registry: registry, installation: installation, connections: connections,
		enroll: enroll, rotations: rotations, resets: resets, now: time.Now,
	}
}

// CreateRegistration creates one remote registration with its frozen network
// trust. The reserved local row is never creatable here.
func (s *ProbeAdminService) CreateRegistration(ctx context.Context, req ProbeRegistrationRequest) (*domain.Probe, error) {
	if s == nil || s.registry == nil {
		return nil, domain.ErrInternal
	}
	if err := validRegistrationNetworkTrust(req.Endpoint, req.TLSPin); err != nil {
		return nil, err
	}
	probe := &domain.Probe{
		Key: req.Key, Name: req.Name, Location: req.Location,
		Kind: domain.ProbeKindRemote, Enabled: true,
		Endpoint: req.Endpoint, TLSPin: req.TLSPin,
	}
	if err := s.registry.Create(ctx, probe); err != nil {
		return nil, err
	}
	return probe, nil
}

// UpdateRegistration applies the mutable fields with optimistic revision
// control. The reserved local row and immutable identity reject mutation.
func (s *ProbeAdminService) UpdateRegistration(ctx context.Context, probeID string, patch ProbeRegistrationPatch) (*domain.Probe, error) {
	if s == nil || s.registry == nil {
		return nil, domain.ErrInternal
	}
	if !validRemoteProbeRegistrationID(probeID) || patch.ExpectedRevision < 1 {
		return nil, fmt.Errorf("invalid registration patch: %w", ErrInvalidRegistration)
	}
	current, err := s.registry.GetByID(ctx, probeID)
	if err != nil {
		return nil, err
	}
	updated := *current
	updated.Name, updated.Location, updated.Enabled = patch.Name, patch.Location, patch.Enabled
	if err := s.registry.Update(ctx, &updated, patch.ExpectedRevision); err != nil {
		return nil, err
	}
	return &updated, nil
}

// Enroll runs the write-only enrollment exchange: it prepares the connection
// from the registration's frozen network trust when none exists, then consumes
// the operator token over the pinned endpoint. The token is never stored,
// logged or echoed; failure receipts carry bounded redacted diagnostics only.
func (s *ProbeAdminService) Enroll(ctx context.Context, probeID, enrollmentToken string) (*domain.ProbeOperation, error) {
	if s == nil || s.operations == nil || s.registry == nil || s.connections == nil || s.enroll == nil || s.installation == nil {
		return nil, domain.ErrInternal
	}
	if !validRemoteProbeRegistrationID(probeID) || !ValidEnrollmentToken(enrollmentToken) {
		return nil, fmt.Errorf("invalid enrollment request: %w", ErrInvalidOperation)
	}
	registration, err := s.registry.GetByID(ctx, probeID)
	if err != nil {
		return nil, err
	}
	if _, err := s.installation.Get(ctx); err != nil {
		return nil, err
	}
	if _, err := s.connections.GetConnection(ctx, probeID); err != nil {
		if !errors.Is(err, ports.ErrNotFound) && !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		// First enrollment: the frozen network trust becomes the prepared
		// connection together with a fresh generated runtime credential.
		if registration.Endpoint == "" || registration.TLSPin == "" {
			return nil, fmt.Errorf("registration has no network trust: %w", ErrInvalidRegistration)
		}
		endpoint, err := RuntimeEndpoint(registration.Endpoint)
		if err != nil {
			return nil, err
		}
		if _, err := s.enroll.Prepare(ctx, probeID, uuid.NewString(), endpoint, registration.TLSPin); err != nil {
			return nil, err
		}
	}
	operation, err := s.begin(ctx, probeID, domain.ProbeOperationEnroll, "exchanging", "")
	if err != nil {
		return nil, err
	}
	if err := s.enroll.Enroll(ctx, probeID, enrollmentToken); err != nil {
		return s.finish(ctx, operation, domain.ProbeOperationFailed, "exchanging", operationCodeEnrollmentFailed, operationMessageEnrollmentFailed)
	}
	return s.finish(ctx, operation, domain.ProbeOperationSucceeded, "exchanged", "", "")
}

// RotateCredential issues one immutable credential rotation under the durable
// operation identity. No plaintext token appears in the request or response.
func (s *ProbeAdminService) RotateCredential(ctx context.Context, probeID string, credentialVersion int64) (*domain.ProbeOperation, error) {
	if s == nil || s.operations == nil || s.registry == nil || s.installation == nil || s.rotations == nil {
		return nil, domain.ErrInternal
	}
	if !validRemoteProbeRegistrationID(probeID) || credentialVersion < 1 {
		return nil, fmt.Errorf("invalid rotation request: %w", ErrInvalidOperation)
	}
	if _, err := s.registry.GetByID(ctx, probeID); err != nil {
		return nil, err
	}
	installation, err := s.installation.Get(ctx)
	if err != nil {
		return nil, err
	}
	operation, err := s.begin(ctx, probeID, domain.ProbeOperationRotateCredential, "exchanging", "")
	if err != nil {
		return nil, err
	}
	if _, err := s.rotations.Issue(ctx, domain.ProbeCredentialRotationIssue{
		HubID: installation.HubID, ProbeID: probeID,
		RotationID: operation.OperationID, CredentialVersion: credentialVersion,
	}); err != nil {
		code, message := operationCodeRotationFailed, operationMessageRotationFailed
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, domain.ErrValidation) {
			code = operationCodeRotationConflict
		}
		return s.finish(ctx, operation, domain.ProbeOperationFailed, "exchanging", code, message)
	}
	return s.finish(ctx, operation, domain.ProbeOperationSucceeded, "issued", "", "")
}

// ResetStream durably prepares the immutable stream-reset plan under the
// caller-supplied operation identity (retained for retries). Success is hub
// side: the new stream is proved only by later authenticated health.
func (s *ProbeAdminService) ResetStream(ctx context.Context, probeID, streamID, operationID string) (*domain.ProbeOperation, error) {
	if s == nil || s.operations == nil || s.registry == nil || s.installation == nil || s.connections == nil || s.resets == nil {
		return nil, domain.ErrInternal
	}
	if !validRemoteProbeRegistrationID(probeID) || !domain.ValidHubID(streamID) || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid stream reset request: %w", ErrInvalidOperation)
	}
	if _, err := s.registry.GetByID(ctx, probeID); err != nil {
		return nil, err
	}
	installation, err := s.installation.Get(ctx)
	if err != nil {
		return nil, err
	}
	connection, err := s.connections.GetConnection(ctx, probeID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
			return nil, ports.ErrConflict
		}
		return nil, err
	}
	if connection.StreamID == "" || connection.StreamID == streamID {
		return nil, fmt.Errorf("reset stream must differ from the bound stream: %w", ErrInvalidOperation)
	}
	operation, err := s.begin(ctx, probeID, domain.ProbeOperationResetStream, "preparing", operationID)
	if err != nil {
		return nil, err
	}
	if _, err := s.resets.PrepareStreamReset(ctx, domain.ProbeStreamResetIssue{
		ResetID: operationID, HubID: installation.HubID, ProbeID: probeID,
		PreviousStreamID: connection.StreamID, StreamID: streamID,
	}); err != nil {
		code, message := operationCodeResetFailed, operationMessageResetFailed
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, domain.ErrValidation) {
			code = operationCodeResetConflict
		}
		return s.finish(ctx, operation, domain.ProbeOperationFailed, "preparing", code, message)
	}
	return s.finish(ctx, operation, domain.ProbeOperationSucceeded, "prepared", "", "")
}

// Operation reads one exact receipt. Reads never create rows.
func (s *ProbeAdminService) Operation(ctx context.Context, operationID string) (*domain.ProbeOperation, error) {
	if s == nil || s.operations == nil {
		return nil, domain.ErrInternal
	}
	if !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid operation id: %w", ErrInvalidOperation)
	}
	return s.operations.GetOperation(ctx, operationID)
}

// begin persists the receipt before any acknowledged work starts.
func (s *ProbeAdminService) begin(ctx context.Context, probeID, kind, phase, operationID string) (*domain.ProbeOperation, error) {
	if operationID == "" {
		operationID = uuid.NewString()
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	operation := domain.ProbeOperation{
		OperationID: operationID, ProbeID: probeID, Kind: kind,
		Status: domain.ProbeOperationPending, Phase: phase, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.operations.CreateOperation(ctx, operation); err != nil {
		return nil, err
	}
	return &operation, nil
}

// finish records the terminal hub-side outcome and returns the exact receipt.
func (s *ProbeAdminService) finish(ctx context.Context, operation *domain.ProbeOperation, status, phase, code, message string) (*domain.ProbeOperation, error) {
	var opErr *domain.ProbeOperationError
	if code != "" {
		opErr = &domain.ProbeOperationError{Code: code, Message: message}
	}
	at := s.now().UTC().Truncate(time.Microsecond)
	if err := s.operations.FinishOperation(ctx, operation.OperationID, status, phase, opErr, at); err != nil {
		return nil, err
	}
	operation.Status, operation.Phase, operation.Error, operation.UpdatedAt = status, phase, opErr, at
	return operation, nil
}

// ValidEnrollmentToken enforces the write-only operator token shape: the
// enrollment prefix (never a runtime credential) and at least 32 random bytes
// of unpadded base64url suffix, bounded above.
func ValidEnrollmentToken(token string) bool {
	if len(token) > maxEnrollmentTokenLength || !strings.HasPrefix(token, enrollmentTokenPrefix) {
		return false
	}
	suffix := token[len(enrollmentTokenPrefix):]
	if len(suffix) < minEnrollmentTokenSuffix {
		return false
	}
	for _, c := range suffix {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// RuntimeEndpoint normalizes the operator-declared registration endpoint into
// the pinned runtime URL the connection requires. A declared host[:port] (the
// registration create shape) becomes wss://host[:port]/ws/probe/v1; an already
// canonical runtime URL passes through unchanged.
func RuntimeEndpoint(declared string) (string, error) {
	if u, err := url.Parse(declared); err == nil && u.Scheme == "wss" && u.Path == "/ws/probe/v1" &&
		u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Opaque == "" {
		return declared, nil
	}
	if strings.Contains(declared, "://") || strings.ContainsAny(declared, "/?#@ \t\r\n") {
		return "", fmt.Errorf("unsupported endpoint declaration: %w", ErrInvalidRegistration)
	}
	return "wss://" + declared + "/ws/probe/v1", nil
}

// validRegistrationNetworkTrust validates the operator-declared endpoint and
// pin. Both are mandatory for a registration created over the API; the local
// operator flow may prepare a connection directly instead.
func validRegistrationNetworkTrust(endpoint, pin string) error {
	if endpoint == "" || len(endpoint) > 2048 || strings.TrimSpace(endpoint) != endpoint ||
		strings.ContainsAny(endpoint, "\t\r\n \x00") {
		return fmt.Errorf("invalid endpoint: %w", ErrInvalidRegistration)
	}
	if !domain.ValidKeyHash(pin) {
		return fmt.Errorf("invalid TLS fingerprint: %w", ErrInvalidRegistration)
	}
	return nil
}

func validRemoteProbeRegistrationID(id string) bool {
	return id != domain.LocalProbeID && domain.ValidHubID(id)
}
