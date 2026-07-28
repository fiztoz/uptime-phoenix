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
