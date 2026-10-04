package edge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/uptrace/bun"
)

// This is a write admission threshold, not a filesystem quota. A transaction
// admitted below it can add its own WAL frames before the next admission check.
const walCheckpointBytes = 16 << 20

func (s *Store) databaseByteLimit() int64 {
	// Space for B-trees, indexes, free pages and the independently bounded command,
	// provider and metadata ledgers, beyond the operator's telemetry allocation.
	return max(1<<30, 2*s.retention.MaxBytes+(512<<20))
}

// prepareStorageWrite runs outside a transaction on the reserved connection.
// SQLite's page limit is connection-local, so reapply it after driver reconnects.
// A grandfathered oversized DB may reuse/delete pages but may not grow further.
func (s *Store) prepareStorageWrite(ctx context.Context, db bun.IDB) error {
	var pageSize, pages int64
	if err := db.NewRaw("PRAGMA page_size").Scan(ctx, &pageSize); err != nil {
		return err
	}
	if pageSize < 512 || pageSize > 65536 {
		return ErrStorage
	}
	if err := db.NewRaw(fmt.Sprintf("PRAGMA max_page_count = %d", s.databaseByteLimit()/pageSize)).Scan(ctx, &pages); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA journal_size_limit = %d", walCheckpointBytes)); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(s.dataDir, "edge.db-wal"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return ErrStorage
	}
	if info.Size() < walCheckpointBytes {
		return nil
	}
	var checkpoint struct {
		Busy         int
		Log          int
		Checkpointed int
	}
	if err := db.NewRaw("PRAGMA wal_checkpoint(TRUNCATE)").Scan(ctx, &checkpoint); err != nil {
		return err
	}
	if checkpoint.Busy != 0 {
		// A long reader must not cause unbounded append-only growth. Preserve all
		// evidence and refuse new writes until that reader releases its snapshot.
		return ErrStorage
	}
	return nil
}
