package probe

import (
	"testing"
	"time"
)

// TestProbeQueueReportCarriesTheValidatedHealthQueue pins #157 at the protocol
// boundary: the queue the hub hands to the established callback is exactly
// what a validated probe health frame stated, an empty queue stays an empty
// queue, and the oldest queued item is UTC with sub-second precision intact.
func TestProbeQueueReportCarriesTheValidatedHealthQueue(t *testing.T) {
	fixture := readFixture(t, "valid", "health-probe-empty.json")
	_, empty, err := DecodeHealth(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if got := probeQueueReport(empty); got.QueueBytes != 0 || got.OldestQueuedAt != nil {
		t.Fatalf("empty probe queue changed: %+v", got)
	}

	_, queued, err := DecodeHealth(changePayload(t, fixture, map[string]any{"queue_bytes": 4096, "oldest_queued_at": "2026-10-28T02:35:00.123456Z"}))
	if err != nil {
		t.Fatal(err)
	}
	got := probeQueueReport(queued)
	want := time.Date(2026, 10, 28, 2, 35, 0, 123456000, time.UTC)
	if got.QueueBytes != 4096 || got.OldestQueuedAt == nil || !got.OldestQueuedAt.Equal(want) || got.OldestQueuedAt.Location() != time.UTC {
		t.Fatalf("queued probe report changed: %+v", got)
	}
}
