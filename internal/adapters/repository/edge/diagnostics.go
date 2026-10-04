package edge

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Diagnostics is a coherent local progress snapshot without confidential data.
type Diagnostics struct {
	Identity          domain.EdgeIdentity
	FirstRetainedSeq  int64
	QueueBytes        int64
	OldestQueuedAt    *time.Time
	PendingDeliveries int64
	FailedDeliveries  int64
	GapRanges         int64
	QueuePressure     bool
	MetadataBytes     int64
	MetadataPressure  bool
}

// ReadDiagnostics reads stream progress and retained telemetry in one transaction.
func (s *Store) ReadDiagnostics(ctx context.Context) (Diagnostics, error) {
	var out Diagnostics
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out.Identity, err = readIdentity(ctx, tx)
		if err != nil {
			return err
		}
		if err := tx.NewRaw("SELECT used_bytes FROM edge_metadata_budget WHERE id=1").Scan(ctx, &out.MetadataBytes); err != nil {
			return err
		}
		out.MetadataPressure = out.MetadataBytes >= maxMetadataBytes*8/10
		var row struct {
			First  int64
			Bytes  int64
			Oldest *int64
		}
		if err := tx.NewRaw("SELECT COALESCE(MIN(seq),0) AS first, COALESCE(SUM(length(payload)),0) AS bytes, MIN(observed_at) AS oldest FROM edge_telemetry_outbox").Scan(ctx, &row); err != nil {
			return err
		}
		out.FirstRetainedSeq, out.QueueBytes, out.OldestQueuedAt = row.First, row.Bytes, timeFromMicro(row.Oldest)
		var gaps struct {
			First  int64
			Oldest *int64
			Count  int64
		}
		if err := tx.NewRaw("SELECT COALESCE(MIN(from_seq),0) AS first, MIN(observed_from) AS oldest, COUNT(*) AS count FROM edge_gaps").Scan(ctx, &gaps); err != nil {
			return err
		}
		out.GapRanges = gaps.Count
		if gaps.Count > 0 {
			if out.FirstRetainedSeq == 0 || gaps.First < out.FirstRetainedSeq {
				out.FirstRetainedSeq = gaps.First
			}
			if out.OldestQueuedAt == nil || time.UnixMicro(*gaps.Oldest).Before(*out.OldestQueuedAt) {
				out.OldestQueuedAt = timeFromMicro(gaps.Oldest)
			}
		}
		if s.retention.MaxBytes > 0 {
			out.QueueBytes, err = telemetryStorageBytes(ctx, tx)
			if err != nil {
				return err
			}
			out.QueuePressure = out.QueueBytes >= s.retention.MaxBytes*8/10
		} else {
			out.QueueBytes += gaps.Count * queueRowBytes
		}
		if err := tx.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox WHERE status IN ('pending', 'leased', 'retrying')").Scan(ctx, &out.PendingDeliveries); err != nil {
			return err
		}
		if err := tx.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox WHERE status = 'failed'").Scan(ctx, &out.FailedDeliveries); err != nil {
			return err
		}
		return nil
	})
	return out, storageError(ctx, err)
}

// CheckWritable verifies an actual bounded SQLite write transaction. Merely
// opening a read connection would not detect a read-only filesystem or full disk.
func (s *Store) CheckWritable(ctx context.Context) error {
	return s.write(ctx, func(context.Context, bun.Tx, domain.EdgeIdentity) error { return nil })
}
