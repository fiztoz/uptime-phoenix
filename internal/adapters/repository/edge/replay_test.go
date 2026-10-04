package edge

import (
	"bytes"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func validFence() domain.EdgeReplayFence {
	return domain.EdgeReplayFence{
		HubID:                testHubID,
		ProbeID:              testIdentity().ProbeID,
		StreamID:             testIdentity().StreamID,
		ConnectionGeneration: 1,
	}
}

func insertOutboxRow(t *testing.T, s *Store, seq int64, kind string, observedAt time.Time, payload []byte) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(),
		"INSERT INTO edge_telemetry_outbox (seq, kind, observed_at, payload) VALUES (?, ?, ?, ?)",
		seq, kind, observedAt.UnixMicro(), payload)
	if err != nil {
		t.Fatalf("insert outbox row %d: %v", seq, err)
	}
}

func setIdentitySeqs(t *testing.T, s *Store, lastCreatedSeq, committedSeq int64) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(),
		"UPDATE edge_identity SET last_created_seq = ?, committed_seq = ? WHERE id = 1",
		lastCreatedSeq, committedSeq)
	if err != nil {
		t.Fatalf("set identity seqs: %v", err)
	}
}

func TestReplayRead_ExactBytePreservationAndPayloads(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	ctx := t.Context()

	p1 := []byte{0x00, 0x01, 0xFF, 0xFE, 0x7F, 0x80, 'h', 'e', 'l', 'l', 'o', ' ', 0xF0, 0x9F, 0x9A, 0x80}
	p2 := bytes.Repeat([]byte{0xAB}, 64<<10) // exactly 64 KiB
	p3 := []byte("delivery-payload-exact-bytes-test-verification")

	t1 := time.Date(2026, 9, 20, 10, 0, 0, 123000, time.UTC)
	t2 := time.Date(2026, 9, 20, 10, 0, 1, 456000, time.UTC)
	t3 := time.Date(2026, 9, 20, 10, 0, 2, 789000, time.UTC)

	insertOutboxRow(t, s, 1, domain.ReplayKindObservation, t1, p1)
	insertOutboxRow(t, s, 2, domain.ReplayKindAlertTransition, t2, p2)
	insertOutboxRow(t, s, 3, domain.ReplayKindDeliveryResult, t3, p3)
	setIdentitySeqs(t, s, 3, 0)

	batch, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10)
	if err != nil {
		t.Fatalf("ReadReplayBatch failed: %v", err)
	}

	if batch.StreamID != testIdentity().StreamID {
		t.Fatalf("StreamID = %q, want %q", batch.StreamID, testIdentity().StreamID)
	}
	if batch.FirstSeq != 1 || batch.LastSeq != 3 {
		t.Fatalf("FirstSeq=%d, LastSeq=%d, want 1, 3", batch.FirstSeq, batch.LastSeq)
	}
	if len(batch.Items) != 3 {
		t.Fatalf("len(Items) = %d, want 3", len(batch.Items))
	}

	expectedPayloads := [][]byte{p1, p2, p3}
	expectedKinds := []string{domain.ReplayKindObservation, domain.ReplayKindAlertTransition, domain.ReplayKindDeliveryResult}
	expectedTimes := []time.Time{t1, t2, t3}

	for i, item := range batch.Items {
		if item.Seq != int64(i+1) {
			t.Errorf("item[%d].Seq = %d, want %d", i, item.Seq, i+1)
		}
		if item.Kind != expectedKinds[i] {
			t.Errorf("item[%d].Kind = %q, want %q", i, item.Kind, expectedKinds[i])
		}
		if !item.ObservedAt.Equal(expectedTimes[i]) {
			t.Errorf("item[%d].ObservedAt = %v, want %v", i, item.ObservedAt, expectedTimes[i])
		}
		if item.ObservedAt.Location() != time.UTC {
			t.Errorf("item[%d].ObservedAt.Location() = %v, want UTC", i, item.ObservedAt.Location())
		}
		if !bytes.Equal(item.Payload, expectedPayloads[i]) {
			t.Errorf("item[%d].Payload mismatch: got %d bytes, want %d bytes", i, len(item.Payload), len(expectedPayloads[i]))
		}
	}

	expectedTotalBytes := len(p1) + len(p2) + len(p3)
	if batch.TotalBytes != expectedTotalBytes {
		t.Fatalf("TotalBytes = %d, want %d", batch.TotalBytes, expectedTotalBytes)
	}
}

func TestReplayRead_CountAndByteBounds(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	ctx := t.Context()

	// 1. Validation of caller limits
	for name, tc := range map[string]struct {
		fromSeq   int64
		maxEvents int
		maxBytes  int
	}{
		"negative fromSeq":          {-1, 10, 1024},
		"zero maxEvents":            {0, 0, 1024},
		"negative maxEvents":        {0, -1, 1024},
		"excessive maxEvents (257)": {0, 257, 1024},
		"zero maxBytes":             {0, 10, 0},
		"negative maxBytes":         {0, 10, -1},
		"excessive maxBytes":        {0, 10, (512 << 10) + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.ReadReplayBatch(ctx, tc.fromSeq, tc.maxEvents, tc.maxBytes); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected domain.ErrValidation, got %v", err)
			}
		})
	}

	// 2. Insert 10 rows with 100-byte payloads
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := int64(1); i <= 10; i++ {
		insertOutboxRow(t, s, i, domain.ReplayKindObservation, now, bytes.Repeat([]byte{byte(i)}, 100))
	}
	setIdentitySeqs(t, s, 10, 0)

	// Test count bound: maxEvents = 4 -> returns valid prefix of 4 items
	batch, err := s.ReadReplayBatch(ctx, 0, 4, 512<<10)
	if err != nil {
		t.Fatalf("maxEvents=4 failed: %v", err)
	}
	if len(batch.Items) != 4 || batch.FirstSeq != 1 || batch.LastSeq != 4 {
		t.Fatalf("count bound: len=%d, FirstSeq=%d, LastSeq=%d", len(batch.Items), batch.FirstSeq, batch.LastSeq)
	}
	if batch.TotalBytes != 400 {
		t.Fatalf("TotalBytes = %d, want 400", batch.TotalBytes)
	}

	// Test byte bound: maxBytes = 250 -> returns valid prefix of 2 items (200 bytes <= 250, 3rd would be 300)
	batch, err = s.ReadReplayBatch(ctx, 0, 10, 250)
	if err != nil {
		t.Fatalf("maxBytes=250 failed: %v", err)
	}
	if len(batch.Items) != 2 || batch.FirstSeq != 1 || batch.LastSeq != 2 {
		t.Fatalf("byte bound: len=%d, FirstSeq=%d, LastSeq=%d", len(batch.Items), batch.FirstSeq, batch.LastSeq)
	}
	if batch.TotalBytes != 200 {
		t.Fatalf("TotalBytes = %d, want 200", batch.TotalBytes)
	}

	// Test single event exceeds maxBytes -> domain.ErrValidation
	if _, err := s.ReadReplayBatch(ctx, 0, 10, 50); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("single event exceeds maxBytes: expected domain.ErrValidation, got %v", err)
	}

	// Test corrupt empty payload via temporary table without CHECK constraint
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE temp_outbox AS SELECT * FROM edge_telemetry_outbox; DROP TABLE edge_telemetry_outbox; CREATE TABLE edge_telemetry_outbox (seq INTEGER PRIMARY KEY, kind TEXT NOT NULL, observed_at INTEGER NOT NULL, payload BLOB);"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO edge_telemetry_outbox (seq, kind, observed_at, payload) VALUES (1, 'observation', 1000, X'');"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10); !errors.Is(err, ErrStorage) {
		t.Fatalf("corrupt empty payload: expected ErrStorage, got %v", err)
	}

	// Test corrupt oversized payload (> 64 KiB)
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_telemetry_outbox SET payload = ? WHERE seq = 1", bytes.Repeat([]byte{0xFF}, (64<<10)+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10); !errors.Is(err, ErrStorage) {
		t.Fatalf("oversized payload: expected ErrStorage, got %v", err)
	}

	// Restore original table
	if _, err := s.db.ExecContext(ctx, "DROP TABLE edge_telemetry_outbox; CREATE TABLE edge_telemetry_outbox (seq INTEGER PRIMARY KEY CHECK (typeof(seq) = 'integer' AND seq > 0), kind TEXT NOT NULL CHECK (kind IN ('observation', 'alert.transition', 'delivery.result')), observed_at INTEGER NOT NULL, payload BLOB NOT NULL CHECK (length(payload) BETWEEN 1 AND 65536)); INSERT INTO edge_telemetry_outbox SELECT * FROM temp_outbox; DROP TABLE temp_outbox;"); err != nil {
		t.Fatal(err)
	}
}

func TestReplayRead_MissingFirstMiddleTail(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	ctx := t.Context()

	// 1. Healthy empty queue when last_created_seq == committed_seq
	batch, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10)
	if err != nil {
		t.Fatalf("empty queue read: %v", err)
	}
	if batch.FirstSeq != 1 || batch.LastSeq != 0 || len(batch.Items) != 0 || batch.TotalBytes != 0 {
		t.Fatalf("unexpected empty batch: %+v", batch)
	}

	// 2. Missing first row when outbox is completely empty but last_created_seq = 5
	setIdentitySeqs(t, s, 5, 0)
	if _, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("missing first (empty table): expected ports.ErrConflict, got %v", err)
	}

	// 3. Missing first row when rows [2, 3, 4, 5] exist (seq 1 missing)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := int64(2); i <= 5; i++ {
		insertOutboxRow(t, s, i, domain.ReplayKindObservation, now, []byte("val"))
	}
	if _, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("missing first (seq 1 missing): expected ports.ErrConflict, got %v", err)
	}

	// 4. Missing middle row (insert seq 1, delete seq 3 -> rows [1, 2, 4, 5])
	insertOutboxRow(t, s, 1, domain.ReplayKindObservation, now, []byte("val"))
	if _, err := s.db.ExecContext(ctx, "DELETE FROM edge_telemetry_outbox WHERE seq = 3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("missing middle (seq 3 missing): expected ports.ErrConflict, got %v", err)
	}

	// 5. Missing tail row (restore seq 3, delete seq 5 -> rows [1, 2, 3, 4], last_created_seq = 5)
	insertOutboxRow(t, s, 3, domain.ReplayKindObservation, now, []byte("val"))
	if _, err := s.db.ExecContext(ctx, "DELETE FROM edge_telemetry_outbox WHERE seq = 5"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("missing tail (seq 5 missing): expected ports.ErrConflict, got %v", err)
	}

	// 6. Valid prefix returns successfully even if tail rows exist beyond limit
	insertOutboxRow(t, s, 5, domain.ReplayKindObservation, now, []byte("val"))
	// With maxEvents = 2, returns rows 1, 2 without failing on missing tail
	prefixBatch, err := s.ReadReplayBatch(ctx, 0, 2, 512<<10)
	if err != nil {
		t.Fatalf("valid prefix read: %v", err)
	}
	if len(prefixBatch.Items) != 2 || prefixBatch.FirstSeq != 1 || prefixBatch.LastSeq != 2 {
		t.Fatalf("valid prefix: len=%d, FirstSeq=%d, LastSeq=%d", len(prefixBatch.Items), prefixBatch.FirstSeq, prefixBatch.LastSeq)
	}

	// 7. Explicit fromSeq skipping unsent data -> ports.ErrConflict
	if _, err := s.ReadReplayBatch(ctx, 3, 10, 512<<10); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("skipping unsent data: expected ports.ErrConflict, got %v", err)
	}

	// 8. Explicit fromSeq below committed cursor -> ports.ErrConflict
	setIdentitySeqs(t, s, 5, 2)
	for _, from := range []int64{1, 2} {
		if _, err := s.ReadReplayBatch(ctx, from, 10, 512<<10); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("fromSeq %d below cursor 2: expected ports.ErrConflict, got %v", from, err)
		}
	}
	// Explicit fromSeq matching next sequence (3) succeeds
	if b, err := s.ReadReplayBatch(ctx, 3, 10, 512<<10); err != nil || b.FirstSeq != 3 {
		t.Fatalf("explicit next sequence 3 failed: %+v %v", b, err)
	}
}

func TestReplayRead_OverflowBoundary(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	ctx := t.Context()

	// 1. MaxInt64 exhaustion returning empty batch without overflow
	setIdentitySeqs(t, s, math.MaxInt64, math.MaxInt64)
	batch, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10)
	if err != nil {
		t.Fatalf("MaxInt64 exhaustion read: %v", err)
	}
	if batch.FirstSeq != math.MaxInt64 || batch.LastSeq != math.MaxInt64 || len(batch.Items) != 0 || batch.TotalBytes != 0 {
		t.Fatalf("unexpected MaxInt64 exhausted batch: %+v", batch)
	}

	// Explicit fromSeq cannot equal next sequence when exhausted
	if _, err := s.ReadReplayBatch(ctx, 10, 10, 512<<10); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("explicit fromSeq on exhausted: expected ports.ErrConflict, got %v", err)
	}

	// 2. Event at MaxInt64 sequence
	setIdentitySeqs(t, s, math.MaxInt64, math.MaxInt64-1)
	insertOutboxRow(t, s, math.MaxInt64, domain.ReplayKindObservation, time.Now().UTC().Truncate(time.Microsecond), []byte("max-int-event"))

	batch, err = s.ReadReplayBatch(ctx, 0, 10, 512<<10)
	if err != nil {
		t.Fatalf("MaxInt64 event read: %v", err)
	}
	if len(batch.Items) != 1 || batch.Items[0].Seq != math.MaxInt64 || batch.FirstSeq != math.MaxInt64 || batch.LastSeq != math.MaxInt64 {
		t.Fatalf("MaxInt64 event batch mismatch: %+v", batch)
	}
}

func TestReplayRead_ConcurrentAppendCoherentReads(t *testing.T) {
	s, _ := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	var wg sync.WaitGroup
	const iterations = 25

	// Writer goroutine commits edge checks
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			rec := checkRecord()
			rec.Observation.ObservedAt = time.Now().UTC().Truncate(time.Microsecond)
			rec.Observation.ReceivedAt = rec.Observation.ObservedAt
			if _, err := s.CommitEdgeCheck(ctx, rec); err != nil && !errors.Is(err, ports.ErrStaleLocalState) {
				t.Errorf("CommitEdgeCheck error: %v", err)
				return
			}
		}
	}()

	// Reader goroutine reads batches concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			batch, err := s.ReadReplayBatch(ctx, 0, 10, 512<<10)
			if err != nil {
				t.Errorf("concurrent ReadReplayBatch error: %v", err)
				return
			}
			// Verify coherence: items must be strictly contiguous
			for j := 1; j < len(batch.Items); j++ {
				if batch.Items[j].Seq != batch.Items[j-1].Seq+1 {
					t.Errorf("incoherent sequence gap: %d -> %d", batch.Items[j-1].Seq, batch.Items[j].Seq)
				}
			}
		}
	}()

	wg.Wait()
}

func TestReplayACK_FencingAndIdempotence(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	ctx := t.Context()

	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := int64(1); i <= 5; i++ {
		insertOutboxRow(t, s, i, domain.ReplayKindObservation, now, []byte("val"))
	}
	setIdentitySeqs(t, s, 5, 0)

	fence := validFence()

	// 1. Validation errors on input parameters
	invalidFences := []domain.EdgeReplayFence{
		{HubID: "", ProbeID: fence.ProbeID, StreamID: fence.StreamID, ConnectionGeneration: 1},
		{HubID: fence.HubID, ProbeID: "", StreamID: fence.StreamID, ConnectionGeneration: 1},
		{HubID: fence.HubID, ProbeID: fence.ProbeID, StreamID: "", ConnectionGeneration: 1},
		{HubID: fence.HubID, ProbeID: fence.ProbeID, StreamID: fence.StreamID, ConnectionGeneration: 0},
		{HubID: fence.HubID, ProbeID: fence.ProbeID, StreamID: fence.StreamID, ConnectionGeneration: -1},
	}
	for i, f := range invalidFences {
		if err := s.CommitReplayACK(ctx, f, domain.ProbeReplayResult{StreamID: f.StreamID, CommittedSeq: 1}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid fence [%d]: expected domain.ErrValidation, got %v", i, err)
		}
	}
	if err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: "different-stream", CommittedSeq: 1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("mismatched result stream: expected domain.ErrValidation, got %v", err)
	}
	if err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: -1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative committed seq: expected domain.ErrValidation, got %v", err)
	}

	// 2. Fence mismatches against store
	for name, f := range map[string]domain.EdgeReplayFence{
		"wrong hub":        {HubID: "4d5e29ba-a1c4-4e39-a0c7-f73b35a2c999", ProbeID: fence.ProbeID, StreamID: fence.StreamID, ConnectionGeneration: 1},
		"wrong probe":      {HubID: fence.HubID, ProbeID: "0897874c-a573-457b-8953-893992136999", StreamID: fence.StreamID, ConnectionGeneration: 1},
		"wrong stream":     {HubID: fence.HubID, ProbeID: fence.ProbeID, StreamID: "34f542f2-eab7-4c28-a11b-7746fc0a2999", ConnectionGeneration: 1},
		"stale generation": {HubID: fence.HubID, ProbeID: fence.ProbeID, StreamID: fence.StreamID, ConnectionGeneration: 2},
	} {
		t.Run(name, func(t *testing.T) {
			res := domain.ProbeReplayResult{StreamID: f.StreamID, CommittedSeq: 2}
			if err := s.CommitReplayACK(ctx, f, res); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("expected ports.ErrConflict, got %v", err)
			}
		})
	}

	// 3. Cursor bounds
	// Future cursor > last_created_seq (5)
	if err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 6}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("future cursor > last_created_seq: expected ports.ErrConflict, got %v", err)
	}

	// 4. ACK cannot cross a missing sequence
	if _, err := s.db.ExecContext(ctx, "DELETE FROM edge_telemetry_outbox WHERE seq = 3"); err != nil {
		t.Fatal(err)
	}
	// Try to ACK seq 5 across missing seq 3
	if err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 5}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("ACK across missing sequence: expected ports.ErrConflict, got %v", err)
	}
	// Verify neither cursor nor rows changed
	i, err := s.ReadIdentity(ctx)
	if err != nil || i.CommittedSeq != 0 {
		t.Fatalf("committed cursor changed after failed ACK: %+v %v", i, err)
	}
	var remainingSeqs []int64
	if err := s.db.NewRaw("SELECT seq FROM edge_telemetry_outbox ORDER BY seq").Scan(ctx, &remainingSeqs); err != nil || len(remainingSeqs) != 4 {
		t.Fatalf("outbox rows modified after failed ACK: %v", remainingSeqs)
	}

	// 5. Successful ACK
	insertOutboxRow(t, s, 3, domain.ReplayKindObservation, now, []byte("val"))
	if err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 3}); err != nil {
		t.Fatalf("valid ACK commit failed: %v", err)
	}
	i, err = s.ReadIdentity(ctx)
	if err != nil || i.CommittedSeq != 3 {
		t.Fatalf("committed cursor not advanced to 3: %+v %v", i, err)
	}
	if err := s.db.NewRaw("SELECT seq FROM edge_telemetry_outbox ORDER BY seq").Scan(ctx, &remainingSeqs); err != nil || len(remainingSeqs) != 2 || remainingSeqs[0] != 4 || remainingSeqs[1] != 5 {
		t.Fatalf("rows 1-3 not deleted: %v", remainingSeqs)
	}

	// 6. Repeated ACK at current cursor is idempotent
	if err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 3}); err != nil {
		t.Fatalf("repeated ACK failed: %v", err)
	}
	// But repeated ACK with stale fence must NOT bypass generation check
	staleFence := fence
	staleFence.ConnectionGeneration = 99
	if err := s.CommitReplayACK(ctx, staleFence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 3}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("repeated ACK with stale generation: expected ports.ErrConflict, got %v", err)
	}

	// 7. Backward ACK cursor < committed_seq
	if err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 2}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("backward cursor: expected ports.ErrConflict, got %v", err)
	}
}

func TestReplayACK_LateWriteRollbackCases(t *testing.T) {
	s, _ := testStore(t)
	enroll(t, s)
	ctx := t.Context()

	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := int64(1); i <= 5; i++ {
		insertOutboxRow(t, s, i, domain.ReplayKindObservation, now, []byte("val"))
	}
	setIdentitySeqs(t, s, 5, 0)
	fence := validFence()

	// Case 1: Late delete failure must roll back cursor
	if _, err := s.db.ExecContext(ctx, "CREATE TRIGGER fail_delete BEFORE DELETE ON edge_telemetry_outbox BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END"); err != nil {
		t.Fatal(err)
	}
	err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 3})
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("expected ErrStorage, got %v", err)
	}
	// Verify committed_seq was rolled back to 0
	i, err := s.ReadIdentity(ctx)
	if err != nil || i.CommittedSeq != 0 {
		t.Fatalf("cursor was not rolled back after delete failure: %+v %v", i, err)
	}
	// Verify all rows preserved
	var count int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_telemetry_outbox").Scan(ctx, &count); err != nil || count != 5 {
		t.Fatalf("rows deleted despite rollback: %d %v", count, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_delete"); err != nil {
		t.Fatal(err)
	}

	// Case 2: Late cursor failure must preserve rows
	if _, err := s.db.ExecContext(ctx, "CREATE TRIGGER fail_cursor BEFORE UPDATE OF committed_seq ON edge_identity BEGIN SELECT RAISE(ABORT, 'injected cursor failure'); END"); err != nil {
		t.Fatal(err)
	}
	err = s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 3})
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("expected ErrStorage, got %v", err)
	}
	// Verify committed_seq is still 0
	i, err = s.ReadIdentity(ctx)
	if err != nil || i.CommittedSeq != 0 {
		t.Fatalf("cursor updated despite failure: %+v %v", i, err)
	}
	// Verify all rows preserved
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_telemetry_outbox").Scan(ctx, &count); err != nil || count != 5 {
		t.Fatalf("rows deleted despite cursor update failure: %d %v", count, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_cursor"); err != nil {
		t.Fatal(err)
	}
}

func TestReplay_ReopenPersistence(t *testing.T) {
	s, dir := testStore(t)
	enroll(t, s)
	ctx := t.Context()

	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := int64(1); i <= 6; i++ {
		insertOutboxRow(t, s, i, domain.ReplayKindObservation, now, []byte("val"))
	}
	setIdentitySeqs(t, s, 6, 0)
	fence := validFence()

	// Acknowledge first 3 rows
	if err := s.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 3}); err != nil {
		t.Fatalf("CommitReplayACK: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen store
	reopened, err := Open(ctx, dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()

	// Verify identity persisted
	i, err := reopened.ReadIdentity(ctx)
	if err != nil || i.CommittedSeq != 3 || i.LastCreatedSeq != 6 {
		t.Fatalf("reopened identity mismatch: %+v %v", i, err)
	}

	// ReadReplayBatch from reopened store begins at cursor + 1 (seq 4)
	batch, err := reopened.ReadReplayBatch(ctx, 0, 10, 512<<10)
	if err != nil {
		t.Fatalf("reopened ReadReplayBatch: %v", err)
	}
	if len(batch.Items) != 3 || batch.FirstSeq != 4 || batch.LastSeq != 6 {
		t.Fatalf("reopened batch mismatch: len=%d, FirstSeq=%d, LastSeq=%d", len(batch.Items), batch.FirstSeq, batch.LastSeq)
	}
	for idx, item := range batch.Items {
		if item.Seq != int64(idx+4) {
			t.Errorf("item[%d].Seq = %d, want %d", idx, item.Seq, idx+4)
		}
	}

	// Acknowledge remaining rows
	if err := reopened.CommitReplayACK(ctx, fence, domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 6}); err != nil {
		t.Fatalf("reopened CommitReplayACK: %v", err)
	}

	// Empty queue after full ACK
	batch, err = reopened.ReadReplayBatch(ctx, 0, 10, 512<<10)
	if err != nil {
		t.Fatalf("empty queue after ACK: %v", err)
	}
	if len(batch.Items) != 0 || batch.FirstSeq != 7 || batch.LastSeq != 6 {
		t.Fatalf("expected empty queue: %+v", batch)
	}
}
