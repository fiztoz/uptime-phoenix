package edge

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func edgeFileSize(t *testing.T, dir, name string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestEdgeStorageLongReaderStopsWALGrowthAndRecovers(t *testing.T) {
	s, dir := testStore(t)
	enroll(t, s)
	reader, err := sql.Open("sqlite", filepath.Join(dir, "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	tx, err := reader.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var revision int64
	if err := tx.QueryRowContext(t.Context(), "SELECT config_revision FROM edge_identity").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	// Keep the real checkpoint behavior but shorten its busy wait for this test.
	if _, err := s.db.ExecContext(t.Context(), "PRAGMA busy_timeout=10"); err != nil {
		t.Fatal(err)
	}
	for revision = 1; revision <= 4; revision++ {
		err = s.ActivateConfig(t.Context(), sizedProtectedConfig(t, revision, 8<<20))
		if errors.Is(err, ErrStorage) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if revision > 4 {
		t.Fatal("long reader allowed continued WAL growth")
	}
	before, err := s.ReadActiveConfig(t.Context())
	if err != nil || before.Snapshot.Revision != revision-1 {
		t.Fatalf("failed write changed config: %v", err)
	}
	size := edgeFileSize(t, dir, "edge.db-wal")
	if size < walCheckpointBytes || size > walCheckpointBytes+(10<<20) {
		t.Fatalf("WAL threshold plus admitted transaction: %d", size)
	}
	if err := s.CheckWritable(t.Context()); !errors.Is(err, ErrStorage) {
		t.Fatalf("health claimed writable during blocked checkpoint: %v", err)
	}
	if after := edgeFileSize(t, dir, "edge.db-wal"); after != size {
		t.Fatalf("refused write grew WAL: %d -> %d", size, after)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConfig(t.Context(), sizedProtectedConfig(t, revision, 8<<20)); err != nil {
		t.Fatalf("admission did not recover after reader closed: %v", err)
	}
	if size := edgeFileSize(t, dir, "edge.db-wal"); size >= walCheckpointBytes {
		t.Fatalf("checkpoint did not reclaim old WAL: %d", size)
	}
	metadataBytes(t, s)
}

func TestEdgeStorageRepeatedCleanupReusesFilePages(t *testing.T) {
	s, dir := testStore(t)
	enroll(t, s)
	s.retention = RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}
	for cycle := int64(0); cycle < 4; cycle++ {
		for n := int64(1); n <= 3; n++ {
			c := sizedProtectedConfig(t, cycle*3+n, 8<<20)
			c.AppliedAt = time.Now().UTC().Add(-8 * 24 * time.Hour)
			if err := s.ActivateConfig(t.Context(), c); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SweepRetention(t.Context(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if used := metadataBytes(t, s); used != (8<<20)+512+256 {
			t.Fatalf("cycle %d retained old configs: %d", cycle, used)
		}
		var pageSize, limit int64
		if err := s.db.NewRaw("PRAGMA page_size").Scan(t.Context(), &pageSize); err != nil {
			t.Fatal(err)
		}
		if err := s.db.NewRaw("PRAGMA max_page_count").Scan(t.Context(), &limit); err != nil || pageSize*limit != s.databaseByteLimit() {
			t.Fatalf("database page cap: %d * %d: %v", pageSize, limit, err)
		}
		dbSize, walSize := edgeFileSize(t, dir, "edge.db"), edgeFileSize(t, dir, "edge.db-wal")
		t.Logf("cycle=%d db=%d wal=%d metadata=%d", cycle, dbSize, walSize, metadataBytes(t, s))
		if dbSize > 40<<20 || walSize > 40<<20 {
			t.Fatalf("repeated retirement did not reuse file pages: db=%d WAL=%d", dbSize, walSize)
		}
	}
}
