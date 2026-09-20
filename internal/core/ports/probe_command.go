package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EdgeCommandRepository applies an incident-specific ACK and persists its result
// and source transition atomically. Current session authority is required even
// for duplicates. No external I/O runs while the transaction is held.
type EdgeCommandRepository interface {
	ApplyAlertAcknowledgement(context.Context, domain.EdgeCommandAuthority, domain.ProbeAlertAcknowledgement) (domain.ProbeCommandOutcome, error)
}
