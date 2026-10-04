package handlers

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestFilterHeartbeats_ImportantOnly(t *testing.T) {
	heartbeats := []*domain.Heartbeat{
		{ID: 1, Important: true},
		{ID: 2, Important: false},
		{ID: 3, Important: true},
	}

	trueVal := true
	filtered := filterHeartbeats(heartbeats, &trueVal)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 important heartbeats, got %d", len(filtered))
	}

	filteredAll := filterHeartbeats(heartbeats, nil)
	if len(filteredAll) != 3 {
		t.Fatalf("expected all heartbeats, got %d", len(filteredAll))
	}
}

func TestSortHeartbeats_Order(t *testing.T) {
	now := time.Now().UTC()
	heartbeats := []*domain.Heartbeat{
		{ID: 1, Time: now.Add(-2 * time.Hour)},
		{ID: 2, Time: now},
		{ID: 3, Time: now.Add(-1 * time.Hour)},
	}

	desc := sortHeartbeats(heartbeats, "desc")
	if desc[0].ID != 2 || desc[len(desc)-1].ID != 1 {
		t.Fatalf("desc sort failed: %+v", desc)
	}

	asc := sortHeartbeats(heartbeats, "asc")
	if asc[0].ID != 1 || asc[len(asc)-1].ID != 2 {
		t.Fatalf("asc sort failed: %+v", asc)
	}
}

// TestSortHeartbeats_TimestampTieBreaksByID constructs the same-second tie:
// heartbeats.time is second-precision on MariaDB, and rows written within one
// second carry an identical Time. The id is the monotonic chronological key,
// so asc must end on the higher id and desc must start on it — deterministic,
// not sort.Slice luck. Mutation-checked against the fixture's live failure:
// without the tie-break, asc order of two same-second rows is arbitrary.
func TestSortHeartbeats_TimestampTieBreaksByID(t *testing.T) {
	tied := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	// Deliberately out of id order so a Time-only sort has no stable input;
	// three tied rows make both orders fail without the tie-break.
	heartbeats := []*domain.Heartbeat{
		{ID: 1000, Time: tied, Status: domain.StatusPending},
		{ID: 998, Time: tied, Status: domain.StatusUp},
		{ID: 999, Time: tied, Status: domain.StatusDown},
	}

	asc := sortHeartbeats(heartbeats, "asc")
	if asc[0].ID != 998 || asc[1].ID != 999 || asc[2].ID != 1000 {
		t.Fatalf("asc tie-break failed: [%d %d %d]", asc[0].ID, asc[1].ID, asc[2].ID)
	}

	desc := sortHeartbeats(heartbeats, "desc")
	if desc[0].ID != 1000 || desc[1].ID != 999 || desc[2].ID != 998 {
		t.Fatalf("desc tie-break failed: [%d %d %d]", desc[0].ID, desc[1].ID, desc[2].ID)
	}
}

func TestLimitHeartbeats(t *testing.T) {
	heartbeats := []*domain.Heartbeat{
		{ID: 1}, {ID: 2}, {ID: 3},
	}

	limited := limitHeartbeats(heartbeats, 2)
	if len(limited) != 2 {
		t.Fatalf("expected 2 heartbeats, got %d", len(limited))
	}
	if limited[0].ID != 1 || limited[1].ID != 2 {
		t.Fatalf("unexpected limited order: %+v", limited)
	}
}
