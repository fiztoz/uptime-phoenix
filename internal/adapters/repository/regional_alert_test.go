package repository_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type regionalAlertRepos struct {
	alerts      ports.AlertRepository
	state       ports.AlertEscalationRepository
	policies    ports.EscalationPolicyRepository
	assignments ports.EscalationAssignmentRepository
}

func alertScopeRepos(f probeRegistryFixture) regionalAlertRepos {
	if f.engine == "sqlite" {
		return regionalAlertRepos{sqlite.NewAlertRepo(f.db), sqlite.NewAlertEscalationRepo(f.db), sqlite.NewEscalationPolicyRepo(f.db), sqlite.NewEscalationAssignmentRepo(f.db)}
	}
	return regionalAlertRepos{mariadb.NewAlertRepo(f.db), mariadb.NewAlertEscalationRepo(f.db), mariadb.NewEscalationPolicyRepo(f.db), mariadb.NewEscalationAssignmentRepo(f.db)}
}

func scopedAlerts(t *testing.T, r ports.AlertRepository, probe string, generation int64) ports.AlertRepository {
	t.Helper()
	bound, err := r.(ports.RegionalAlertRepository).ForAssignment(probe, generation)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func openScopedAlert(t *testing.T, repo ports.AlertRepository, mid int64, at time.Time) *domain.Alert {
	t.Helper()
	a, err := services.NewAlertService(repo).OpenOnDown(context.Background(), &domain.Monitor{ID: mid, Name: "shared target"}, at)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type regionalAlertNotifier struct {
	alerts        []domain.AlertContext
	notifications int
}

func (n *regionalAlertNotifier) Notify(context.Context, *domain.Monitor, domain.Status, domain.Status) error {
	n.notifications++
	return nil
}
func (n *regionalAlertNotifier) DispatchToNotificationIDs(_ context.Context, _ []int64, alert domain.AlertContext) error {
	n.alerts = append(n.alerts, alert)
	return nil
}

func alertScopePolicy(t *testing.T, f probeRegistryFixture, r regionalAlertRepos, mid int64) *domain.EscalationPolicy {
	t.Helper()
	ctx := context.Background()
	p := &domain.EscalationPolicy{UserID: f.user(t), Name: "on call", Enabled: true,
		Steps: []domain.EscalationStep{{StepOrder: 1, WaitMinutes: 0}, {StepOrder: 2, WaitMinutes: 5}}}
	if err := r.policies.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := r.assignments.AssignMonitor(ctx, mid, p.ID); err != nil {
		t.Fatal(err)
	}
	return p
}

func startScopedLadder(t *testing.T, r regionalAlertRepos, a *domain.Alert, policyID int64) *domain.AlertEscalation {
	t.Helper()
	e := &domain.AlertEscalation{AlertID: a.ID, MonitorID: a.MonitorID, PolicyID: policyID,
		NextStep: 1, NextRunAt: a.FiredAt, Status: domain.EscalationStatePending}
	if err := r.state.Create(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRegionalAlertContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("IsolationAndGeneration", func(t *testing.T) { testAlertScopeIsolation(t, newProbeRegistryFixture(t, engine)) })
			t.Run("ConcurrentOpenAndTransitions", func(t *testing.T) { testAlertScopeConcurrency(t, newProbeRegistryFixture(t, engine)) })
			t.Run("DispatcherAndRunner", func(t *testing.T) { testAlertScopeRuntime(t, newProbeRegistryFixture(t, engine)) })
			t.Run("PopulatedMigration", func(t *testing.T) { testAlertScopeMigration(t, newProbeRegistryFixture(t, engine)) })
		})
	}
}

func testAlertScopeIsolation(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	mid := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, mid); err != nil {
		t.Fatal(err)
	}
	f.remote(t, probeRegistryID1, "east")
	if _, err := f.assignments.Replace(ctx, mid, 1, []string{"local", probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	r := alertScopeRepos(f)
	local, remote := scopedAlerts(t, r.alerts, "local", 1), scopedAlerts(t, r.alerts, probeRegistryID1, 1)
	at := time.Now().UTC().Truncate(time.Second)
	a, b := openScopedAlert(t, local, mid, at), openScopedAlert(t, remote, mid, at)
	if a.ID == b.ID || a.ProbeID != "local" || b.ProbeID != probeRegistryID1 {
		t.Fatal("shared incident identity")
	}
	if _, err := r.alerts.GetByID(ctx, b.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("remote leaked by ID: %v", err)
	}
	for _, repo := range []ports.AlertRepository{r.alerts, local, remote} {
		if _, err := repo.GetByAckToken(ctx, b.AckToken); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("remote deep link accepted: %v", err)
		}
	}
	if _, err := remote.GetByID(ctx, a.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("cross-probe read")
	}
	if err := remote.Update(ctx, a); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("cross-probe update")
	}
	rows, err := r.alerts.List(ctx, ports.AlertFilter{})
	if err != nil || len(rows) != 1 || rows[0].ID != a.ID {
		t.Fatalf("legacy list: %+v %v", rows, err)
	}
	rows, err = remote.List(ctx, ports.AlertFilter{RestrictToMonitorIDs: true})
	if err != nil || len(rows) != 0 {
		t.Fatal("empty grants leaked alerts")
	}

	policy := alertScopePolicy(t, f, r, mid)
	startScopedLadder(t, r, a, policy.ID)
	startScopedLadder(t, r, b, policy.ID)
	esc := services.NewEscalationService(r.policies, r.assignments, r.state, r.alerts, newEngineMonitorRepo(f), nil, &regionalAlertNotifier{})
	localSvc := services.NewAlertService(local)
	localSvc.SetEscalationCanceller(esc)
	if _, err := localSvc.Acknowledge(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	ea, _ := r.state.GetByAlertID(ctx, a.ID)
	eb, _ := r.state.GetByAlertID(ctx, b.ID)
	if ea.Status != domain.EscalationStateCanceled || eb.Status != domain.EscalationStatePending {
		t.Fatal("ack crossed ladders")
	}
	if err := localSvc.ResolveOpen(ctx, mid, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := remote.GetOpenByMonitorID(ctx, mid); err != nil || got.ID != b.ID || got.Status != domain.AlertStatusFiring {
		t.Fatal("local recovery closed remote outage")
	}

	// A remote acknowledgement cannot stop a local incident either.
	a = openScopedAlert(t, local, mid, at.Add(2*time.Minute))
	remoteSvc := services.NewAlertService(remote)
	remoteSvc.SetEscalationCanceller(esc)
	if _, err := remoteSvc.Acknowledge(ctx, b.ID, nil); err != nil {
		t.Fatal(err)
	}
	if acked, err := localSvc.IsOpenAcked(ctx, mid); err != nil || acked {
		t.Fatal("remote acknowledgement suppressed local")
	}
	if _, err := f.assignments.Replace(ctx, mid, 2, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := r.alerts.GetOpenByMonitorID(ctx, mid); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("removed local still selected")
	}
	if _, err := services.NewAlertService(r.alerts).OpenOnDown(ctx, &domain.Monitor{ID: mid}, at); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("unbound write resurrected local: %v", err)
	}
	if _, err := f.assignments.Replace(ctx, mid, 3, []string{"local", probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	next := openScopedAlert(t, r.alerts, mid, at.Add(3*time.Minute))
	if next.AssignmentGeneration != 2 || next.ID == a.ID {
		t.Fatal("new generation reused incident")
	}
	newLocal := scopedAlerts(t, r.alerts, "local", 2)
	if _, err := newLocal.GetByID(ctx, a.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("old generation leaked through bound read")
	}
	if got, err := newLocal.GetOpenByMonitorID(ctx, mid); err != nil || got.ID != next.ID {
		t.Fatal("bound generation selected wrong incident")
	}
	// A retained old local token acknowledges only its own incident.
	if _, err := services.NewAlertService(r.alerts).AcknowledgeByToken(ctx, a.AckToken); err != nil {
		t.Fatal(err)
	}
	if got, err := r.alerts.GetOpenByMonitorID(ctx, mid); err != nil || got.ID != next.ID || got.IsAcked() {
		t.Fatal("old token acknowledged new generation")
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM monitors WHERE id = ?", mid); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"alerts", "alert_escalations"} {
		n, err := f.db.NewSelect().TableExpr(table).Count(ctx)
		if err != nil || n != 0 {
			t.Fatalf("cascade %s: %d %v", table, n, err)
		}
	}
}

func testAlertScopeConcurrency(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	r := alertScopeRepos(f)
	mid := f.monitor(t)
	svc := services.NewAlertService(scopedAlerts(t, r.alerts, "local", 1))
	at := time.Now().UTC().Truncate(time.Second)
	const count = 8
	results := make(chan *domain.Alert, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			a, err := svc.OpenOnDown(ctx, &domain.Monitor{ID: mid}, at)
			if err != nil {
				errs <- err
				return
			}
			results <- a
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var id int64
	for a := range results {
		if id != 0 && id != a.ID {
			t.Fatal("multiple open alerts for one assignment")
		}
		id = a.ID
	}
	stale, err := r.alerts.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	user := f.user(t)
	if _, err := svc.Acknowledge(ctx, id, &user); err != nil {
		t.Fatal(err)
	}
	resolved := *stale
	resolved.Status = domain.AlertStatusResolved
	resolved.ResolvedAt = &at
	resolved.OpenMonitorID = nil
	if err := r.alerts.Update(ctx, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.AckedByUserID == nil || *resolved.AckedByUserID != user {
		t.Fatal("stale resolution erased acknowledgement")
	}
	stale.Status, stale.AckedAt = domain.AlertStatusAcked, &at
	if err := r.alerts.Update(ctx, stale); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("late acknowledgement reopened incident: %v", err)
	}
	got, err := r.alerts.GetByID(ctx, id)
	if err != nil || got.Status != domain.AlertStatusResolved || got.OpenMonitorID != nil {
		t.Fatal("resolved incident changed")
	}
	if got.FiredAt.Location() != time.UTC || got.ResolvedAt.Location() != time.UTC {
		t.Fatal("non-UTC storage boundary")
	}
}

func testAlertScopeRuntime(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	mid := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, mid); err != nil {
		t.Fatal(err)
	}
	f.remote(t, probeRegistryID1, "east")
	r := alertScopeRepos(f)
	p := alertScopePolicy(t, f, r, mid)
	notifier := &regionalAlertNotifier{}
	esc := services.NewEscalationService(r.policies, r.assignments, r.state, r.alerts, newEngineMonitorRepo(f), nil, notifier)
	esc.SetAssignmentRepository(f.assignments)
	svc := services.NewAlertService(r.alerts)
	svc.SetEscalationCanceller(esc)
	dispatcher := services.NewNotificationDispatcher(notifier, &auxiliaryMaintenance{})
	dispatcher.SetAlertLifecycle(svc)
	dispatcher.SetAssignmentRepository(f.assignments)
	dispatcher.SetEscalationStarter(esc)
	dispatcher.SetThrottleRepository(repository.NewNotificationThrottleStore(f.db))
	monitor := &domain.Monitor{ID: mid, Name: "shared target", ResendInterval: 1}
	down := &domain.Heartbeat{MonitorID: mid, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusDown}
	upStatus, downStatus := domain.StatusUp, domain.StatusDown
	dispatcher.OnHeartbeat(ctx, monitor, down, &upStatus)
	old, err := r.alerts.GetOpenByMonitorID(ctx, mid)
	if err != nil || notifier.notifications != 1 {
		t.Fatalf("initial DOWN: %+v %v sends=%d", old, err, notifier.notifications)
	}
	remote := openScopedAlert(t, scopedAlerts(t, r.alerts, probeRegistryID1, 1), mid, time.Now().Add(-time.Hour))
	startScopedLadder(t, r, remote, p.ID)
	if err := esc.StartForAlert(ctx, remote, monitor); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("hub started remote ladder")
	}
	// Old generation's due ladder must be canceled after removal/re-add.
	if _, err := f.assignments.Replace(ctx, mid, 1, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.Replace(ctx, mid, 2, []string{"local", probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if n, err := esc.RunDue(ctx); err != nil || n != 0 {
		t.Fatalf("obsolete ladder delivered: %d %v", n, err)
	}
	prior, err := r.state.GetByAlertID(ctx, old.ID)
	if err != nil || prior.Status != domain.EscalationStateCanceled {
		t.Fatal("obsolete ladder not canceled")
	}
	untouched, err := r.state.GetByAlertID(ctx, remote.ID)
	if err != nil || untouched.Status != domain.EscalationStatePending || untouched.LeaseOwner != nil {
		t.Fatal("hub claimed remote ladder")
	}
	// Stale heartbeats cannot open, resolve, reserve attempts, or notify.
	staleUp := *down
	staleUp.Status = domain.StatusUp
	dispatcher.OnHeartbeat(ctx, monitor, &staleUp, &downStatus)
	dispatcher.OnHeartbeat(ctx, monitor, down, &upStatus)
	if notifier.notifications != 1 {
		t.Fatal("obsolete result delivered")
	}
	nextDown := *down
	nextDown.AssignmentGeneration = 2
	dispatcher.OnHeartbeat(ctx, monitor, &nextDown, &upStatus)
	next, err := r.alerts.GetOpenByMonitorID(ctx, mid)
	if err != nil || next.ID == old.ID || next.AssignmentGeneration != 2 || notifier.notifications != 2 {
		t.Fatal("new generation didn't open independently")
	}
	// Acknowledging the previous generation must not cancel the new ladder.
	if _, err := svc.AcknowledgeByToken(ctx, old.AckToken); err != nil {
		t.Fatal(err)
	}
	if n, err := esc.RunDue(ctx); err != nil || n != 1 {
		t.Fatalf("new ladder blocked by old ack: %d %v", n, err)
	}
	if len(notifier.alerts) != 1 || notifier.alerts[0].AssignmentGeneration != 2 || notifier.alerts[0].ProbeID != "local" {
		t.Fatal("escalation lost assignment context")
	}
	if _, err := svc.Acknowledge(ctx, next.ID, nil); err != nil {
		t.Fatal(err)
	}
	pending, err := r.state.GetByAlertID(ctx, next.ID)
	if err != nil || pending.Status != domain.EscalationStateCanceled {
		t.Fatal("ack left new ladder pending")
	}
	// Reconstruct dispatcher/service to establish restart parity.
	dispatcher = services.NewNotificationDispatcher(notifier, &auxiliaryMaintenance{})
	dispatcher.SetAssignmentRepository(f.assignments)
	dispatcher.SetAlertLifecycle(services.NewAlertService(r.alerts))
	dispatcher.SetThrottleRepository(repository.NewNotificationThrottleStore(f.db))
	dispatcher.OnHeartbeat(ctx, monitor, &nextDown, &downStatus)
	if notifier.notifications != 2 {
		t.Fatal("restart lost ack suppression")
	}
	recovery := nextDown
	recovery.Status = domain.StatusUp
	dispatcher.OnHeartbeat(ctx, monitor, &recovery, &downStatus)
	if notifier.notifications != 3 {
		t.Fatal("missing current generation recovery")
	}
	got, err := r.alerts.GetByID(ctx, next.ID)
	if err != nil || got.Status != domain.AlertStatusResolved {
		t.Fatal("recovery didn't resolve current generation")
	}
	got, err = r.alerts.GetByID(ctx, old.ID)
	if err != nil || got.Status != domain.AlertStatusAcked {
		t.Fatal("new recovery altered old generation")
	}
}

// Run SQLite rebuilds in one transaction with foreign keys enabled, just like startup.
func runAlertScopeMigration(t *testing.T, f probeRegistryFixture, direction string) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.engine, "migrations", "044_probe_alert_scope."+direction+".sql"))
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	apply := func(db bun.IDB) error {
		for _, stmt := range strings.Split(strings.Join(lines, "\n"), ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.ExecContext(context.Background(), stmt); err != nil {
				return err
			}
		}
		return nil
	}
	if f.engine == "sqlite" {
		return f.db.RunInTx(context.Background(), nil, func(_ context.Context, tx bun.Tx) error { return apply(tx) })
	}
	return apply(f.db)
}

func testAlertScopeMigration(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	mid := f.monitor(t)
	r := alertScopeRepos(f)
	p := alertScopePolicy(t, f, r, mid)
	a := openScopedAlert(t, r.alerts, mid, time.Now().UTC().Truncate(time.Second))
	e := startScopedLadder(t, r, a, p.ID)
	user := f.user(t)
	a, err := services.NewAlertService(r.alerts).Acknowledge(ctx, a.ID, &user)
	if err != nil {
		t.Fatal(err)
	}
	leaseUntil := a.FiredAt.Add(time.Hour)
	if _, err := f.db.ExecContext(ctx, "UPDATE alert_escalations SET lease_owner = ?, lease_until = ? WHERE id = ?", "migration-owner", leaseUntil, e.ID); err != nil {
		t.Fatal(err)
	}
	// Preserve deleted high IDs as well as rows and their foreign keys.
	extraMid := f.monitor(t)
	extra := openScopedAlert(t, r.alerts, extraMid, a.FiredAt)
	extraE := startScopedLadder(t, r, extra, p.ID)
	if _, err := f.db.ExecContext(ctx, "DELETE FROM alerts WHERE id = ?", extra.ID); err != nil {
		t.Fatal(err)
	}
	var token string
	if err := f.db.NewSelect().TableExpr("alerts").Column("ack_token").Where("id = ?", a.ID).Scan(ctx, &token); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := runAlertScopeMigration(t, f, "down"); err != nil {
			t.Fatal(err)
		}
		if err := runAlertScopeMigration(t, f, "up"); err != nil {
			t.Fatal(err)
		}
		got, err := r.alerts.GetByID(ctx, a.ID)
		if err != nil || got.AckToken != token || got.AssignmentGeneration != 1 || got.ProbeID != "local" || !got.FiredAt.Equal(a.FiredAt) || got.Status != domain.AlertStatusAcked || got.AckedAt == nil || got.AckedByUserID == nil || *got.AckedByUserID != user {
			t.Fatalf("backfill lost incident: %+v %v", got, err)
		}
		state, err := r.state.GetByAlertID(ctx, a.ID)
		if err != nil || state.ID != e.ID || state.Status != domain.EscalationStatePending || state.NextStep != e.NextStep || state.LeaseOwner == nil || *state.LeaseOwner != "migration-owner" || state.LeaseUntil == nil || !state.LeaseUntil.Equal(leaseUntil) {
			t.Fatalf("rebuild lost child: %+v %v", state, err)
		}
	}
	// Tracking makes ordinary startup idempotent.
	if err := repository.RunMigrations(f.db.DB, f.engine); err != nil {
		t.Fatal(err)
	}
	later := openScopedAlert(t, r.alerts, extraMid, a.FiredAt)
	laterE := startScopedLadder(t, r, later, p.ID)
	if later.ID <= extra.ID || laterE.ID <= extraE.ID {
		t.Fatal("migration reset AUTOINCREMENT high-water mark")
	}
	for _, statement := range []string{"UPDATE alerts SET assignment_generation = 0", "UPDATE alerts SET probe_id = ''"} {
		if _, err := f.db.ExecContext(ctx, statement); err == nil {
			t.Fatal("schema accepted invalid identity")
		}
	}
	for _, bad := range []struct {
		probe      string
		generation int64
	}{{"", 1}, {"bad", 1}, {"local", 0}} {
		if _, err := r.alerts.(ports.RegionalAlertRepository).ForAssignment(bad.probe, bad.generation); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid binding")
		}
	}
	remoteRepo := scopedAlerts(t, r.alerts, probeRegistryID1, 1)
	remote := openScopedAlert(t, remoteRepo, mid, a.FiredAt)
	if err := services.NewAlertService(remoteRepo).ResolveOpen(ctx, mid, a.FiredAt); err != nil {
		t.Fatal(err)
	}
	if err := runAlertScopeMigration(t, f, "down"); err == nil {
		t.Fatal("downgrade discarded regional identity")
	}
	if got, err := remoteRepo.GetByID(ctx, remote.ID); err != nil || got.Status != domain.AlertStatusResolved {
		t.Fatal("failed guard lost retained regional history")
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM alerts WHERE id = ?", remote.ID); err != nil {
		t.Fatal(err)
	}
	newLocal := scopedAlerts(t, r.alerts, "local", 2)
	laterGeneration := openScopedAlert(t, newLocal, mid, a.FiredAt)
	if err := runAlertScopeMigration(t, f, "down"); err == nil {
		t.Fatal("downgrade discarded later-generation identity")
	}
	if _, err := newLocal.GetByID(ctx, laterGeneration.ID); err != nil {
		t.Fatal("generation guard discarded incident")
	}
	invalid := &domain.AlertEscalation{AlertID: a.ID, MonitorID: extraMid, PolicyID: p.ID, NextStep: 1, NextRunAt: a.FiredAt, Status: domain.EscalationStatePending}
	if err := r.state.Create(ctx, invalid); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("ladder accepted mismatched parent monitor")
	}
}
