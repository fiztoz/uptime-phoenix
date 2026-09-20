package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EdgeReplayRepository manages reading and ACK-pruning of the edge telemetry outbox.
type EdgeReplayRepository interface {
	ReadReplayBatch(ctx context.Context, fromSeq int64, maxEvents int, maxBytes int) (*domain.EdgeReplayBatch, error)
	CommitReplayACK(ctx context.Context, fence domain.EdgeReplayFence, result domain.ProbeReplayResult) error
}
