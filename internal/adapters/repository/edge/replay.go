package edge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	defaultMaxBatchBytes  = 512 << 10 // 512 KiB
	defaultMaxBatchEvents = 256
	maxEventBytes         = 64 << 10 // 64 KiB
)

type outboxRow struct {
	bun.BaseModel `bun:"table:edge_telemetry_outbox"`
	Seq           int64  `bun:"seq,pk"`
	Kind          string `bun:"kind"`
	ObservedAt    int64  `bun:"observed_at"`
	Payload       []byte `bun:"payload"`
}

// ReadReplayBatch reads a contiguous, bounded batch of exact stored event bytes
// starting after the durable edge committed cursor. It detects missing sequences,
// oversized/corrupt rows and signed-64-bit boundaries without skipping.
func (s *Store) ReadReplayBatch(ctx context.Context, fromSeq int64, maxEvents int, maxBytes int) (*domain.EdgeReplayBatch, error) {
	if fromSeq < 0 {
		return nil, domain.ErrValidation
	}
	if maxEvents < 1 || maxEvents > defaultMaxBatchEvents {
		return nil, domain.ErrValidation
	}
	if maxBytes < 1 || maxBytes > defaultMaxBatchBytes {
		return nil, domain.ErrValidation
	}

	var batch *domain.EdgeReplayBatch
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		i, err := readIdentity(ctx, tx)
		if err != nil {
			return err
		}

		if i.CommittedSeq == math.MaxInt64 {
			// Sequence space is exhausted. Any explicit fromSeq cannot equal
			// a next sequence. Return an empty batch without overflow when fromSeq == 0.
			if fromSeq != 0 {
				return ports.ErrConflict
			}
			batch = &domain.EdgeReplayBatch{
				StreamID:   i.StreamID,
				FirstSeq:   math.MaxInt64,
				LastSeq:    math.MaxInt64,
				Items:      nil,
				TotalBytes: 0,
			}
			return nil
		}

		nextSeq := i.CommittedSeq + 1
		if fromSeq == 0 {
			fromSeq = nextSeq
		} else if fromSeq != nextSeq {
			// Explicit fromSeq must equal local committed cursor + 1.
			// Silently skipping unsent data or requesting pruned history is prohibited.
			return ports.ErrConflict
		}

		var gap retainedGap
		gapErr := tx.NewSelect().Model(&gap).Where("through_seq >= ?", fromSeq).Order("from_seq").Limit(1).Scan(ctx)
		if gapErr != nil && !errors.Is(gapErr, sql.ErrNoRows) {
			return gapErr
		}
		if gapErr == nil && gap.FromSeq <= fromSeq {
			if gap.ThroughSeq > i.LastCreatedSeq {
				return ports.ErrConflict
			}
			gap.FromSeq = fromSeq
			batch = &domain.EdgeReplayBatch{StreamID: i.StreamID, FirstSeq: gap.FromSeq, LastSeq: gap.ThroughSeq, Gap: gap.domain(i.StreamID)}
			return nil
		}
		var rows []outboxRow
		query := tx.NewSelect().
			Model(&rows).
			Where("seq >= ?", fromSeq).
			Order("seq ASC").
			Limit(maxEvents)
		if gapErr == nil {
			query = query.Where("seq < ?", gap.FromSeq)
		}
		err = query.Scan(ctx)
		if err != nil {
			return err
		}

		if len(rows) == 0 {
			if i.LastCreatedSeq > i.CommittedSeq {
				// last_created_seq proves pending data exists; missing rows cannot
				// be returned as a healthy empty queue.
				return fmt.Errorf("missing telemetry sequence: expected %d, got none: %w", fromSeq, ports.ErrConflict)
			}
			batch = &domain.EdgeReplayBatch{
				StreamID:   i.StreamID,
				FirstSeq:   fromSeq,
				LastSeq:    fromSeq - 1,
				Items:      nil,
				TotalBytes: 0,
			}
			return nil
		}

		if rows[0].Seq != fromSeq {
			return fmt.Errorf("missing telemetry sequence: expected %d, got %d: %w", fromSeq, rows[0].Seq, ports.ErrConflict)
		}

		items := make([]domain.EdgeReplayItem, 0, len(rows))
		totalBytes := 0
		expectedSeq := fromSeq
		stoppedByLimit := false

		for _, row := range rows {
			if row.Seq > i.LastCreatedSeq {
				return ports.ErrConflict
			}
			if row.Seq != expectedSeq {
				return fmt.Errorf("telemetry sequence gap: expected %d, got %d: %w", expectedSeq, row.Seq, ports.ErrConflict)
			}
			if len(row.Payload) == 0 || len(row.Payload) > maxEventBytes {
				return fmt.Errorf("event %d corrupt or oversized (%d bytes): %w", row.Seq, len(row.Payload), ErrStorage)
			}

			if len(items) > 0 && totalBytes+len(row.Payload) > maxBytes {
				stoppedByLimit = true
				break
			}
			if len(items) == 0 && len(row.Payload) > maxBytes {
				return fmt.Errorf("single event %d exceeds maximum batch bytes (%d > %d): %w", row.Seq, len(row.Payload), maxBytes, domain.ErrValidation)
			}

			items = append(items, domain.EdgeReplayItem{
				Seq:        row.Seq,
				Kind:       row.Kind,
				ObservedAt: time.UnixMicro(row.ObservedAt).UTC(),
				Payload:    row.Payload,
			})
			totalBytes += len(row.Payload)

			if expectedSeq == math.MaxInt64 {
				stoppedByLimit = true
				break
			}
			expectedSeq++
		}

		if len(items) == maxEvents {
			stoppedByLimit = true
		}

		if !stoppedByLimit && items[len(items)-1].Seq < i.LastCreatedSeq && (gapErr != nil || items[len(items)-1].Seq+1 != gap.FromSeq) {
			return fmt.Errorf("missing telemetry tail sequence: expected up to %d, got %d: %w", i.LastCreatedSeq, items[len(items)-1].Seq, ports.ErrConflict)
		}

		batch = &domain.EdgeReplayBatch{
			StreamID:   i.StreamID,
			FirstSeq:   items[0].Seq,
			LastSeq:    items[len(items)-1].Seq,
			Items:      items,
			TotalBytes: totalBytes,
		}
		return nil
	})
	if err != nil {
		return nil, storageError(ctx, err)
	}

	return batch, nil
}

// CommitReplayACK atomically advances edge_identity.committed_seq and deletes
// acknowledged telemetry rows under the exact current installation, probe, stream
// and connection-generation fence.
func (s *Store) CommitReplayACK(ctx context.Context, fence domain.EdgeReplayFence, result domain.ProbeReplayResult) error {
	if fence.HubID == "" || fence.ProbeID == "" || fence.StreamID == "" || fence.ConnectionGeneration <= 0 {
		return domain.ErrValidation
	}
	if result.StreamID != fence.StreamID || result.CommittedSeq < 0 {
		return domain.ErrValidation
	}

	return s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if i.HubID != fence.HubID || i.ProbeID != fence.ProbeID || i.StreamID != fence.StreamID || i.ConnectionGeneration != fence.ConnectionGeneration {
			return ports.ErrConflict
		}
		if result.StreamID != i.StreamID {
			return ports.ErrConflict
		}
		if result.CommittedSeq < i.CommittedSeq {
			return ports.ErrConflict
		}
		if result.CommittedSeq > i.LastCreatedSeq {
			return ports.ErrConflict
		}
		if result.CommittedSeq == i.CommittedSeq {
			return nil // idempotent ACK; generation and fence already verified above
		}

		var count int64
		err := tx.NewRaw("SELECT COUNT(*) FROM edge_telemetry_outbox WHERE seq > ? AND seq <= ?", i.CommittedSeq, result.CommittedSeq).Scan(ctx, &count)
		if err != nil {
			return err
		}
		var gapCount int64
		if err := tx.NewRaw("SELECT COALESCE(SUM(MIN(through_seq, ?) - MAX(from_seq, ?) + 1),0) FROM edge_gaps WHERE through_seq > ? AND from_seq <= ?", result.CommittedSeq, i.CommittedSeq+1, i.CommittedSeq, result.CommittedSeq).Scan(ctx, &gapCount); err != nil {
			return err
		}
		count += gapCount
		expectedCount := result.CommittedSeq - i.CommittedSeq
		if count != expectedCount {
			return fmt.Errorf("cannot commit ACK across missing sequence: expected %d rows, found %d: %w", expectedCount, count, ports.ErrConflict)
		}

		if _, err := tx.ExecContext(ctx, "UPDATE edge_identity SET committed_seq = ? WHERE id = 1", result.CommittedSeq); err != nil {
			return err
		}

		if _, err = tx.ExecContext(ctx, "DELETE FROM edge_telemetry_outbox WHERE seq <= ?", result.CommittedSeq); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM edge_gaps WHERE through_seq <= ?", result.CommittedSeq); err != nil {
			return err
		}
		if result.CommittedSeq < math.MaxInt64 {
			_, err = tx.ExecContext(ctx, "UPDATE edge_gaps SET from_seq = ? WHERE from_seq <= ?", result.CommittedSeq+1, result.CommittedSeq)
		}
		return err
	})
}
