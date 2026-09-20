package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EdgeCommandRepository applies an incident-specific ACK and persists its result
// and source transition atomically. Current session authority is required even
// for duplicates. No external I/O runs while the transaction is held.
type EdgeCommandRepository interface {
	ApplyAlertAcknowledgement(context.Context, domain.EdgeCommandAuthority, domain.ProbeAlertAcknowledgement) (domain.ProbeCommandOutcome, error)
}

// ProbeCommandProtector authenticates a dedicated command purpose and immutable
// scope. KeyHash binds use to the verified installation's existing secret key.
type ProbeCommandProtector interface {
	SealCommand(context.Context, domain.ProbeCommandMetadata, []byte) ([]byte, error)
	OpenCommand(context.Context, domain.ProbeCommandMetadata, []byte) ([]byte, error)
	KeyHash(string) string
}

// ProbeCommandRepository persists issuance before dispatch. Claim and completion
// fence the current connector/session under DB-clock authority. An expired request
// remains retryable until the source returns its immutable terminal receipt.
type ProbeCommandRepository interface {
	CreateCommand(context.Context, domain.ProtectedProbeCommand) (*domain.ProbeCommand, error)
	GetCommand(context.Context, string, string, string) (*domain.ProbeCommand, error)
	GetProtectedCommand(context.Context, string, string, string) (*domain.ProtectedProbeCommand, error)
	ClaimCommand(context.Context, domain.ProbeReplaySession, time.Duration) (*domain.ProtectedProbeCommand, error)
	CompleteCommand(context.Context, domain.ProbeReplaySession, domain.ProbeCommandOutcome) error
}

// ProbeAcknowledgementCodec maps the closed ACK wire DTO to pure core input.
// It validates syntax and bounds only, never operator or session authority.
type ProbeAcknowledgementCodec interface {
	EncodeAcknowledgement(context.Context, domain.ProbeAlertAcknowledgement) ([]byte, error)
	DecodeAcknowledgement(context.Context, []byte) (domain.ProbeAlertAcknowledgement, error)
}

// ProbeCommandDispatcher is the control transport's fenced persistence boundary.
type ProbeCommandDispatcher interface {
	NextCommand(context.Context, domain.ProbeReplaySession, time.Duration) (*domain.ProbeCommandDispatch, error)
	RecordCommandResult(context.Context, domain.ProbeReplaySession, domain.ProbeCommandOutcome) error
}
