package edge

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func retentionStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, dir := testStore(t)
	enroll(t, s)
	if err := WithRetentionPolicy(RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour})(s); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestRetentionAgeGapAndACKSurviveRestart(t *testing.T) {
	s, dir := retentionStore(t)
	now := time.Now().UTC()
	for seq := int64(1); seq <= 4; seq++ {
		at := now.Add(-2 * time.Hour)
		if seq == 3 {
			at = now
		}
		insertOutboxRow(t, s, seq, "observation", at, []byte("evidence"))
	}
	setIdentitySeqs(t, s, 4, 0)
	if err := s.SweepRetention(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	d, err := s.ReadDiagnostics(t.Context())
	if err != nil || d.Identity.CommittedSeq != 0 || d.GapRanges != 2 || d.FirstRetainedSeq != 1 {
		t.Fatalf("loss was hidden: %+v %v", d, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, testIdentity(), WithRetentionPolicy(RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	batch, err := s.ReadReplayBatch(t.Context(), 0, 256, 512<<10)
	if err != nil || batch.Gap == nil || batch.FirstSeq != 1 || batch.LastSeq != 2 || batch.Gap.Reason != "retention_age" || len(batch.Items) != 0 {
		t.Fatalf("gap missing: %+v %v", batch, err)
	}
	ack := domain.ProbeReplayResult{StreamID: testIdentity().StreamID, CommittedSeq: 2}
	stale := validFence()
	stale.ConnectionGeneration++
	if err := s.CommitReplayACK(t.Context(), stale, ack); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale gap ACK accepted", err)
	}
	if err := s.CommitReplayACK(t.Context(), validFence(), ack); err != nil {
		t.Fatal(err)
	}
	batch, err = s.ReadReplayBatch(t.Context(), 0, 256, 512<<10)
	if err != nil || batch.Gap != nil || len(batch.Items) != 1 || batch.FirstSeq != 3 || batch.LastSeq != 3 {
		t.Fatalf("skipped retained event: %+v %v", batch, err)
	}
	ack.CommittedSeq = 3
	if err := s.CommitReplayACK(t.Context(), validFence(), ack); err != nil {
		t.Fatal(err)
	}
	batch, err = s.ReadReplayBatch(t.Context(), 0, 256, 512<<10)
	if err != nil || batch.Gap == nil || batch.FirstSeq != 4 {
		t.Fatalf("tail gap missing: %+v %v", batch, err)
	}
}

func TestRetentionBytePressurePrefersOrdinaryEvidence(t *testing.T) {
	s, _ := retentionStore(t)
	now := time.Now().UTC()
	for seq := int64(1); seq <= 20; seq++ {
		kind := "observation"
		if seq <= 4 {
			kind = "alert.transition"
		}
		insertOutboxRow(t, s, seq, kind, now, bytes.Repeat([]byte("x"), 64<<10))
	}
	setIdentitySeqs(t, s, 20, 0)
	if err := s.SweepRetention(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	d, err := s.ReadDiagnostics(t.Context())
	if err != nil || d.QueueBytes > 1<<20 || !d.QueuePressure || d.GapRanges != 1 || d.Identity.CommittedSeq != 0 {
		t.Fatalf("queue bound: %+v %v", d, err)
	}
	count, err := s.db.NewSelect().Table("edge_telemetry_outbox").Where("kind = 'alert.transition'").Count(t.Context())
	if err != nil || count != 4 {
		t.Fatal("critical evidence evicted before ordinary samples", count, err)
	}
	batch, err := s.ReadReplayBatch(t.Context(), 0, 256, 512<<10)
	if err != nil || batch.FirstSeq != 1 || batch.LastSeq != 4 {
		t.Fatalf("did not stop at durable gap: %+v %v", batch, err)
	}
}

func TestRetentionRollbackAndDelayedACKAfterCoalescing(t *testing.T) {
	s, _ := retentionStore(t)
	now := time.Now().UTC()
	for seq := int64(1); seq <= 10; seq++ {
		insertOutboxRow(t, s, seq, "observation", now.Add(-2*time.Hour), []byte("source"))
	}
	setIdentitySeqs(t, s, 10, 0)
	if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_gap BEFORE INSERT ON edge_gaps BEGIN SELECT RAISE(ABORT, 'gap persistence failed'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepRetention(t.Context(), now); !errors.Is(err, ErrStorage) {
		t.Fatal("failed gap insertion lost evidence", err)
	}
	count, err := s.db.NewSelect().Table("edge_telemetry_outbox").Count(t.Context())
	if err != nil || count != 10 {
		t.Fatal("eviction escaped rollback", count, err)
	}
	if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER fail_gap"); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepRetention(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	ack := domain.ProbeReplayResult{StreamID: testIdentity().StreamID, CommittedSeq: 5}
	if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_gap_ack BEFORE UPDATE ON edge_gaps BEGIN SELECT RAISE(ABORT, 'gap prune failed'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitReplayACK(t.Context(), validFence(), ack); !errors.Is(err, ErrStorage) {
		t.Fatal("late prune failure accepted", err)
	}
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.CommittedSeq != 0 {
		t.Fatal("cursor escaped failed gap ACK", i, err)
	}
	if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER fail_gap_ack"); err != nil {
		t.Fatal(err)
	}
	// A sent event batch or smaller gap can be ACKed after retention coalesces
	// its range. Keep the unacknowledged suffix without resurrecting rows.
	if err := s.CommitReplayACK(t.Context(), validFence(), ack); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ReadReplayBatch(t.Context(), 0, 256, 512<<10)
	if err != nil || batch.Gap == nil || batch.FirstSeq != 6 || batch.LastSeq != 10 {
		t.Fatalf("partial gap ACK lost suffix: %+v %v", batch, err)
	}
}

func TestRetentionBoundsFragmentedLossMetadata(t *testing.T) {
	s, _ := retentionStore(t)
	now := time.Now().UTC()
	// Alternating old samples and fresh critical events create 1,025 ranges.
	// The bounded fallback must explicitly declare the intervening loss too.
	if _, err := s.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < 2050)
 INSERT INTO edge_telemetry_outbox(seq,kind,observed_at,payload)
 SELECT x, CASE WHEN x%2=1 THEN 'observation' ELSE 'alert.transition' END,
 CASE WHEN x%2=1 THEN ? ELSE ? END, 'source' FROM n`, now.Add(-2*time.Hour).UnixMicro(), now.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	setIdentitySeqs(t, s, 2050, 0)
	for n := 0; n < 3; n++ {
		if err := s.SweepRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
	}
	var gaps []retainedGap
	if err := s.db.NewSelect().Model(&gaps).Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].FromSeq != 1 || gaps[0].ThroughSeq != 2049 || gaps[0].Reason != "disk_pressure" || gaps[0].ObservedThrough != now.UnixMicro() {
		t.Fatalf("loss was unbounded or hidden: %+v", gaps)
	}
	count, err := s.db.NewSelect().Table("edge_telemetry_outbox").Where("seq = 2050").Count(t.Context())
	if err != nil || count != 1 {
		t.Fatal("unrelated evidence removed", count, err)
	}
	batch, err := s.ReadReplayBatch(t.Context(), 0, 256, 512<<10)
	if err != nil || batch.Gap == nil || batch.LastSeq != 2049 {
		t.Fatalf("loss is not replayable: %+v %v", batch, err)
	}
}

func TestRetentionSmallQueueLeasesBoundedDeliveriesAndPreservesOutcomes(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	if err := WithRetentionPolicy(RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour})(s); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	var original edgeDeliveryRow
	if err := s.db.NewSelect().Model(&original).Limit(1).Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 19; n++ {
		row := original
		row.DeliveryID = uuid.NewString()
		if _, err := s.db.NewInsert().Model(&row).Exec(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for seq := before.LastCreatedSeq + 1; seq <= before.LastCreatedSeq+14; seq++ {
		insertOutboxRow(t, s, seq, "observation", now, bytes.Repeat([]byte("x"), 64<<10))
	}
	setIdentitySeqs(t, s, before.LastCreatedSeq+14, 0)
	claims, err := s.ClaimDeliveries(t.Context(), testIdentity().ProbeID, now, time.Minute, 100)
	if err != nil || len(claims) == 0 || len(claims) >= 20 {
		t.Fatalf("small budget starved or overleased: %d %v", len(claims), err)
	}
	// Further requests cannot authorize unreserved work while outcomes are due.
	next, err := s.ClaimDeliveries(t.Context(), testIdentity().ProbeID, now, time.Minute, 100)
	if err != nil || len(next) != 0 {
		t.Fatalf("reservations overcommitted: %d %v", len(next), err)
	}
	for _, item := range claims {
		claim := domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: item.ProbeID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
		if err := s.FinishDelivery(t.Context(), claim, domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: now.Add(time.Second)}); err != nil {
			t.Fatalf("reserved outcome lost: %v", err)
		}
	}
	next, err = s.ClaimDeliveries(t.Context(), testIdentity().ProbeID, now.Add(2*time.Second), time.Minute, 100)
	if err != nil || len(next) != 20-len(claims) {
		t.Fatalf("remaining deliveries did not progress: %d %v", len(next), err)
	}
	d, err := s.ReadDiagnostics(t.Context())
	if err != nil || d.QueueBytes > 1<<20 || d.GapRanges == 0 {
		t.Fatalf("queue bound or gap lost: %+v %v", d, err)
	}
}
