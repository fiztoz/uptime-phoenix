package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestMigration015_AlertLifecycle_MariaDB exercises the F2.2 schema and
// repository against real MariaDB. SQLite accepts several timestamp and unique
// constraint shapes that MariaDB does not, so the SQLite lifecycle test cannot
// stand in for this contract.
func TestMigration015_AlertLifecycle_MariaDB(t *testing.T) {
	repos := mariadbFactory(t)
	runAlertLifecycleContract(t, repos)
}

func runAlertLifecycleContract(t *testing.T, repos repositorySet) {
	t.Helper()
	ctx := context.Background()
	user := createUser(t, ctx, repos, "alert-lifecycle-user")
	monitor := createMonitor(t, ctx, repos, user.ID, "Alert Lifecycle")

	monitorID := monitor.ID
	firedAt := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	first := &domain.Alert{
		MonitorID:     monitor.ID,
		Status:        domain.AlertStatusFiring,
		Message:       "Alert Lifecycle is DOWN",
		FiredAt:       firedAt,
		AckToken:      "token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		OpenMonitorID: &monitorID,
	}
	if err := repos.alerts.Create(ctx, first); err != nil {
		t.Fatalf("create first open alert: %v", err)
	}
	if first.ID == 0 || first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Fatalf("created alert did not receive identity/timestamps: %+v", first)
	}

	duplicateOpen := &domain.Alert{
		MonitorID: monitor.ID, Status: domain.AlertStatusFiring, Message: "duplicate",
		FiredAt: firedAt, AckToken: "token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		OpenMonitorID: &monitorID,
	}
	if err := repos.alerts.Create(ctx, duplicateOpen); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("second open alert error = %v; want ErrConflict", err)
	}

	open, getOpenErr := repos.alerts.GetOpenByMonitorID(ctx, monitor.ID)
	if getOpenErr != nil {
		t.Fatalf("get open alert: %v", getOpenErr)
	}
	if open.ID != first.ID || open.Status != domain.AlertStatusFiring {
		t.Fatalf("open alert = %+v; want firing id %d", open, first.ID)
	}

	ackedAt := firedAt.Add(time.Minute)
	open.Status = domain.AlertStatusAcked
	open.AckedAt = &ackedAt
	open.AckedByUserID = &user.ID
	if err := repos.alerts.Update(ctx, open); err != nil {
		t.Fatalf("persist acknowledgement: %v", err)
	}
	byToken, getTokenErr := repos.alerts.GetByAckToken(ctx, first.AckToken)
	if getTokenErr != nil {
		t.Fatalf("get acknowledged alert by token: %v", getTokenErr)
	}
	if byToken.Status != domain.AlertStatusAcked ||
		byToken.AckedByUserID == nil ||
		*byToken.AckedByUserID != user.ID {
		t.Fatalf("acknowledgement did not round-trip: %+v", byToken)
	}
	if byToken.OpenMonitorID == nil || *byToken.OpenMonitorID != monitor.ID {
		t.Fatalf("acknowledgement closed the outage: open_monitor_id=%v", byToken.OpenMonitorID)
	}

	resolvedAt := firedAt.Add(2 * time.Hour)
	byToken.Status = domain.AlertStatusResolved
	byToken.ResolvedAt = &resolvedAt
	byToken.OpenMonitorID = nil
	if err := repos.alerts.Update(ctx, byToken); err != nil {
		t.Fatalf("persist resolution: %v", err)
	}
	if _, err := repos.alerts.GetOpenByMonitorID(ctx, monitor.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("get open after resolution = %v; want ErrNotFound", err)
	}

	secondMonitorID := monitor.ID
	second := &domain.Alert{
		MonitorID: monitor.ID, Status: domain.AlertStatusFiring, Message: "outage two",
		FiredAt: firedAt, AckToken: "token-cccccccccccccccccccccccccccccc",
		OpenMonitorID: &secondMonitorID,
	}
	if err := repos.alerts.Create(ctx, second); err != nil {
		t.Fatalf("create a new alert after resolution: %v", err)
	}

	// fired_at is only second precision on MariaDB. The later id must win the
	// tie so the operator sees the true alert sequence.
	list, listErr := repos.alerts.List(ctx, ports.AlertFilter{})
	if listErr != nil {
		t.Fatalf("list alerts: %v", listErr)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf(
			"same-second alert order = %v; want later ids [%d %d]",
			alertIDs(list),
			second.ID,
			first.ID,
		)
	}

	openOnly, openListErr := repos.alerts.List(ctx, ports.AlertFilter{
		OpenOnly:             true,
		RestrictToMonitorIDs: true,
		MonitorIDs:           []int64{monitor.ID},
	})
	if openListErr != nil {
		t.Fatalf("list open scoped alerts: %v", openListErr)
	}
	if len(openOnly) != 1 || openOnly[0].ID != second.ID {
		t.Fatalf("open scoped list = %v; want only %d", alertIDs(openOnly), second.ID)
	}
}

func alertIDs(alerts []*domain.Alert) []int64 {
	ids := make([]int64, len(alerts))
	for i, alert := range alerts {
		ids[i] = alert.ID
	}
	return ids
}
