package edge

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// certStoreReopen reopens a durable edge store with the production telemetry
// encoder, so a restart exercises the same recovery path the edge binary uses.
// The caller must close the previous handle first: the data directory is
// single-writer by an OS lock, not by cooperation.
func certStoreReopen(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatalf("reopen edge store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func certAssignment() (*domain.EdgeResolvedConfig, domain.EdgeResolvedAssignment) {
	monitor := &domain.Monitor{ID: 17, Name: "portal", Type: "http", Active: true, CertExpiryNotify: true, Timeout: 3}
	links := []domain.MonitorNotification{{MonitorID: 17, NotificationID: 10}, {MonitorID: 17, NotificationID: 11}}
	a := domain.EdgeResolvedAssignment{Monitor: monitor, Generation: 1, NotificationLinks: links}
	c := &domain.EdgeResolvedConfig{
		Metadata:    domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: testHubID, ProbeID: testIdentity().ProbeID}, Revision: 1},
		Assignments: []domain.EdgeResolvedAssignment{a},
		Maintenance: map[int64]*domain.MaintenanceWindow{},
		Channels: map[int64]domain.EdgeResolvedChannel{
			10: {Notification: &domain.Notification{ID: 10, Type: "webhook", Active: true}, Version: 1},
			11: {Notification: &domain.Notification{ID: 11, Type: "slack", Active: false}, Version: 1},
		},
	}
	return c, a
}

// certResult builds a checker result carrying the three public certificate
// fields plus one that must never reach storage.
func certResult(days int, notAfter time.Time) ports.CheckResult {
	return ports.CheckResult{Status: domain.StatusUp, Metadata: map[string]string{
		"tls_not_after":       notAfter.UTC().Format(time.RFC3339Nano),
		"tls_days_remaining":  strconv.Itoa(days),
		"tls_issuer":          "Phoenix Test CA",
		"tls_leaf_subject_cn": "must-never-render",
	}}
}

func certCount(t *testing.T, s *Store, query string) int {
	t.Helper()
	var count int
	if err := s.db.NewRaw(query).Scan(t.Context(), &count); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return count
}

func certRecord(t *testing.T, s *Store, c *domain.EdgeResolvedConfig, a domain.EdgeResolvedAssignment, at time.Time, days int, notAfter time.Time) {
	t.Helper()
	if _, err := services.NewEdgeRecordingService(s, s, nil).Record(t.Context(), c, a, certResult(days, notAfter), at); err != nil {
		t.Fatalf("record certificate observation: %v", err)
	}
}

func certString(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	var value string
	if err := s.db.NewRaw(query, args...).Scan(t.Context(), &value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// mustReadCertState reads the cursor for the assignment every case in this file
// drives: monitor 17 at generation 1, the pair protectedConfig activates.
func mustReadCertState(t *testing.T, s *Store) (*domain.EdgeCertAlertState, *domain.RegionalIncident) {
	t.Helper()
	var state *domain.EdgeCertAlertState
	var open *domain.RegionalIncident
	if err := s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		state, open, err = readEdgeCertState(ctx, tx, certTestMonitorID, certTestGeneration, testIdentity().ProbeID)
		return err
	}); err != nil {
		t.Fatalf("read certificate cursor: %v", err)
	}
	return state, open
}

const (
	certTestMonitorID  = 17
	certTestGeneration = 1
)

// TestEdgeCertificatePagingDurableLifecycle drives the real recorder, encoder and
// SQLite store end to end: one threshold fires once, an advance retires then
// re-fires in order, and every effect (incident row, cursor, event bytes, delivery
// snapshot) survives a process restart without re-alerting.
func TestEdgeCertificatePagingDurableLifecycle(t *testing.T) {
	s, dir := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	c, a := certAssignment()
	start := time.Now().UTC().Truncate(time.Microsecond)
	notAfter := start.AddDate(0, 0, 25)

	certRecord(t, s, c, a, start, 25, notAfter)

	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate' AND status = 'firing'`); got != 1 {
		t.Fatalf("firing certificate incidents = %d, want 1", got)
	}
	var stored struct {
		SourceAlertID              string
		CertificateThreshold       *int64
		CertificateNotAfter        *int64
		MonitorID                  int64
		Generation                 int64
		Scope, SubjectKind, Reason string
	}
	if err := s.db.NewRaw(`SELECT source_alert_id, certificate_threshold, certificate_not_after, monitor_id, generation, scope, subject_kind, reason
 FROM edge_alerts WHERE subject_kind = 'certificate'`).Scan(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.CertificateThreshold == nil || *stored.CertificateThreshold != 30 || stored.CertificateNotAfter == nil ||
		*stored.CertificateNotAfter != notAfter.UTC().UnixMicro() || stored.MonitorID != 17 || stored.Generation != 1 ||
		stored.Scope != string(domain.IncidentScopeRegional) || !strings.Contains(stored.Reason, "30") {
		t.Fatalf("stored certificate incident lost its identity: %+v", stored)
	}

	cursor, open := mustReadCertState(t, s)
	if cursor == nil || cursor.AlertThreshold != 30 || cursor.SourceAlertID != stored.SourceAlertID || cursor.Version != 1 ||
		cursor.AlertNotAfter == nil || !cursor.AlertNotAfter.Equal(notAfter.UTC()) || !domain.ValidEdgeCertAlertState(cursor) {
		t.Fatalf("unexpected cursor: %+v", cursor)
	}
	if open == nil || open.SourceAlertID != stored.SourceAlertID || open.Status != domain.AlertStatusFiring ||
		!domain.ValidCertificateIncident(open) {
		t.Fatalf("open incident not readable from the cursor: %+v", open)
	}

	// The observation commits first, then its certificate transition, in one
	// transaction: the source sequence is what the hub orders lifecycle by.
	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_telemetry_outbox`); got != 2 {
		t.Fatalf("telemetry events = %d, want 2", got)
	}
	if kind := certString(t, s, `SELECT kind FROM edge_telemetry_outbox WHERE seq = 2`); kind != "alert.transition" {
		t.Fatalf("certificate transition was not appended after the observation: %q", kind)
	}
	transition := strings.ReplaceAll(certString(t, s, `SELECT CAST(payload AS TEXT) FROM edge_telemetry_outbox WHERE seq = 2`), " ", "")
	for _, want := range []string{`"kind":"certificate"`, `"certificate_threshold":30`, `"monitor_id":17`, `"assignment_generation":"1"`, `"status":"firing"`} {
		if !strings.Contains(transition, want) {
			t.Fatalf("emitted event lost %s: %s", want, transition)
		}
	}
	expiryText := `"` + notAfter.UTC().Format("2006-01-02T15:04:05.999999Z07:00") + `"`
	if !strings.Contains(transition, `"certificate_not_after":`+expiryText) {
		t.Fatalf("emitted event lost the exact expiry: %s", transition)
	}

	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_delivery_outbox WHERE event_kind = 'certificate_expiry' AND notification_id = 10`); got != 1 {
		t.Fatalf("certificate_expiry intents = %d, want 1", got)
	}
	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_delivery_outbox WHERE notification_id = 11`); got != 0 {
		t.Fatalf("inactive channel received provider work: %d", got)
	}
	var content struct {
		CertThreshold, CertDaysRemaining, CertNotAfter *int64
		CertIssuer, CheckOutput, IncidentStatus        string
	}
	if err := s.db.NewRaw(`SELECT cert_threshold, cert_days_remaining, cert_issuer, cert_not_after, check_output, incident_status
 FROM edge_delivery_outbox WHERE event_kind = 'certificate_expiry'`).Scan(ctx, &content); err != nil {
		t.Fatal(err)
	}
	if content.CertThreshold == nil || *content.CertThreshold != 30 || content.CertDaysRemaining == nil || *content.CertDaysRemaining != 25 ||
		content.CertNotAfter == nil || *content.CertNotAfter != notAfter.UTC().UnixMicro() || content.CertIssuer != "Phoenix Test CA" ||
		!strings.Contains(content.CheckOutput, "30") || content.IncidentStatus != domain.AlertStatusFiring {
		t.Fatalf("delivery snapshot lost the committed alert content: %+v", content)
	}
	if strings.Contains(content.CheckOutput, "must-never-render") {
		t.Fatal("raw checker metadata leaked into the delivery snapshot")
	}

	// A second check inside the same threshold pages nothing.
	certRecord(t, s, c, a, start.Add(time.Minute), 25, notAfter)
	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate'`); got != 1 {
		t.Fatalf("same threshold re-paged: %d certificate incidents", got)
	}
	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_delivery_outbox`); got != 1 {
		t.Fatalf("same threshold duplicated provider work: %d intents", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A cold process repeats the check. Suppression authority is the durable
	// cursor, not an in-memory flag, so nothing re-fires.
	reopened := certStoreReopen(t, dir)
	certRecord(t, reopened, c, a, start.Add(2*time.Minute), 25, notAfter)
	afterRestart, reopenedOpen := mustReadCertState(t, reopened)
	if afterRestart == nil || afterRestart.AlertThreshold != 30 || afterRestart.Version != 1 || afterRestart.SourceAlertID != stored.SourceAlertID {
		t.Fatalf("restart lost or replayed the cursor: %+v", afterRestart)
	}
	if reopenedOpen == nil || reopenedOpen.Status != domain.AlertStatusFiring {
		t.Fatal("restart lost the open certificate incident")
	}
	if got := certCount(t, reopened, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate'`); got != 1 {
		t.Fatalf("cold start re-paged a delivered threshold: %d incidents", got)
	}

	// Advancing to 14 days retires the 30-day incident, then opens a new identity.
	advance := start.Add(3 * time.Minute)
	certRecord(t, reopened, c, a, advance, 13, notAfter)
	if got := certCount(t, reopened, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate' AND status = 'resolved' AND certificate_threshold = 30`); got != 1 {
		t.Fatalf("30-day incident was not retired: %d", got)
	}
	if got := certCount(t, reopened, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate' AND status = 'firing' AND certificate_threshold = 14`); got != 1 {
		t.Fatalf("14-day threshold did not open: %d", got)
	}
	advanced, advancedOpen := mustReadCertState(t, reopened)
	if advanced == nil || advanced.AlertThreshold != 14 || advanced.Version != 2 || advancedOpen == nil || advancedOpen.CertificateThreshold != 14 {
		t.Fatalf("cursor did not take over the new incident: %+v %+v", advanced, advancedOpen)
	}

	// Event order across the whole run: observation, fire30, observation,
	// observation, observation, retire30, fire14. Retirement precedes its successor.
	var events []struct {
		Seq  int64
		Kind string
	}
	if err := reopened.db.NewRaw(`SELECT seq, kind FROM edge_telemetry_outbox ORDER BY seq`).Scan(ctx, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("unexpected event count %d: %+v", len(events), events)
	}
	if events[5].Kind != "alert.transition" || events[6].Kind != "alert.transition" {
		t.Fatalf("advance did not emit two ordered transitions: %+v", events)
	}
	if !strings.Contains(certString(t, reopened, `SELECT CAST(payload AS TEXT) FROM edge_telemetry_outbox WHERE seq = ?`, events[5].Seq), `"status":"resolved"`) {
		t.Fatalf("resolution must precede the replacement alert: %+v", events)
	}
	if got := certCount(t, reopened, `SELECT COUNT(*) FROM edge_delivery_outbox WHERE event_kind = 'certificate_expiry'`); got != 2 {
		t.Fatalf("threshold advance should add exactly one intent, got %d", got)
	}
	if got := certCount(t, reopened, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate' AND status <> 'resolved'`); got != 1 {
		t.Fatalf("more than one open certificate incident: %d", got)
	}

	// The stored intent still renders the threshold it was committed for.
	restored, err := reopened.GetDeliveryIntent(ctx, testIdentity().ProbeID, certString(t, reopened,
		`SELECT delivery_id FROM edge_delivery_outbox WHERE event_kind = 'certificate_expiry' ORDER BY created_at LIMIT 1`))
	if err != nil {
		t.Fatal(err)
	}
	if restored.Certificate == nil || restored.Certificate.Threshold != 30 || restored.Certificate.DaysRemaining != 25 ||
		!restored.Certificate.NotAfter.Equal(notAfter.UTC()) || !domain.ValidEdgeCertAlertContent(restored.Certificate) {
		t.Fatalf("certificate snapshot did not survive the restart: %+v", restored.Certificate)
	}
	if restored.SourceAlertID != stored.SourceAlertID {
		t.Fatalf("certificate intent lost its incident: %q != %q", restored.SourceAlertID, stored.SourceAlertID)
	}
}

// TestEdgeCertificatePagingFenceAndRollback proves a record built against
// superseded cursor state is refused, and a refused lifecycle leaves no partial
// effect behind.
func TestEdgeCertificatePagingFenceAndRollback(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	c, a := certAssignment()
	start := time.Now().UTC().Truncate(time.Microsecond)
	notAfter := start.AddDate(0, 0, 5)

	certRecord(t, s, c, a, start, 5, notAfter)
	beforeAlerts := certCount(t, s, `SELECT COUNT(*) FROM edge_alerts`)
	beforeEvents := certCount(t, s, `SELECT COUNT(*) FROM edge_telemetry_outbox`)
	beforeWork := certCount(t, s, `SELECT COUNT(*) FROM edge_delivery_outbox`)
	identity, err := s.ReadIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}

	base := domain.EdgeCheckRecord{
		ExpectedStateSeq: 1, ExpectedIncidentVersion: 0,
		Observation: domain.RegionalObservation{MonitorID: 17, ProbeID: testIdentity().ProbeID, StreamID: testIdentity().StreamID,
			AssignmentGeneration: 1, ConfigRevision: 1, Status: domain.StatusUp, RawStatus: domain.StatusUp,
			ObservedAt: start.Add(time.Minute), ReceivedAt: start.Add(time.Minute)},
	}
	// A replay claiming the pre-alert cursor version must be refused as stale
	// local state rather than applied on top of the newer cursor.
	stale := base
	stale.ExpectedCertificateVersion = 0
	if _, err := s.CommitEdgeCheck(ctx, stale); !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatalf("stale certificate fence accepted: %v", err)
	}
	// A record with the right fence but an unusable subject is rejected outright.
	broken := base
	broken.ExpectedCertificateVersion = 1
	broken.Certificate = &domain.EdgeCertAlertWork{
		Cursor: domain.EdgeCertAlertState{MonitorID: 17, AssignmentGeneration: 1, ConfigRevision: 1, UpdatedAt: start, AlertThreshold: 7, Version: 2},
		Transitions: []domain.RegionalIncident{{SourceAlertID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Scope: domain.IncidentScopeRegional,
			SubjectKind: domain.IncidentSubjectCertificate, MonitorID: 17, ProbeID: testIdentity().ProbeID, AssignmentGeneration: 1,
			Status: domain.AlertStatusFiring, TransitionVersion: 1, StartedAt: start, ConfigRevision: 1, Reason: "x", CertificateThreshold: 7}},
	}
	if _, err := s.CommitEdgeCheck(ctx, broken); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("certificate incident without an exact expiry accepted: %v", err)
	}
	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_alerts`); got != beforeAlerts {
		t.Fatalf("rejected commit changed alert rows: %d -> %d", beforeAlerts, got)
	}
	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_telemetry_outbox`); got != beforeEvents {
		t.Fatalf("rejected commit allocated telemetry events: %d -> %d", beforeEvents, got)
	}
	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_delivery_outbox`); got != beforeWork {
		t.Fatalf("rejected commit created provider work: %d -> %d", beforeWork, got)
	}
	after, err := s.ReadIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastCreatedSeq != identity.LastCreatedSeq || after.CommittedSeq != identity.CommittedSeq {
		t.Fatalf("rejected commit advanced the stream sequence: %+v -> %+v", identity, after)
	}
	if _, open := mustReadCertState(t, s); open == nil || open.CertificateThreshold != 7 {
		t.Fatalf("rejected commit disturbed the cursor: %+v", open)
	}
}

// TestEdgeCertificatePagingAbsentEvidenceKeepsHistory proves a check without
// certificate evidence neither resolves nor re-alerts: absence is not recovery.
func TestEdgeCertificatePagingAbsentEvidenceKeepsHistory(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	c, a := certAssignment()
	start := time.Now().UTC().Truncate(time.Microsecond)
	notAfter := start.AddDate(0, 0, 5)

	certRecord(t, s, c, a, start, 5, notAfter)
	before := certCount(t, s, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate'`)

	blind := ports.CheckResult{Status: domain.StatusUp}
	if _, err := services.NewEdgeRecordingService(s, s, nil).Record(t.Context(), c, a, blind, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := certCount(t, s, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate'`); got != before {
		t.Fatalf("evidence-free check changed the certificate lifecycle: %d -> %d", before, got)
	}
	state, open := mustReadCertState(t, s)
	if state == nil || open == nil || state.AlertThreshold != 7 || state.Version != 1 {
		t.Fatalf("evidence-free check disturbed the cursor: %+v %+v", state, open)
	}
}

// TestEdgeCertificatePagingMigrationRoundTrip proves the 011 rebuild preserves
// existing certificate rows and that a downgrade refuses to discard them.
func TestEdgeCertificatePagingMigrationRoundTrip(t *testing.T) {
	s, dir := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	c, a := certAssignment()
	start := time.Now().UTC().Truncate(time.Microsecond)
	certRecord(t, s, c, a, start, 5, start.AddDate(0, 0, 5))
	alertID := certString(t, s, `SELECT source_alert_id FROM edge_alerts WHERE subject_kind = 'certificate'`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	stored := certStoreReopen(t, dir)
	script, err := migrations.ReadFile("migrations/011_certificate_paging.tx.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	err = stored.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, string(script))
		return err
	})
	if err == nil {
		t.Fatal("downgrade discarded certificate paging history")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("downgrade failed for the wrong reason: %v", err)
	}
	if got := certCount(t, stored, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate'`); got != 1 {
		t.Fatalf("aborted downgrade lost certificate incidents: %d", got)
	}
	if got := certCount(t, stored, `SELECT COUNT(*) FROM edge_cert_alert_state`); got != 1 {
		t.Fatalf("aborted downgrade lost the cursor: %d", got)
	}
	if reopened := certStoreReopen(t, dir); true {
		if state, open := mustReadCertState(t, reopened); state == nil || open == nil || open.SourceAlertID != alertID {
			t.Fatalf("certificate state unreadable after the failed downgrade: %+v", state)
		}
	}
}
