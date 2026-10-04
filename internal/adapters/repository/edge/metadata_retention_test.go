package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func metadataBytes(t *testing.T, s *Store) int64 {
	t.Helper()
	var actual, counter int64
	if err := s.db.NewRaw(`SELECT
 (SELECT COALESCE(SUM(length(protected_payload)+512),0) FROM edge_config) +
 (SELECT COUNT(*)*256 FROM edge_assignments) +
 (SELECT COALESCE(SUM(length(current_observation)+512),0) FROM edge_regional_state) +
 (SELECT COALESCE(SUM(length(CAST(reason AS BLOB))+length(CAST(COALESCE(ack_actor_display_name,'') AS BLOB))+length(CAST(COALESCE(ack_note,'') AS BLOB))+1024),0) FROM edge_alerts) +
 (SELECT COUNT(*)*512 FROM edge_watchdog_state)`).Scan(t.Context(), &actual); err != nil {
		t.Fatal(err)
	}
	if err := s.db.NewRaw("SELECT used_bytes FROM edge_metadata_budget WHERE id=1").Scan(t.Context(), &counter); err != nil || counter != actual {
		t.Fatalf("metadata budget: counter=%d actual=%d error=%v", counter, actual, err)
	}
	return actual
}

func sizedProtectedConfig(t *testing.T, revision int64, size int) domain.EdgeActiveConfig {
	t.Helper()
	c := protectedConfig(t, revision)
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte("x"), size-domain.ProbeConfigProtectionOverhead)
	hash := sha256.Sum256(plain)
	c.Snapshot.SHA256 = hex.EncodeToString(hash[:])
	c.Snapshot.ProtectedPayload, err = p.Seal(t.Context(), c.Snapshot.ProbeConfigMetadata, plain)
	if err != nil || len(c.Snapshot.ProtectedPayload) != size {
		t.Fatalf("protected fixture: %v size=%d", err, len(c.Snapshot.ProtectedPayload))
	}
	return c
}

func TestEdgeMetadataQuotaRollsBackAndCleanupRestoresAdmission(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	s.retention = RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}
	for revision := int64(1); revision <= 4; revision++ {
		size := 16 << 20
		if revision == 4 {
			size = maxMetadataBytes - int(metadataBytes(t, s)) - 512
		}
		if err := s.ActivateConfig(t.Context(), sizedProtectedConfig(t, revision, size)); err != nil {
			t.Fatal(err)
		}
	}
	if used := metadataBytes(t, s); used != maxMetadataBytes {
		t.Fatalf("quota fixture: %d", used)
	}
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 5)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("over-quota activation: %v", err)
	}
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.ConfigRevision != 4 || metadataBytes(t, s) != maxMetadataBytes {
		t.Fatalf("failed activation changed durable selection: %+v %v", i, err)
	}
	d, err := s.ReadDiagnostics(t.Context())
	if err != nil || !d.MetadataPressure || d.MetadataBytes != maxMetadataBytes {
		t.Fatalf("quota pressure absent: %+v %v", d, err)
	}
	if err := s.SweepRetention(t.Context(), time.Now().UTC().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if metadataBytes(t, s) >= maxMetadataBytes/2 {
		t.Fatal("eligible old configs did not release quota")
	}
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 5)); err != nil {
		t.Fatalf("activation after cleanup: %v", err)
	}
	metadataBytes(t, s)
}

func metadataMigration(t *testing.T, s *Store, direction string) error {
	t.Helper()
	script, err := migrations.ReadFile("migrations/010_metadata_retention.tx." + direction + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	return s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, string(script))
		return err
	})
}

func TestEdgeMetadataMigrationAndGenerationTombstone(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	s.retention = RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}
	before := metadataBytes(t, s)
	if err := metadataMigration(t, s, "down"); err != nil {
		t.Fatal(err)
	}
	if err := metadataMigration(t, s, "up"); err != nil {
		t.Fatal(err)
	}
	if metadataBytes(t, s) != before {
		t.Fatal("upgrade changed populated metadata")
	}
	removed := protectedConfig(t, 2)
	removed.Assignments = nil
	if err := s.ActivateConfig(t.Context(), removed); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepRetention(t.Context(), time.Now().UTC().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := metadataMigration(t, s, "down"); err == nil {
		t.Fatal("downgrade lost generation tombstone after config retirement")
	}
	metadataBytes(t, s) // Failed downgrade kept its counter and triggers intact.
	reused := protectedConfig(t, 3)
	if err := s.ActivateConfig(t.Context(), reused); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("retired generation resurrected: %v", err)
	}
	reused.Assignments[0].Generation++
	if err := s.ActivateConfig(t.Context(), reused); err != nil {
		t.Fatalf("new generation blocked: %v", err)
	}
	metadataBytes(t, s)
}

func TestEdgeMetadataCleanupPreservesPendingWorkAndRollsBackLateFailure(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	s.retention = RetentionPolicy{MaxBytes: 1 << 20, MaxAge: 24 * time.Hour}
	now := time.Now().UTC().Truncate(time.Microsecond)
	record := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	metadataBytes(t, s)
	// No result yet: even an old incident/config must remain while delivery is pending.
	old := now.Add(-8 * 24 * time.Hour).UnixMicro()
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_config SET applied_at=?", old); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 2)); err != nil {
		t.Fatal(err)
	}
	before, err := s.ReadReplayBatch(t.Context(), 0, 200, 512<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SweepRetention(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetDeliveryIntent(t.Context(), testIdentity().ProbeID, record.DeliveryIntents[0].DeliveryID)
	if err != nil || item.Status != domain.DeliveryStatusPending {
		t.Fatalf("pending work lost: %+v %v", item, err)
	}
	claims, err := s.ClaimDeliveries(t.Context(), testIdentity().ProbeID, now, time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %v %+v", err, claims)
	}
	if err := s.SweepRetention(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	claim := claims[0]
	if err := s.FinishDelivery(t.Context(), domain.DeliveryClaim{DeliveryID: claim.DeliveryID, ProbeID: claim.ProbeID, Attempt: claim.Attempt, LeaseToken: claim.LeaseToken}, domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	// An unrelated unreferenced config makes deletion the last operation in the
	// cleanup transaction; its injected failure must roll back earlier deletion.
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 3)); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE edge_delivery_outbox SET outcome_at=?",
		"UPDATE edge_config SET applied_at=? WHERE revision=2",
	} {
		if _, err := s.db.ExecContext(t.Context(), query, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_metadata_cleanup BEFORE DELETE ON edge_config BEGIN SELECT RAISE(ABORT,'injected cleanup failure'); END"); err != nil {
		t.Fatal(err)
	}
	budget := metadataBytes(t, s)
	if err := s.SweepRetention(t.Context(), now); !errors.Is(err, ErrStorage) {
		t.Fatalf("cleanup fault: %v", err)
	}
	if _, err := s.GetDeliveryIntent(t.Context(), testIdentity().ProbeID, claim.DeliveryID); err != nil || metadataBytes(t, s) != budget {
		t.Fatalf("late failure partially committed: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER fail_metadata_cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepRetention(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDeliveryIntent(t.Context(), testIdentity().ProbeID, claim.DeliveryID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("terminal delivery not retired: %v", err)
	}
	after, err := s.ReadReplayBatch(t.Context(), 0, 200, 512<<10)
	if err != nil || len(after.Items) <= len(before.Items) || !reflect.DeepEqual(before.Items, after.Items[:len(before.Items)]) {
		t.Fatalf("cleanup altered queued evidence: %v", err)
	}
	evidence, err := s.ReadEdgeEvidence(t.Context(), 17, 1)
	if err != nil || evidence.Incident == nil || evidence.Incident.Status != domain.AlertStatusFiring {
		t.Fatalf("unresolved incident lost: %+v %v", evidence, err)
	}
	metadataBytes(t, s)
}

func TestEdgeMetadataRetentionRemovesUnreferencedHistory(t *testing.T) {
	t.Run("unused configurations", func(t *testing.T) {
		s, _ := testStore(t)
		enroll(t, s)
		s.retention = RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}
		for revision := int64(1); revision <= 64; revision++ {
			if err := s.ActivateConfig(t.Context(), protectedConfig(t, revision)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SweepRetention(t.Context(), time.Now().UTC().Add(400*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_config").Scan(t.Context(), &count); err != nil {
			t.Fatal(err)
		}
		if count > 1 {
			t.Fatalf("64 revisions leave %d configs after retention; only current revision is referenced", count)
		}
	})
	t.Run("unnotified resolved incidents", func(t *testing.T) {
		s, _ := setupEdgeDeliveryStore(t)
		s.retention = RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}
		for n := 1; n <= 32; n++ {
			before, err := s.ReadEdgeEvidence(t.Context(), 17, 1)
			if err != nil {
				t.Fatal(err)
			}
			down := checkRecord()
			down.DeliveryIntents = nil
			down.Incident.SourceAlertID = fmt.Sprintf("%08x-1111-4111-8111-111111111111", n)
			if before.State != nil {
				down.ExpectedStateSeq = before.State.Seq
			}
			if before.Incident != nil {
				down.ExpectedIncidentVersion = before.Incident.TransitionVersion
			}
			observed, err := s.CommitEdgeCheck(t.Context(), down)
			if err != nil {
				t.Fatal(err)
			}
			up := down
			up.ExpectedStateSeq = observed.Seq
			up.ExpectedIncidentVersion = 1
			incident := *down.Incident
			resolved := down.Observation.ObservedAt.Add(time.Millisecond)
			incident.Status = domain.AlertStatusResolved
			incident.TransitionVersion = 2
			incident.ResolvedAt = &resolved
			up.Incident = &incident
			up.Observation.Status = domain.StatusUp
			up.Observation.RawStatus = domain.StatusUp
			up.Observation.DownCount = 0
			up.Observation.ObservedAt = resolved
			up.Observation.ReceivedAt = resolved
			if _, err := s.CommitEdgeCheck(t.Context(), up); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SweepRetention(t.Context(), time.Now().UTC().Add(400*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_alerts").Scan(t.Context(), &count); err != nil {
			t.Fatal(err)
		}
		if count > 1 {
			t.Fatalf("32 DOWN/UP cycles leave %d resolved incidents without deliveries after retention", count)
		}
		metadataBytes(t, s)
	})
}

func TestEdgeMetadataCleanupHorizonAndBoundedProgress(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	s.retention = RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}
	for revision := int64(1); revision <= 600; revision++ {
		if err := s.ActivateConfig(t.Context(), protectedConfig(t, revision)); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []struct {
		days, want int
	}{{6, 600}, {8, 88}, {8, 1}} {
		if err := s.SweepRetention(t.Context(), time.Now().UTC().Add(time.Duration(step.days)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_config").Scan(t.Context(), &count); err != nil || count != step.want {
			t.Fatalf("bounded horizon cleanup: got=%d want=%d error=%v", count, step.want, err)
		}
		metadataBytes(t, s)
	}
}

func TestEdgeMetadataUpgradePreservesOversizedStoreAndAllowsCleanup(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	s.retention = RetentionPolicy{MaxBytes: 1 << 20, MaxAge: time.Hour}
	if err := metadataMigration(t, s, "down"); err != nil {
		t.Fatal(err)
	}
	for revision := int64(1); revision <= 5; revision++ {
		if err := s.ActivateConfig(t.Context(), sizedProtectedConfig(t, revision, 16<<20)); err != nil {
			t.Fatal(err)
		}
	}
	if err := metadataMigration(t, s, "up"); err != nil {
		t.Fatalf("upgrade rejected pre-existing history: %v", err)
	}
	if used := metadataBytes(t, s); used <= maxMetadataBytes {
		t.Fatalf("oversized upgrade fixture: %d", used)
	}
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 6)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("oversized upgrade admitted growth: %v", err)
	}
	if err := s.SweepRetention(t.Context(), time.Now().UTC().Add(8*24*time.Hour)); err != nil {
		t.Fatalf("oversized store could not retire history: %v", err)
	}
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 6)); err != nil {
		t.Fatal(err)
	}
	metadataBytes(t, s)
}
