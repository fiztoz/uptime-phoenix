package sqlite

// UserPermissionRepo against a real SQLite database.
//
// Grant is an upsert built from hand-written ON CONFLICT SQL with a conflict
// target chosen per grant type. None of that is reachable through the in-memory
// fakes the service tests use — those reimplement the behavior in Go — so a
// broken conflict target here would fail only at runtime, in production, on the
// one path (re-granting) that the fakes model correctly by construction.

import (
	"context"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func newPermRepo(t *testing.T) (*UserPermissionRepo, context.Context) {
	t.Helper()
	sqlDB := migratedDB(t)
	db := bun.NewDB(sqlDB, sqlitedialect.New())
	ctx := context.Background()

	if _, err := sqlDB.Exec(`INSERT INTO users (id, username, password_hash) VALUES (1, 'u', 'h')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO monitor_groups (id, user_id, name) VALUES (1, 1, 'g')`); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO monitors (id, user_id, name, type, check_interval, config)
		VALUES (1, 1, 'm', 'http', 60, '{}')`); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	return NewUserPermissionRepo(db), ctx
}

func onlyGrant(t *testing.T, repo *UserPermissionRepo, ctx context.Context) *domain.UserPermission {
	t.Helper()
	grants, err := repo.ListByUser(ctx, 1)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("got %d grants; want exactly 1 — the upsert inserted a duplicate instead of updating", len(grants))
	}
	return grants[0]
}

// Re-granting a group with a different reach updates the existing row rather
// than erroring or silently doing nothing.
func TestUserPermissionRepo_GrantGroupUpsertsTheReach(t *testing.T) {
	repo, ctx := newPermRepo(t)
	groupID := int64(1)

	if err := repo.Grant(ctx, &domain.UserPermission{UserID: 1, GroupID: &groupID, IncludeDescendants: true}); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if got := onlyGrant(t, repo, ctx); !got.IncludeDescendants {
		t.Fatal("the first grant did not store include_descendants=true")
	}

	// Same target, narrower reach.
	if err := repo.Grant(ctx, &domain.UserPermission{UserID: 1, GroupID: &groupID, IncludeDescendants: false}); err != nil {
		t.Fatalf("re-grant shallow: %v", err)
	}
	if got := onlyGrant(t, repo, ctx); got.IncludeDescendants {
		t.Error("re-granting shallow left the row deep; the ON CONFLICT DO UPDATE is not taking effect")
	}

	// And back again — the widening direction must work too.
	if err := repo.Grant(ctx, &domain.UserPermission{UserID: 1, GroupID: &groupID, IncludeDescendants: true}); err != nil {
		t.Fatalf("re-grant deep: %v", err)
	}
	if got := onlyGrant(t, repo, ctx); !got.IncludeDescendants {
		t.Error("re-granting deep left the row shallow")
	}
}

// A monitor grant conflicts on a DIFFERENT unique index than a group grant, so
// it needs its own conflict target. Naming the wrong one raises a constraint
// error instead of upserting — this is the test that catches that.
func TestUserPermissionRepo_GrantMonitorIsIdempotent(t *testing.T) {
	repo, ctx := newPermRepo(t)
	monitorID := int64(1)

	for i := 0; i < 2; i++ {
		if err := repo.Grant(ctx, &domain.UserPermission{UserID: 1, MonitorID: &monitorID}); err != nil {
			t.Fatalf("grant #%d: %v", i+1, err)
		}
	}
	got := onlyGrant(t, repo, ctx)
	if got.MonitorID == nil || *got.MonitorID != monitorID {
		t.Errorf("stored grant points at %v; want monitor %d", got.MonitorID, monitorID)
	}
}

// A shallow grant must survive the round trip through the driver. The column is
// NOT NULL DEFAULT 1, so a model that omitted it on write (Bun's nullzero) would
// let the DB fill in 1 and turn every shallow grant deep on the way to disk —
// silently, and only against a real database.
func TestUserPermissionRepo_ShallowGrantSurvivesTheRoundTrip(t *testing.T) {
	repo, ctx := newPermRepo(t)
	groupID := int64(1)

	if err := repo.Grant(ctx, &domain.UserPermission{UserID: 1, GroupID: &groupID, IncludeDescendants: false}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := onlyGrant(t, repo, ctx); got.IncludeDescendants {
		t.Error("a shallow grant read back deep; the false value never reached the column")
	}
}
