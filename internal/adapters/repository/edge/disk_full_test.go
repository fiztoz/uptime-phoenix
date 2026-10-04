package edge

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
)

// TestEdgeDiskFullCriticalCommit runs only on the disposable Linux tmpfs
// described in docs/TESTING.md. It exercises a real kernel ENOSPC,
// not a fake repository or an injected SQLite trigger.
func TestEdgeDiskFullCriticalCommit(t *testing.T) {
	if os.Getenv("PHOENIX_EDGE_DISK_FULL_TEST") != "1" {
		t.Skip("requires isolated /data tmpfs and PHOENIX_EDGE_DISK_FULL_TEST=1")
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("cannot verify isolated tmpfs: %v", err)
	}
	var isolated bool
	for _, line := range strings.Split(string(mounts), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) == 2 && strings.Contains(" "+parts[0]+" ", " /data ") && strings.HasPrefix(parts[1], "tmpfs ") {
			isolated = true
			break
		}
	}
	if !isolated || os.TempDir() != "/data" {
		t.Fatal("refusing to fill a non-isolated filesystem")
	}

	s, dir := testStore(t) // TMPDIR=/data puts the entire edge database on this tmpfs.
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	ctx := t.Context()
	if err := s.ActivateConfig(ctx, protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckWritable(ctx); err != nil {
		t.Fatalf("healthy baseline refused a write: %v", err)
	}
	var checkpoint struct{ Busy, Log, Checkpointed int }
	if err := s.db.NewRaw("PRAGMA wal_checkpoint(TRUNCATE)").Scan(ctx, &checkpoint); err != nil || checkpoint.Busy != 0 {
		t.Fatalf("could not empty WAL before disk pressure: %+v %v", checkpoint, err)
	}

	filler, err := os.CreateTemp("/data", "edge-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = filler.Close()
		_ = os.Remove(filler.Name())
	}()
	block := make([]byte, 1<<20)
	var full bool
	for n := 0; n < 64; n++ { // The harness mounts exactly 32 MiB.
		_, err := filler.Write(block)
		if errors.Is(err, syscall.ENOSPC) {
			full = true
			break
		}
		if err != nil {
			t.Fatalf("unexpected tmpfs write error: %v", err)
		}
	}
	if !full {
		t.Fatal("test did not exhaust the isolated tmpfs")
	}

	record := checkRecord() // DOWN + incident + delivery intent, two telemetry events.
	got, err := s.CommitEdgeCheck(ctx, record)
	if !errors.Is(err, ErrStorage) || got.Seq != 0 || err.Error() != ErrStorage.Error() {
		t.Fatalf("full disk falsely reported a durable commit: seq=%d err=%v", got.Seq, err)
	}
	if err := s.CheckWritable(ctx); !errors.Is(err, ErrStorage) {
		t.Fatalf("full disk falsely reported storage healthy: %v", err)
	}
	diagnostics, err := s.ReadDiagnostics(ctx)
	if err != nil || diagnostics.Identity.LastCreatedSeq != 0 || diagnostics.QueueBytes != 0 {
		t.Fatalf("full disk hid readable failure diagnostics: %+v %v", diagnostics, err)
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filler.Name()); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatalf("reopen after ENOSPC: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	identity, err := reopened.ReadIdentity(ctx)
	if err != nil || identity.LastCreatedSeq != 0 {
		t.Fatalf("failed commit advanced stream after restart: %+v %v", identity, err)
	}
	for _, table := range []string{"edge_regional_state", "edge_telemetry_outbox", "edge_alerts", "edge_delivery_outbox"} {
		var count int
		if err := reopened.db.NewRaw("SELECT COUNT(*) FROM "+table).Scan(ctx, &count); err != nil || count != 0 {
			t.Fatalf("%s escaped failed transaction: %d %v", table, count, err)
		}
	}
	if err := reopened.CheckWritable(ctx); err != nil {
		t.Fatalf("writable health did not recover after freeing space: %v", err)
	}
	committed, err := reopened.CommitEdgeCheck(ctx, record)
	if err != nil || committed.Seq != 1 {
		t.Fatalf("retry did not commit original observation once: seq=%d err=%v", committed.Seq, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := Open(ctx, dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = final.Close() }()
	identity, err = final.ReadIdentity(ctx)
	if err != nil || identity.LastCreatedSeq != 2 {
		t.Fatalf("recovered stream did not persist two events: %+v %v", identity, err)
	}
	evidence, err := final.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || evidence.State == nil || evidence.State.Seq != 1 || evidence.Incident == nil || evidence.Incident.SourceAlertID != record.Incident.SourceAlertID {
		t.Fatalf("retry evidence missing after restart: %+v %v", evidence, err)
	}
	var events, intents int
	if err := final.db.NewRaw("SELECT COUNT(*) FROM edge_telemetry_outbox").Scan(ctx, &events); err != nil || events != 2 {
		t.Fatalf("expected one observation and one transition: %d %v", events, err)
	}
	if err := final.db.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox").Scan(ctx, &intents); err != nil || intents != 1 {
		t.Fatalf("expected exactly one delivery intent: %d %v", intents, err)
	}
}
