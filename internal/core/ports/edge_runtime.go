package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EdgeIdentityRepository persists stream identity and fences runtime sessions.
type EdgeIdentityRepository interface {
	ReadIdentity(ctx context.Context) (domain.EdgeIdentity, error)
	AcceptConnectionGeneration(ctx context.Context, hubID string, generation int64) error
}

// EdgeEnrollmentRepository consumes a local token in the same transaction as
// hub binding and runtime credential storage. A consumed token never authorizes
// another enrollment; a lost response is recovered with the runtime credential.
type EdgeEnrollmentRepository interface {
	IssueEnrollmentToken(ctx context.Context, token domain.EdgeEnrollmentToken) error
	EnrollmentTokenValid(ctx context.Context, hash [32]byte, at time.Time) (bool, error)
	CommitEnrollment(ctx context.Context, hash [32]byte, binding domain.EdgeEnrollment) error
	ReadEnrollment(ctx context.Context) (domain.EdgeEnrollment, error)
	ReadRuntimeCredentials(ctx context.Context) ([]domain.EdgeEnrollment, error)
}

// EdgeCredentialRepository persists rotation effects and their immutable receipts.
// Admission atomically rechecks a credential and claims a newer session generation;
// its returned binding carries the current fixed authorization deadline.
type EdgeCredentialRepository interface {
	ApplyCredentialCommand(context.Context, domain.EdgeCommandAuthority, domain.ProbeCredentialCommand) (domain.ProbeCommandOutcome, error)
	AcceptCredentialConnection(context.Context, domain.EdgeEnrollment, int64) (domain.EdgeEnrollment, error)
}

// EdgeConfigRepository retains immutable encrypted configurations across restart.
// Activations are monotonic, bind the enrolled installation, and atomically replace
// the nonsecret assignment index. Identical current content is idempotent.
type EdgeConfigRepository interface {
	ActivateConfig(ctx context.Context, config domain.EdgeActiveConfig) error
	ReadActiveConfig(ctx context.Context) (domain.EdgeActiveConfig, error)
}

// EdgeConfigDecoder validates complete remote documents against the implemented
// runtime capabilities and returns resolved domain inputs without performing I/O.
type EdgeConfigDecoder interface {
	DecodeEdge(ctx context.Context, document []byte, target domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error)
}

// EdgeConfigReader returns the authenticated, accepted runtime graph.
type EdgeConfigReader interface {
	Load(ctx context.Context) (*domain.EdgeResolvedConfig, error)
}

// EdgeCheckRepository atomically allocates sequence numbers and records state,
// telemetry, incident transitions and provider intents without provider I/O.
type EdgeCheckRepository interface {
	ReadEdgeEvidence(ctx context.Context, monitorID, generation int64) (domain.EdgeMonitorEvidence, error)
	CommitEdgeCheck(ctx context.Context, record domain.EdgeCheckRecord) (domain.RegionalObservation, error)
}

// EdgeTelemetryEncoder maps source records into explicit, validated event DTOs.
// Encoding errors abort the caller's storage transaction; no domain is marshaled.
type EdgeTelemetryEncoder interface {
	EncodeObservation(observation domain.RegionalObservation) ([]byte, error)
	EncodeIncident(seq int64, at time.Time, incident domain.RegionalIncident) ([]byte, error)
	EncodeDelivery(seq int64, delivery domain.RegionalDelivery) ([]byte, error)
}
