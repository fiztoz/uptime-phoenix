package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestEdgeHealthPressureGapAndAcknowledgedDrain(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	i := domain.EdgeIdentity{ProbeID: uuid.NewString(), StreamID: uuid.NewString(), Fingerprint: strings.Repeat("a", 64)}
	store, err := edge.Open(t.Context(), dir, i, edge.WithRetentionPolicy(edge.RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "edge.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	hubID := uuid.NewString()
	if _, err := db.ExecContext(t.Context(), "UPDATE edge_identity SET hub_id=?, connection_generation=1, last_created_seq=13", hubID); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	const threshold = (1 << 20) * 8 / 10
	// Exact byte-sized fixtures exercise storage accounting and the production
	// health mapping. Wire/ACK authentication is covered by the real TLS suite.
	for seq := 1; seq <= 13; seq++ {
		size := 65536
		if seq == 13 {
			size = threshold - 1 - 12*(65536+256) - 256
		}
		if _, err := db.ExecContext(t.Context(), "INSERT INTO edge_telemetry_outbox(seq,kind,observed_at,payload) VALUES(?,'observation',?,zeroblob(?))", seq, at.UnixMicro(), size); err != nil {
			t.Fatal(err)
		}
	}
	assertHealth := func(wantBytes int64, pressure, gap bool) {
		t.Helper()
		diagnostic, err := store.ReadDiagnostics(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		health := edgeHealth(diagnostic, true, true, 1, false)
		if *health.QueueBytes != wantBytes || slices.Contains(health.Errors, "queue_pressure") != pressure || slices.Contains(health.Errors, "telemetry_gap") != gap || !health.Ready {
			t.Fatal("incorrect production health", health, diagnostic)
		}
		if edgeHealth(diagnostic, true, true, 1, true).Ready {
			t.Fatal("shutdown advertised ready")
		}
	}
	assertHealth(threshold-1, false, false)
	if _, err := db.ExecContext(t.Context(), "UPDATE edge_telemetry_outbox SET payload=CAST(payload || x'00' AS BLOB) WHERE seq=13"); err != nil {
		t.Fatal(err)
	}
	assertHealth(threshold, true, false)
	if err := store.SweepRetention(t.Context(), at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertHealth(256, false, true)
	fence := domain.EdgeReplayFence{HubID: hubID, ProbeID: i.ProbeID, StreamID: i.StreamID, ConnectionGeneration: 1}
	if err := store.CommitReplayACK(t.Context(), fence, domain.ProbeReplayResult{StreamID: i.StreamID, CommittedSeq: 13}); err != nil {
		t.Fatal(err)
	}
	assertHealth(0, false, false)
}
