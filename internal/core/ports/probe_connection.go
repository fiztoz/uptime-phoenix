package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeCredentialProtector authenticates all management connection metadata.
type ProbeCredentialProtector interface {
	SealCredential(ctx context.Context, metadata domain.ProbeCredentialMetadata, token string) ([]byte, error)
	OpenCredential(ctx context.Context, metadata domain.ProbeCredentialMetadata, ciphertext []byte) (string, error)
}

// ProbeConnectionRepository durably prepares a credential before network use.
// Preparation is immutable; a matching retry reads existing protected material,
// while replacement requires a future explicit rotation/recovery operation.
type ProbeConnectionRepository interface {
	PrepareConnection(ctx context.Context, connection domain.ProbeConnection) (*domain.ProbeConnection, error)
	GetConnection(ctx context.Context, probeID string) (*domain.ProbeConnection, error)
	ListConnections(ctx context.Context) ([]domain.ProbeConnection, error)
	ActivateConnection(ctx context.Context, probeID, enrollmentID string, version int64, at time.Time) error
	GetConnectionCursor(ctx context.Context, probeID, streamID string) (int64, error)
}

// ProbeConnectionTransport performs pinned enrollment and one runtime session.
// established runs only after validated probe health confirms the new generation.
// applied receives only a fully validated receipt matching the transferred bytes.
// ingest receives replayed telemetry batches under the current unexpired lease fence.
// The caller must hold and renew a hub DB lease until Run returns.
// ErrProbeCredentialRejected is reserved for HTTP 401 before websocket admission.
type ProbeConnectionTransport interface {
	Enroll(ctx context.Context, connection domain.ProbeCredentialMetadata, enrollmentToken, runtimeToken string) error
	Run(ctx context.Context, input domain.ProbeSessionInput, established func(context.Context) error, applied func(context.Context, domain.ProbeActiveConfig) error, ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)) error
}

// ProbeWatchdogConnectionTransport additionally admits application-health frames
// through the runtime owner's monotonic ordering gate. The caller starts/ends
// the source session only after adopting/releasing its durable generation fence.
type ProbeWatchdogConnectionTransport interface {
	RunWithWatchdog(ctx context.Context, input domain.ProbeSessionInput, admission ProbeHealthAdmission, established func(context.Context) error, applied func(context.Context, domain.ProbeActiveConfig) error, ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)) error
}
