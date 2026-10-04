package repository_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestAlertSourceContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, probeRegistryFixture){
				"IdentityAndScope": testAlertSourceIdentity,
				"Transitions":      testAlertSourceTransitions,
				"Migration":        testAlertSourceMigration,
			} {
				t.Run(name, func(t *testing.T) { test(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func alertBySource(t *testing.T, repo ports.AlertRepository, source string) *domain.Alert {
	t.Helper()
	a, err := repo.(ports.AlertSourceRepository).GetBySourceAlertID(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testAlertSourceIdentity(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	r := alertScopeRepos(f).alerts
	mid := localMonitor(t, f)
	at := time.Now().UTC().Truncate(time.Second)
	a := openScopedAlert(t, r, mid, at)
	if id, err := uuid.Parse(a.SourceAlertID); err != nil || id == uuid.Nil || a.TransitionVersion != 1 {
		t.Fatalf("missing source identity/version: %s %d", a.SourceAlertID, a.TransitionVersion)
	}
	// Compare persisted rows: MariaDB truncates existing alert timestamps to seconds.
	byID, err := r.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := alertBySource(t, r, strings.ToUpper(a.SourceAlertID)); !reflect.DeepEqual(got, byID) || got.AckToken != a.AckToken {
		t.Fatal("source lookup changed persisted legacy identity or token")
	}
	if _, err := r.GetByAckToken(ctx, a.SourceAlertID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("public identity accepted as acknowledgement credential")
	}
	remote := scopedAlerts(t, r, probeRegistryID1, 1)
	b := openScopedAlert(t, remote, mid, at)
	if b.SourceAlertID == a.SourceAlertID || alertBySource(t, remote, b.SourceAlertID).ID != b.ID {
		t.Fatal("regional sources share identity")
	}
	for _, lookup := range []struct {
		repo   ports.AlertRepository
		source string
	}{
		{r, b.SourceAlertID}, {remote, a.SourceAlertID}, {scopedAlerts(t, r, "local", 2), a.SourceAlertID},
	} {
		if _, err := lookup.repo.(ports.AlertSourceRepository).GetBySourceAlertID(ctx, lookup.source); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("source read escaped assignment: %v", err)
		}
	}
	otherMid := localMonitor(t, f)
	copy := *a
	copy.ID, copy.MonitorID, copy.OpenMonitorID, copy.AckToken = 0, otherMid, &otherMid, "another-token"
	if err = r.Create(ctx, &copy); !errors.Is(err, ports.ErrConflict) || copy.ID != 0 {
		t.Fatal("duplicate source accepted or failed insert returned ID")
	}
	for _, bad := range []string{"bad", uuid.Nil.String()} {
		copy.SourceAlertID = bad
		if err = r.Create(ctx, &copy); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid source accepted")
		}
	}
	copy.SourceAlertID, copy.TransitionVersion = "", 2
	if err = r.Create(ctx, &copy); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("caller assigned initial transition version")
	}
	copy.TransitionVersion = 0
	clear := injectOutboxFailure(t, f, "alerts", "INSERT")
	err = r.Create(ctx, &copy)
	clear()
	if err == nil || copy.ID != 0 || copy.SourceAlertID != "" || copy.TransitionVersion != 0 {
		t.Fatal("failed create leaked source identity")
	}
	// A new connection sees the same source identity after reconstructing the service.
	var reopened ports.AlertRepository
	if f.engine == "sqlite" {
		db, err := sqlite.NewDB(f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		reopened = sqlite.NewAlertRepo(db)
	} else {
		db, err := mariadb.NewDB(f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		reopened = mariadb.NewAlertRepo(db)
	}
	if got := openScopedAlert(t, reopened, mid, at); got.SourceAlertID != a.SourceAlertID || got.AckToken != a.AckToken {
		t.Fatal("restart changed source identity or token")
	}
	if err := services.NewAlertService(r).ResolveOpen(ctx, mid, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	next := openScopedAlert(t, r, mid, at.Add(2*time.Minute))
	if next.SourceAlertID == a.SourceAlertID || next.TransitionVersion != 1 {
		t.Fatal("new outage reused source identity or version")
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM monitors WHERE id = ?", mid); err != nil {
		t.Fatal(err)
	}
	if _, err := r.(ports.AlertSourceRepository).GetBySourceAlertID(ctx, a.SourceAlertID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("deleted monitor retained alert mapping")
	}
}

func testAlertSourceTransitions(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	r := alertScopeRepos(f).alerts
	svc := services.NewAlertService(r)
	a := openScopedAlert(t, r, localMonitor(t, f), time.Now().UTC().Truncate(time.Second))
	stale := *a
	ackAt := a.FiredAt.Add(time.Minute)
	a.Status, a.AckedAt = domain.AlertStatusAcked, &ackAt
	// Caller-supplied versions and identities cannot rewrite the stored mapping.
	a.SourceAlertID, a.TransitionVersion = probeRegistryID2, 999
	clear := injectOutboxFailure(t, f, "alerts", "UPDATE")
	err := r.Update(ctx, a)
	clear()
	if err == nil {
		t.Fatal("expected failed lifecycle write")
	}
	if got := alertBySource(t, r, stale.SourceAlertID); got.Status != domain.AlertStatusFiring || got.TransitionVersion != 1 {
		t.Fatal("failed transition advanced lifecycle or source version")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { copy := *a; errs <- r.Update(ctx, &copy) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := alertBySource(t, r, stale.SourceAlertID)
	if got.Status != domain.AlertStatusAcked || got.TransitionVersion != 2 || got.AckToken != stale.AckToken {
		t.Fatal("duplicate acknowledgements changed source identity/version")
	}
	// A stale recovery still preserves a concurrent acknowledgement, then advances once.
	resolvedAt := ackAt.Add(time.Minute)
	stale.Status, stale.ResolvedAt, stale.OpenMonitorID = domain.AlertStatusResolved, &resolvedAt, nil
	if err := r.Update(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	if stale.TransitionVersion != 3 || stale.AckedAt == nil || !stale.AckedAt.Equal(ackAt) {
		t.Fatal("recovery lost ack/version")
	}
	if err := r.Update(ctx, &stale); err != nil || stale.TransitionVersion != 3 {
		t.Fatal("duplicate recovery advanced version")
	}
	if err := r.Update(ctx, a); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("delayed ack reopened source")
	}
	if got := alertBySource(t, r, stale.SourceAlertID); got.TransitionVersion != 3 {
		t.Fatal("rejected ack advanced version")
	}
	// Exhaustion cannot overflow or report an unpersisted transition as successful.
	next := openScopedAlert(t, r, stale.MonitorID, resolvedAt.Add(time.Minute))
	if _, err := f.db.ExecContext(ctx, "UPDATE alerts SET transition_version = ? WHERE id = ?", int64(math.MaxInt64), next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Acknowledge(ctx, next.ID, nil); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("exhausted source version accepted ack")
	}
	if got := alertBySource(t, r, next.SourceAlertID); got.TransitionVersion != math.MaxInt64 || got.Status != domain.AlertStatusFiring {
		t.Fatal("version overflow mutated source")
	}
	for _, statement := range []string{
		"UPDATE alerts SET transition_version = 0", "UPDATE alerts SET source_alert_id = ''",
		"UPDATE alerts SET source_alert_id = '00000000-0000-0000-0000-000000000000'",
		"UPDATE alerts SET source_alert_id = 'GGGGGGGG-GGGG-GGGG-GGGG-GGGGGGGGGGGG'",
	} {
		if _, err := f.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("accepted invalid source: %s", statement)
		}
	}
}

func testAlertSourceMigration(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	r := alertScopeRepos(f)
	at := time.Now().UTC().Truncate(time.Second)
	a := openScopedAlert(t, r.alerts, localMonitor(t, f), at)
	p := alertScopePolicy(t, f, r, a.MonitorID)
	e := startScopedLadder(t, r, a, p.ID)
	user := f.user(t)
	a, err := services.NewAlertService(r.alerts).Acknowledge(ctx, a.ID, &user)
	if err != nil {
		t.Fatal(err)
	}
	leaseUntil := at.Add(time.Hour)
	if _, err := f.db.ExecContext(ctx, "UPDATE alert_escalations SET lease_owner = ?, lease_until = ? WHERE id = ?", "source-migration", leaseUntil, e.ID); err != nil {
		t.Fatal(err)
	}
	e, err = r.state.GetByAlertID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	remoteRepo := scopedAlerts(t, r.alerts, probeRegistryID1, 2)
	b := openScopedAlert(t, remoteRepo, a.MonitorID, at)
	if err := services.NewAlertService(remoteRepo).ResolveOpen(ctx, a.MonitorID, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	b, err = remoteRepo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	extraMid := localMonitor(t, f)
	extra := openScopedAlert(t, r.alerts, extraMid, at)
	extraE := startScopedLadder(t, r, extra, p.ID)
	if _, err := f.db.ExecContext(ctx, "DELETE FROM alerts WHERE id = ?", extra.ID); err != nil {
		t.Fatal(err)
	}
	// An old update timestamp exposes database-managed timestamp changes during backfill.
	oldUpdatedAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := f.db.ExecContext(ctx, "UPDATE alerts SET updated_at = ?", oldUpdatedAt); err != nil {
		t.Fatal(err)
	}
	a.UpdatedAt, b.UpdatedAt = oldUpdatedAt, oldUpdatedAt
	// Rehearse a populated 045 -> 046 migration twice. Unpublished source IDs may reset.
	for range 2 {
		if err := runAlertSourceMigration(t, f, "down"); err != nil {
			t.Fatal(err)
		}
		if err := runAlertSourceMigration(t, f, "up"); err != nil {
			t.Fatal(err)
		}
		for _, pair := range []struct {
			repo   ports.AlertRepository
			before *domain.Alert
		}{{r.alerts, a}, {remoteRepo, b}} {
			got, err := pair.repo.GetByID(ctx, pair.before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := uuid.Parse(got.SourceAlertID); err != nil || got.TransitionVersion != 1 {
				t.Fatal("invalid backfilled source")
			}
			before := *pair.before
			before.SourceAlertID, before.TransitionVersion = got.SourceAlertID, 1
			if !reflect.DeepEqual(got, &before) {
				typ := reflect.TypeOf(before)
				oldValue, newValue := reflect.ValueOf(before), reflect.ValueOf(*got)
				var changed []string
				for i := range typ.NumField() {
					if !reflect.DeepEqual(oldValue.Field(i).Interface(), newValue.Field(i).Interface()) {
						changed = append(changed, typ.Field(i).Name)
					}
				}
				t.Fatalf("backfill changed existing alert fields: %v", changed)
			}
		}
		child, err := r.state.GetByAlertID(ctx, a.ID)
		if err != nil || !reflect.DeepEqual(child, e) {
			t.Fatal("backfill changed escalation progress/lease")
		}
	}
	if err := repository.RunMigrations(f.db.DB, f.engine); err != nil {
		t.Fatal(err)
	}
	a, err = r.alerts.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Identity now crosses the source/mirror boundary. Downgrade must fail intact.
	incident := availabilityIncident(a.SourceAlertID, a.MonitorID, a.ProbeID, a.FiredAt)
	if err := f.incidents.PutIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	if err := runAlertSourceMigration(t, f, "down"); err == nil {
		t.Fatal("downgrade discarded published source mapping")
	}
	if got := alertBySource(t, r.alerts, a.SourceAlertID); !reflect.DeepEqual(got, a) {
		t.Fatal("failed downgrade changed source")
	}
	if acked, err := services.NewAlertService(r.alerts).AcknowledgeByToken(ctx, a.AckToken); err != nil || acked.SourceAlertID != a.SourceAlertID {
		t.Fatal("migration broke retained acknowledgement link")
	}
	later := openScopedAlert(t, r.alerts, extraMid, at)
	laterE := startScopedLadder(t, r, later, p.ID)
	if later.ID <= extra.ID || laterE.ID <= extraE.ID {
		t.Fatal("migration reused deleted IDs")
	}
}
