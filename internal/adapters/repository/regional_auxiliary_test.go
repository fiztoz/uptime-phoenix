package repository_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func auxiliaryRepos(f probeRegistryFixture) (ports.MonitorConditionRepository, ports.TLSInfoRepository) {
	if f.engine == "sqlite" {
		return sqlite.NewMonitorConditionRepo(f.db), sqlite.NewTLSInfoRepo(f.db)
	}
	return mariadb.NewMonitorConditionRepo(f.db), mariadb.NewTLSInfoRepo(f.db)
}

func boundAuxiliary(t *testing.T, f probeRegistryFixture, probeID string, generation int64) (ports.MonitorConditionRepository, ports.TLSInfoRepository) {
	t.Helper()
	conditions, certs := auxiliaryRepos(f)
	conditions, err := conditions.(ports.RegionalConditionRepository).ForAssignment(probeID, generation)
	if err != nil {
		t.Fatal(err)
	}
	certs, err = certs.(ports.RegionalTLSInfoRepository).ForAssignment(probeID, generation)
	if err != nil {
		t.Fatal(err)
	}
	return conditions, certs
}

type auxiliaryNotifier struct {
	alerts []domain.AlertContext
	fail   bool
}

func (n *auxiliaryNotifier) Dispatch(_ context.Context, _ *domain.Monitor, alert domain.AlertContext) error {
	if n.fail {
		return errors.New("provider unavailable")
	}
	n.alerts = append(n.alerts, alert)
	return nil
}

func (n *auxiliaryNotifier) DispatchTracked(ctx context.Context, monitor *domain.Monitor, alert domain.AlertContext) (bool, error) {
	err := n.Dispatch(ctx, monitor, alert)
	return err == nil, err
}

type auxiliaryMaintenance struct{ active bool }

func (m *auxiliaryMaintenance) IsActive(context.Context, int64) (bool, error) { return m.active, nil }

type auxiliaryBus struct {
	silentBus
	events []ports.Event
}

func (b *auxiliaryBus) Publish(_ context.Context, event ports.Event) error {
	b.events = append(b.events, event)
	return nil
}

func capacitySample(state domain.ConditionState, percent float64) domain.ConditionObservation {
	threshold := 80.0
	return domain.ConditionObservation{
		Kind: domain.MonitorConditionStorage, State: state, Percent: &percent, Threshold: &threshold,
		ObservedAt: time.Now().In(time.FixedZone("UTC+7", 7*3600)),
	}
}

func TestRegionalAuxiliaryContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("IndependentPromotionAndRecovery", func(t *testing.T) { testRegionalCapacity(t, newProbeRegistryFixture(t, engine)) })
			t.Run("IndependentCertificates", func(t *testing.T) { testRegionalCertificates(t, newProbeRegistryFixture(t, engine)) })
			t.Run("CurrentLocalAndCleanup", func(t *testing.T) { testAuxiliaryViews(t, newProbeRegistryFixture(t, engine)) })
			t.Run("MigrationAndGuards", func(t *testing.T) { testAuxiliaryMigration(t, newProbeRegistryFixture(t, engine)) })
			t.Run("LocalRecorder", func(t *testing.T) { testAuxiliaryHeartbeat(t, newProbeRegistryFixture(t, engine)) })
		})
	}
}

func testRegionalCapacity(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitor := &domain.Monitor{ID: f.monitor(t), Interval: 60}
	repo, _ := auxiliaryRepos(f)
	notifier, maintenance, bus := &auxiliaryNotifier{}, &auxiliaryMaintenance{}, &auxiliaryBus{}
	svc := services.NewMonitorConditionService(repo, notifier, maintenance, bus)
	record := func(probe string, generation int64, state domain.ConditionState, percent float64) {
		t.Helper()
		if err := svc.OnAssignmentCheck(ctx, monitor, probe, generation, []domain.ConditionObservation{capacitySample(state, percent)}); err != nil {
			t.Fatal(err)
		}
	}
	record(probeRegistryID1, 1, domain.ConditionStateWarning, 90)
	record("local", 1, domain.ConditionStateOK, 20)
	record(probeRegistryID2, 1, domain.ConditionStateError, 0)
	if len(notifier.alerts) != 0 {
		t.Fatal("one region borrowed another's promotion count")
	}
	record(probeRegistryID1, 1, domain.ConditionStateWarning, 90)
	record(probeRegistryID2, 1, domain.ConditionStateError, 0)
	if len(notifier.alerts) != 2 || notifier.alerts[0].ProbeID != probeRegistryID1 || notifier.alerts[1].ProbeID != probeRegistryID2 {
		t.Fatalf("independent notifications: %+v", notifier.alerts)
	}
	// Recreate the service to prove promotion and delivery cursors survive restart.
	svc = services.NewMonitorConditionService(repo, notifier, maintenance, bus)
	record(probeRegistryID1, 1, domain.ConditionStateWarning, 90)
	record("local", 1, domain.ConditionStateOK, 20)
	record(probeRegistryID1, 1, domain.ConditionStateOK, 77) // Hysteresis retains warning.
	if len(notifier.alerts) != 2 {
		t.Fatal("restart or another region reset the cursor")
	}
	record(probeRegistryID1, 1, domain.ConditionStateOK, 70)
	record(probeRegistryID1, 1, domain.ConditionStateOK, 70)
	if len(notifier.alerts) != 3 || notifier.alerts[2].ConditionState != domain.ConditionStateOK {
		t.Fatal("missing regional recovery")
	}
	remote, _ := boundAuxiliary(t, f, probeRegistryID2, 1)
	row, err := remote.Get(ctx, monitor.ID, domain.MonitorConditionStorage)
	if err != nil || row.State != domain.ConditionStateError || row.LastNotifiedState != domain.ConditionStateError {
		t.Fatalf("recovery crossed regions: %+v %v", row, err)
	}
	maintenance.active = true
	record(probeRegistryID1, 2, domain.ConditionStateWarning, 90)
	record(probeRegistryID1, 2, domain.ConditionStateWarning, 90)
	if len(notifier.alerts) != 3 {
		t.Fatal("maintenance delivered an alert")
	}
	maintenance.active = false
	record(probeRegistryID1, 2, domain.ConditionStateWarning, 90)
	if len(notifier.alerts) != 4 || notifier.alerts[3].AssignmentGeneration != 2 {
		t.Fatal("new generation inherited old cursor")
	}
	for _, event := range bus.events {
		if event.Payload.(*domain.MonitorCondition).ProbeID != domain.LocalProbeID {
			t.Fatal("remote condition reached legacy browser event")
		}
	}
	for _, alert := range notifier.alerts {
		if alert.Status != domain.StatusUp || alert.DeliveryScope != domain.IncidentScopeRegional {
			t.Fatalf("capacity changed availability/scope: %+v", alert)
		}
	}
}

func testRegionalCertificates(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitor := &domain.Monitor{ID: f.monitor(t), CertExpiryNotify: true}
	_, repo := auxiliaryRepos(f)
	notifier, maintenance := &auxiliaryNotifier{}, &auxiliaryMaintenance{}
	svc := services.NewCertificateAlertService(repo, notifier, maintenance)
	expiry := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	record := func(probe string, generation int64, days int, notAfter time.Time) error {
		return svc.OnAssignmentCheck(ctx, monitor, probe, generation, map[string]string{
			"tls_days_remaining": fmt.Sprint(days), "tls_not_after": notAfter.Format(time.RFC3339), "tls_issuer": probe,
		})
	}
	for _, probe := range []string{"local", probeRegistryID1, probeRegistryID2} {
		if err := record(probe, 1, 20, expiry); err != nil {
			t.Fatal(err)
		}
	}
	if len(notifier.alerts) != 3 {
		t.Fatal("regions shared a certificate cursor")
	}
	svc = services.NewCertificateAlertService(repo, notifier, maintenance)
	if err := record(probeRegistryID1, 1, 20, expiry); err != nil {
		t.Fatal(err)
	}
	if len(notifier.alerts) != 3 {
		t.Fatal("restart lost certificate cursor")
	}
	if err := record(probeRegistryID1, 1, 5, expiry); err != nil {
		t.Fatal(err)
	}
	if err := record(probeRegistryID1, 1, 20, expiry.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := record(probeRegistryID1, 2, 20, expiry); err != nil {
		t.Fatal(err)
	}
	if len(notifier.alerts) != 6 {
		t.Fatalf("threshold/renewal/generation notifications = %d", len(notifier.alerts))
	}
	_, other := boundAuxiliary(t, f, probeRegistryID2, 1)
	row, err := other.GetByMonitorID(ctx, monitor.ID)
	if err != nil || row.LastCertAlertThreshold != 30 || !row.LastCertAlertNotAfter.Equal(expiry) {
		t.Fatalf("renewal crossed region: %+v %v", row, err)
	}
	maintenance.active = true
	if err := record(probeRegistryID2, 1, 5, expiry); err != nil {
		t.Fatal(err)
	}
	maintenance.active = false
	notifier.fail = true
	if err := record(probeRegistryID2, 1, 5, expiry); err == nil {
		t.Fatal("provider failure returned success")
	}
	row, err = other.GetByMonitorID(ctx, monitor.ID)
	if err != nil || row.LastCertAlertThreshold != 30 {
		t.Fatal("failed/suppressed delivery advanced threshold")
	}
	notifier.fail = false
	if err := record(probeRegistryID2, 1, 5, expiry); err != nil {
		t.Fatal(err)
	}
	if len(notifier.alerts) != 7 {
		t.Fatal("unsent threshold did not retry")
	}
	for _, alert := range notifier.alerts {
		if alert.ProbeID == "" || alert.AssignmentGeneration < 1 || alert.AlertScope != domain.AlertScopeMonitor {
			t.Fatalf("missing ownership: %+v", alert)
		}
	}
}

func testAuxiliaryViews(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitorID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
		t.Fatal(err)
	}
	f.remote(t, probeRegistryID1, "remote")
	baseConditions, baseCerts := auxiliaryRepos(f)
	for _, identity := range []struct {
		probe      string
		generation int64
	}{{"local", 1}, {probeRegistryID1, 1}, {"local", 2}} {
		conditions, certs := boundAuxiliary(t, f, identity.probe, identity.generation)
		observation := capacitySample(domain.ConditionStateWarning, 90)
		row := &domain.MonitorCondition{MonitorID: monitorID, ConditionObservation: observation, LastSuccessAt: &observation.ObservedAt, LastNotifiedAt: &observation.ObservedAt}
		if err := conditions.Upsert(ctx, row); err != nil {
			t.Fatal(err)
		}
		if err := certs.Upsert(ctx, &ports.TLSInfo{MonitorID: monitorID, CheckedAt: observation.ObservedAt, NotAfter: observation.ObservedAt.Add(time.Hour), Issuer: identity.probe}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(generation int64) {
		t.Helper()
		rows, err := baseConditions.ListByMonitorIDs(ctx, []int64{monitorID})
		if err != nil || len(rows) != 1 || rows[0].ProbeID != "local" || rows[0].AssignmentGeneration != generation {
			t.Fatalf("legacy list scope: %+v %v", rows, err)
		}
		if rows[0].ObservedAt.Location() != time.UTC || rows[0].LastSuccessAt.Location() != time.UTC || rows[0].LastNotifiedAt.Location() != time.UTC {
			t.Fatal("condition boundary is not UTC")
		}
		cert, err := baseCerts.GetByMonitorID(ctx, monitorID)
		if err != nil || cert.ProbeID != "local" || cert.AssignmentGeneration != generation || cert.CheckedAt.Location() != time.UTC {
			t.Fatalf("legacy TLS scope: %+v %v", cert, err)
		}
	}
	check(1)
	if _, err := f.assignments.Replace(ctx, monitorID, 1, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := baseCerts.GetByMonitorID(ctx, monitorID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("removed local TLS remained visible: %v", err)
	}
	if rows, err := baseConditions.ListAll(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("remote leaked into legacy list: %+v %v", rows, err)
	}
	if err := baseCerts.Upsert(ctx, &ports.TLSInfo{MonitorID: monitorID}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("legacy writer resurrected removed local: %v", err)
	}
	if _, err := f.assignments.Replace(ctx, monitorID, 2, []string{"local", probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	check(2)
	if rows, err := baseConditions.ListByMonitorIDs(ctx, nil); err != nil || len(rows) != 0 {
		t.Fatal("empty scope leaked conditions")
	}
	remote, _ := boundAuxiliary(t, f, probeRegistryID1, 1)
	if err := remote.DeleteKind(ctx, monitorID, domain.MonitorConditionStorage); err != nil {
		t.Fatal(err)
	}
	check(2)
	if err := baseConditions.DeleteKind(ctx, monitorID, domain.MonitorConditionStorage); err != nil {
		t.Fatal(err)
	}
	old, _ := boundAuxiliary(t, f, "local", 1)
	if _, err := old.Get(ctx, monitorID, domain.MonitorConditionStorage); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("administrative cleanup retained old generation")
	}
	for _, identity := range []struct {
		probe      string
		generation int64
	}{{"", 1}, {"local", 0}, {"invalid", 1}} {
		if _, err := baseCerts.(ports.RegionalTLSInfoRepository).ForAssignment(identity.probe, identity.generation); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid TLS identity accepted")
		}
		if _, err := baseConditions.(ports.RegionalConditionRepository).ForAssignment(identity.probe, identity.generation); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid capacity identity accepted")
		}
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM monitors WHERE id = ?", monitorID); err != nil {
		t.Fatal(err)
	}
	_, remoteTLS := boundAuxiliary(t, f, probeRegistryID1, 1)
	if _, err := remoteTLS.GetByMonitorID(ctx, monitorID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("regional TLS survived monitor delete")
	}
}

func runAuxiliaryMigration(t *testing.T, f probeRegistryFixture, direction string) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.engine, "migrations", "041_probe_auxiliary_state."+direction+".sql"))
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := f.db.ExecContext(context.Background(), statement); err != nil {
			return err
		}
	}
	return nil
}

func testAuxiliaryMigration(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitorID := f.monitor(t)
	if err := runAuxiliaryMigration(t, f, "down"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	json := `{"days_remaining":5,"not_after":"2030-01-01T00:00:00Z","last_cert_alert_threshold":7,"last_cert_alert_not_after":"2030-01-01T00:00:00Z"}`
	if _, err := f.db.ExecContext(ctx, "INSERT INTO tls_info (id, monitor_id, info_json, checked_at) VALUES (100, ?, ?, ?)", monitorID, json, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, "INSERT INTO monitor_conditions (monitor_id, kind, state, message, observed_at, stale_after, consecutive_state, consecutive_count, last_notified_state, last_notified_at) VALUES (?, 'storage', 'warning', '', ?, ?, 'warning', 2, 'warning', ?)", monitorID, now, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	for range 2 { // A complete up/down cycle preserves legacy identities and cursors.
		if err := runAuxiliaryMigration(t, f, "up"); err != nil {
			t.Fatal(err)
		}
		conditions, certs := auxiliaryRepos(f)
		condition, err := conditions.Get(ctx, monitorID, "storage")
		if err != nil || condition.ConsecutiveCount != 2 || condition.LastNotifiedState != domain.ConditionStateWarning || condition.AssignmentGeneration != 1 {
			t.Fatalf("legacy condition backfill: %+v %v", condition, err)
		}
		cert, err := certs.GetByMonitorID(ctx, monitorID)
		if err != nil || cert.LastCertAlertThreshold != 7 || cert.AssignmentGeneration != 1 {
			t.Fatalf("legacy TLS backfill: %+v %v", cert, err)
		}
		var id int64
		if err := f.db.NewSelect().TableExpr("tls_info").Column("id").Where("monitor_id = ?", monitorID).Scan(ctx, &id); err != nil || id != 100 {
			t.Fatal("migration changed TLS ID")
		}
		if err := runAuxiliaryMigration(t, f, "down"); err != nil {
			t.Fatal(err)
		}
	}
	if err := runAuxiliaryMigration(t, f, "up"); err != nil {
		t.Fatal(err)
	}
	remote, certs := boundAuxiliary(t, f, probeRegistryID1, 2)
	if err := certs.Upsert(ctx, &ports.TLSInfo{MonitorID: monitorID, NotAfter: now, CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := remote.Upsert(ctx, &domain.MonitorCondition{MonitorID: monitorID, ConditionObservation: capacitySample(domain.ConditionStateError, 0)}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE tls_info SET assignment_generation = 0", "UPDATE monitor_conditions SET assignment_generation = 0",
		"UPDATE tls_info SET probe_id = ''", "UPDATE monitor_conditions SET probe_id = ''",
	} {
		if _, err := f.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %s", statement)
		}
	}
	if err := runAuxiliaryMigration(t, f, "down"); err == nil {
		t.Fatal("downgrade discarded regional data")
	}
	if _, err := certs.GetByMonitorID(ctx, monitorID); err != nil {
		t.Fatal("downgrade guard lost TLS")
	}
	if _, err := remote.Get(ctx, monitorID, "storage"); err != nil {
		t.Fatal("downgrade guard lost capacity")
	}
}

func testAuxiliaryHeartbeat(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitor := &domain.Monitor{UserID: f.user(t), Name: "regional auxiliary", Type: "http", Active: true, Interval: 60, MaxRetries: 1, CertExpiryNotify: true, Config: map[string]any{}}
	if err := newEngineMonitorRepo(f).Create(ctx, monitor); err != nil {
		t.Fatal(err)
	}
	conditions, certs := auxiliaryRepos(f)
	notifier := &auxiliaryNotifier{}
	svc := services.NewHeartbeatService(newEngineHeartbeatRepo(f), silentBus{})
	svc.SetRegionalRecorder(f.assignments, f.localHeartbeat)
	svc.SetTLSInfoRepo(certs)
	svc.SetCertAlert(services.NewCertificateAlertService(certs, notifier, nil))
	svc.SetConditionEvaluator(services.NewMonitorConditionService(conditions, notifier, nil, silentBus{}))
	result := ports.CheckResult{Status: domain.StatusDown, Metadata: map[string]string{"tls_days_remaining": "5", "tls_not_after": time.Now().Add(5 * 24 * time.Hour).UTC().Format(time.RFC3339)}, Conditions: []domain.ConditionObservation{capacitySample(domain.ConditionStateWarning, 90)}}
	for range 3 {
		if err := svc.Record(ctx, monitor, result); err != nil {
			t.Fatal(err)
		}
	}
	if len(notifier.alerts) != 2 {
		t.Fatalf("local parity alert count = %d", len(notifier.alerts))
	}
	f.remote(t, probeRegistryID1, "remote")
	if _, err := f.assignments.Replace(ctx, monitor.ID, 1, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.Replace(ctx, monitor.ID, 2, []string{"local"}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if err := svc.Record(ctx, monitor, result); err != nil {
		t.Fatal(err)
	}
	state, err := f.commits.GetState(ctx, monitor.ID, "local")
	if err != nil || state.AssignmentGeneration != 2 || state.DownCount != 1 || state.Status != domain.StatusPending || state.Seq != 4 {
		t.Fatalf("reassignment reused retry state/sequence: %+v %v", state, err)
	}
	condition, err := conditions.Get(ctx, monitor.ID, "storage")
	if err != nil || condition.AssignmentGeneration != 2 || condition.ConsecutiveCount != 1 || condition.LastNotifiedState != "" {
		t.Fatalf("reassignment reused condition: %+v %v", condition, err)
	}
	if len(notifier.alerts) != 3 || notifier.alerts[2].EventKind != domain.AlertEventCertificateExpiry || notifier.alerts[2].AssignmentGeneration != 2 {
		t.Fatal("new assignment did not reset certificate cursor")
	}
	if err := svc.Record(ctx, monitor, result); err != nil {
		t.Fatal(err)
	}
	if len(notifier.alerts) != 4 {
		t.Fatal("new assignment did not promote its own condition")
	}
}
