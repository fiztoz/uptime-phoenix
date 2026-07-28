package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// spReorderFixture builds a status page with three monitors already assigned,
// and returns their ids in creation order.
func spReorderFixture(t *testing.T, repo *Repository) (int64, [3]int64) {
	t.Helper()
	ctx := context.Background()

	user := &domain.User{Username: "spuser", PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	sp := &domain.StatusPage{Slug: "sp", Title: "SP", Theme: "auto", Published: true}
	if err := repo.StatusPageRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create status page: %v", err)
	}

	var ids [3]int64
	for i, name := range []string{"m1", "m2", "m3"} {
		m := &domain.Monitor{
			UserID: user.ID, Name: name, Type: "http", Active: true,
			Interval: 60, Timeout: 30.0,
			Config: map[string]any{"url": "https://example.com"},
		}
		if err := repo.MonitorRepo.Create(ctx, m); err != nil {
			t.Fatalf("create monitor %q: %v", name, err)
		}
		ids[i] = m.ID
		if err := repo.StatusPageMonitorRepo.AddMonitor(ctx, sp.ID, m.ID, 1000); err != nil {
			t.Fatalf("add monitor %q: %v", name, err)
		}
	}
	return sp.ID, ids
}

// assignments returns the current links in display_order ASC — the order the
// public status page renders them in (ListByStatusPage orders by display_order).
func assignments(t *testing.T, repo *Repository, spID int64) []*domain.StatusPageMonitor {
	t.Helper()
	links, err := repo.StatusPageMonitorRepo.ListByStatusPage(context.Background(), spID)
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	return links
}

// ReorderMonitors is the ONLY safe way to reorder: the alternative the UI used
// to do (remove + re-add) drops the row between two calls and loses the
// assignment on a network error. It replaces the whole set in one transaction.
//
// NOTE: the MariaDB implementation of this method is written differently
// (delete-absent + ON DUPLICATE KEY UPDATE upsert, vs delete-all + insert here)
// and is NOT covered by these tests — repo tests are SQLite-only. Any change
// here must be smoke-run against real MariaDB; see scripts/reorder_smoke.py.
func TestStatusPageMonitorReorder(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()
	spID, ids := spReorderFixture(t, repo)
	m1, m2, m3 := ids[0], ids[1], ids[2]

	if err := repo.StatusPageMonitorRepo.ReorderMonitors(ctx, spID, []int64{m3, m1, m2}); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	links := assignments(t, repo, spID)
	if len(links) != 3 {
		t.Fatalf("want 3 assignments after reorder, got %d", len(links))
	}

	// Assert the EFFECT — the persisted sequence and the spacing — not just that
	// the call returned nil.
	wantOrder := []int64{m3, m1, m2}
	for i, link := range links {
		if link.MonitorID != wantOrder[i] {
			t.Errorf("position %d: got monitor %d, want %d", i, link.MonitorID, wantOrder[i])
		}
		if want := (i + 1) * 10; link.DisplayOrder != want {
			t.Errorf("monitor %d: display_order = %d, want %d", link.MonitorID, link.DisplayOrder, want)
		}
	}
}

// The endpoint is a replace-set: a monitor absent from the list is REMOVED.
// This is deliberate (it is what lets one PUT express any reordering), but it
// means a caller sending a partial list silently drops assignments — so pin the
// behavior down rather than let it drift.
func TestStatusPageMonitorReorderDropsOmittedMonitors(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()
	spID, ids := spReorderFixture(t, repo)
	m1, m2, m3 := ids[0], ids[1], ids[2]

	if err := repo.StatusPageMonitorRepo.ReorderMonitors(ctx, spID, []int64{m2, m1}); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	links := assignments(t, repo, spID)
	if len(links) != 2 {
		t.Fatalf("want 2 assignments, got %d", len(links))
	}
	for _, link := range links {
		if link.MonitorID == m3 {
			t.Fatalf("monitor %d was omitted from the reorder but is still assigned", m3)
		}
	}
	if links[0].MonitorID != m2 || links[1].MonitorID != m1 {
		t.Errorf("got order [%d %d], want [%d %d]", links[0].MonitorID, links[1].MonitorID, m2, m1)
	}
}

// An empty list clears every assignment. This is the branch where the SQLite and
// MariaDB implementations diverge structurally, so it is worth pinning: after a
// clear, a monitor must be re-addable (no orphaned unique-key row left behind).
func TestStatusPageMonitorReorderEmptyClearsAndAllowsReAdd(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()
	spID, ids := spReorderFixture(t, repo)

	if err := repo.StatusPageMonitorRepo.ReorderMonitors(ctx, spID, nil); err != nil {
		t.Fatalf("reorder to empty: %v", err)
	}
	if links := assignments(t, repo, spID); len(links) != 0 {
		t.Fatalf("want 0 assignments after clearing, got %d", len(links))
	}

	if err := repo.StatusPageMonitorRepo.AddMonitor(ctx, spID, ids[0], 1000); err != nil {
		t.Fatalf("re-add after clear: %v", err)
	}
	if links := assignments(t, repo, spID); len(links) != 1 {
		t.Fatalf("want 1 assignment after re-add, got %d", len(links))
	}
}

// The unique(status_page_id, monitor_id) constraint must surface as a typed
// ErrConflict, not a raw driver error — the service turns exactly this into
// ports.ErrMonitorAlreadyLinked, which is what produces the 409's message.
func TestStatusPageMonitorAddDuplicateIsConflict(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()
	spID, ids := spReorderFixture(t, repo)

	err := repo.StatusPageMonitorRepo.AddMonitor(ctx, spID, ids[0], 1000)
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("duplicate add: got %v, want ports.ErrConflict", err)
	}
}
