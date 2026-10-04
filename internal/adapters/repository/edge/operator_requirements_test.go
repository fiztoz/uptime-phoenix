package edge

import "testing"

// TestOperatorStorageBudgetsMatchDocumentedContract freezes the admission
// numbers cited by docs/multi-region/M6_OPERATOR_REQUIREMENTS.md. A change
// here is an operator-visible contract change: update that document in the
// same commit. These are budgets, not a measured load envelope.
func TestOperatorStorageBudgetsMatchDocumentedContract(t *testing.T) {
	if walCheckpointBytes != 16<<20 {
		t.Fatalf("WAL admission threshold = %d, operator doc says 16 MiB", walCheckpointBytes)
	}
	if maxDeliveryQueueBytes != 64<<20 || maxMetadataBytes != 64<<20 {
		t.Fatalf("delivery=%d metadata=%d, operator doc says 64 MiB each", maxDeliveryQueueBytes, maxMetadataBytes)
	}
	if maxResetArchiveBytes != 1<<30 {
		t.Fatalf("stream-reset archive cap = %d, operator doc says 1 GiB", maxResetArchiveBytes)
	}

	defaultTelemetry := int64(536870912) // PROBE_TELEMETRY_MAX_BYTES default
	s := &Store{retention: RetentionPolicy{MaxBytes: defaultTelemetry}}
	const wantDefault = int64(1536 << 20) // max(1 GiB, 2×512 MiB + 512 MiB)
	if got := s.databaseByteLimit(); got != wantDefault {
		t.Fatalf("default page cap = %d, want %d", got, wantDefault)
	}

	s.retention.MaxBytes = 1 << 20
	if got := s.databaseByteLimit(); got != 1<<30 {
		t.Fatalf("small telemetry page cap = %d, want the 1 GiB floor", got)
	}
}
