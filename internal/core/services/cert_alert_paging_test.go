package services

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// certFixture builds an opted-in assignment with one active and one inactive
// channel, so intent fan-out and active filtering are both observable.
func certFixture() (*domain.EdgeResolvedConfig, domain.EdgeResolvedAssignment) {
	a := domain.EdgeResolvedAssignment{
		Monitor:           &domain.Monitor{ID: 1, Name: "portal", Type: "http", Active: true, CertExpiryNotify: true},
		Generation:        2,
		NotificationLinks: []domain.MonitorNotification{{MonitorID: 1, NotificationID: 7}, {MonitorID: 1, NotificationID: 8}},
	}
	c := &domain.EdgeResolvedConfig{
		Metadata:    domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: "hub", ProbeID: "probe"}, Revision: 3},
		Assignments: []domain.EdgeResolvedAssignment{a},
		Channels: map[int64]domain.EdgeResolvedChannel{
			7: {Notification: &domain.Notification{ID: 7, Type: "webhook", Active: true}, Version: 3},
			8: {Notification: &domain.Notification{ID: 8, Type: "slack", Active: false}, Version: 3},
		},
		Maintenance: map[int64]*domain.MaintenanceWindow{},
	}
	return c, a
}

// certIDs hands out distinct canonical UUIDs and counts calls, so a test can
// prove that a path which must stay silent invents no identity at all.
type certIDs struct{ issued int }

func (c *certIDs) next() (string, error) {
	c.issued++
	if c.issued > 9 {
		return "", errors.New("id sequence exhausted")
	}
	return strings.Replace("11111111-1111-4111-8111-111111111111", "1", string(rune('0'+c.issued)), 1), nil
}

func certEvidence(days int, notAfter time.Time) *domain.TLSObservation {
	return &domain.TLSObservation{NotAfter: notAfter, DaysRemaining: int64(days), Issuer: "Phoenix Test CA"}
}

func certCursor(threshold int, alertNotAfter, updatedAt time.Time, alertID string) *domain.EdgeCertAlertState {
	state := &domain.EdgeCertAlertState{MonitorID: 1, AssignmentGeneration: 2, ConfigRevision: 3, UpdatedAt: updatedAt, Version: 1, AlertThreshold: threshold}
	if threshold != 0 {
		expiry := alertNotAfter
		state.AlertNotAfter, state.SourceAlertID = &expiry, alertID
	}
	return state
}

func certOpenIncident(threshold int, notAfter time.Time, alertID string, started time.Time) *domain.RegionalIncident {
	expiry := notAfter
	return &domain.RegionalIncident{
		SourceAlertID: alertID, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectCertificate,
		MonitorID: 1, ProbeID: "probe", AssignmentGeneration: 2, Status: domain.AlertStatusFiring, TransitionVersion: 1,
		StartedAt: started, Reason: "previously delivered", ConfigRevision: 3,
		CertificateThreshold: int64(threshold), CertificateNotAfter: &expiry,
	}
}

type certCase struct {
	now         time.Time
	evidence    *domain.TLSObservation
	prior       *domain.EdgeCertAlertState
	priorOpen   *domain.RegionalIncident
	maintenance bool
	ids         *certIDs
}

func (tc certCase) input(c *domain.EdgeResolvedConfig, a domain.EdgeResolvedAssignment) CertAlertPagingInput {
	if tc.ids == nil {
		tc.ids = &certIDs{}
	}
	ids := tc.ids
	return CertAlertPagingInput{
		Config: c, Monitor: a.Monitor, Assignment: a, Maintenance: tc.maintenance, Now: tc.now, NewID: ids.next,
		Observation: domain.RegionalObservation{
			MonitorID: a.Monitor.ID, ProbeID: "probe", AssignmentGeneration: a.Generation,
			ConfigRevision: c.Metadata.Revision, Status: domain.StatusUp, TLS: tc.evidence, ObservedAt: tc.now,
		},
		Prior: tc.prior, PriorCursor: tc.priorOpen,
	}
}

func TestCertAlertPagingRequiresTrustedContext(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	notAfter := now.AddDate(0, 0, 5)
	c, a := certFixture()
	openID := "11111111-1111-4111-8111-111111111111"

	cases := map[string]func(*CertAlertPagingInput){
		"missing config":  func(in *CertAlertPagingInput) { in.Config = nil },
		"missing monitor": func(in *CertAlertPagingInput) { in.Monitor = nil },
		"zero monitor":    func(in *CertAlertPagingInput) { in.Monitor = &domain.Monitor{ID: 0, CertExpiryNotify: true} },
		"zero generation": func(in *CertAlertPagingInput) { in.Assignment.Generation = 0 },
		"zero clock":      func(in *CertAlertPagingInput) { in.Now = time.Time{} },
		"no id source":    func(in *CertAlertPagingInput) { in.NewID = nil },
		"foreign cursor": func(in *CertAlertPagingInput) {
			in.Prior = &domain.EdgeCertAlertState{MonitorID: 99, AssignmentGeneration: 2, ConfigRevision: 3, UpdatedAt: now}
		},
		"other assignment": func(in *CertAlertPagingInput) {
			in.Prior = &domain.EdgeCertAlertState{MonitorID: 1, AssignmentGeneration: 9, ConfigRevision: 3, UpdatedAt: now}
		},
		"pending status": func(in *CertAlertPagingInput) { in.Observation.Status = domain.StatusPending },
		"unbounded issuer": func(in *CertAlertPagingInput) {
			in.Observation.TLS = &domain.TLSObservation{NotAfter: notAfter, DaysRemaining: 5, Issuer: strings.Repeat("x", 257)}
		},
		"cursor without row": func(in *CertAlertPagingInput) {
			in.Prior, in.PriorCursor = certCursor(30, notAfter, now, openID), nil
		},
		"cursor wrong subject": func(in *CertAlertPagingInput) {
			in.Prior = certCursor(30, notAfter, now, openID)
			in.PriorCursor = certOpenIncident(30, notAfter, openID, now)
			in.PriorCursor.SubjectKind = domain.IncidentSubjectAvailability
		},
	}

	// Sanity: the unmutated baseline must be valid, or every assertion below is
	// vacuously true.
	if _, err := EvaluateCertAlertPaging(certCase{now: now, evidence: certEvidence(5, notAfter)}.input(c, a)); err != nil {
		t.Fatalf("baseline input rejected: %v", err)
	}
	for name := range cases {
		t.Run(name, func(t *testing.T) {
			mutation := cases[name]
			in := certCase{now: now, evidence: certEvidence(5, notAfter)}.input(c, a)
			mutation(&in)
			work, err := EvaluateCertAlertPaging(in)
			if !errors.Is(err, domain.ErrValidation) || work != nil {
				t.Fatalf("invalid context accepted: work=%+v err=%v", work, err)
			}
		})
	}
}

func TestCertAlertPagingStaysSilentWithoutObligation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, optedOut := certFixture()
	optedOut.Monitor.CertExpiryNotify = false
	ids := &certIDs{}
	in := certCase{now: now, evidence: certEvidence(1, now.AddDate(0, 0, 1)), ids: ids}.input(c, optedOut)
	if work, err := EvaluateCertAlertPaging(in); err != nil || work != nil {
		t.Fatalf("opt-out assignment paged: %+v %v", work, err)
	}

	c, a := certFixture()
	for name, evidence := range map[string]*domain.TLSObservation{
		"absent evidence":          nil,
		"outside every threshold":  certEvidence(45, now.AddDate(0, 0, 45)),
		"exactly outside the edge": certEvidence(31, now.AddDate(0, 0, 31)),
	} {
		t.Run(name, func(t *testing.T) {
			work, err := EvaluateCertAlertPaging(certCase{now: now, evidence: evidence, ids: ids}.input(c, a))
			if err != nil || work != nil {
				t.Fatalf("unexpected work: %+v %v", work, err)
			}
		})
	}
	if ids.issued != 0 {
		t.Fatalf("silent paths invented %d identities", ids.issued)
	}
}

func TestCertAlertPagingFiresEachThresholdOnce(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	notAfter := now.AddDate(0, 0, 25)
	c, a := certFixture()

	work, err := EvaluateCertAlertPaging(certCase{now: now, evidence: certEvidence(25, notAfter)}.input(c, a))
	if err != nil {
		t.Fatal(err)
	}
	if work == nil || len(work.Transitions) != 1 || len(work.Intents) != 1 {
		t.Fatalf("first crossing produced %+v", work)
	}
	firing := work.Transitions[0]
	if firing.SubjectKind != domain.IncidentSubjectCertificate || firing.Status != domain.AlertStatusFiring ||
		firing.TransitionVersion != 1 || firing.CertificateThreshold != 30 || firing.CertificateNotAfter == nil ||
		!firing.CertificateNotAfter.Equal(notAfter) || firing.MonitorID != a.Monitor.ID || firing.AssignmentGeneration != a.Generation ||
		firing.ProbeID != "probe" || firing.ConfigRevision != 3 || !domain.ValidCertificateIncident(&firing) {
		t.Fatalf("unexpected firing identity: %+v", firing)
	}
	if work.Cursor.AlertThreshold != 30 || work.Cursor.AlertNotAfter == nil || !work.Cursor.AlertNotAfter.Equal(notAfter) ||
		work.Cursor.SourceAlertID != firing.SourceAlertID || work.Cursor.Version != 1 || !domain.ValidEdgeCertAlertState(&work.Cursor) {
		t.Fatalf("cursor not advanced to the fired threshold: %+v", work.Cursor)
	}
	intent := work.Intents[0]
	if intent.EventKind != domain.DeliveryEventCertificateExpiry || intent.SourceAlertID != firing.SourceAlertID ||
		intent.SourceTransitionVersion != 1 || intent.NotificationID != 7 || intent.NotificationVersion != 3 || intent.ProbeID != "probe" {
		t.Fatalf("unexpected intent: %+v", intent)
	}
	content := work.Certificate
	if content == nil || content.Threshold != 30 || content.DaysRemaining != 25 || content.Issuer != "Phoenix Test CA" ||
		!content.NotAfter.Equal(notAfter) || !domain.ValidEdgeCertAlertContent(content) || !strings.Contains(content.Message, "30") {
		t.Fatalf("unexpected snapshot content: %+v", content)
	}

	// The delivered threshold covers every looser reading of the same certificate.
	suppressionIDs := &certIDs{}
	for _, days := range []int{30, 29, 25, 20} {
		crossed := certCase{now: now.AddDate(0, 0, 25-days), evidence: certEvidence(days, notAfter),
			prior: certCursor(30, notAfter, now, firing.SourceAlertID), priorOpen: certOpenIncident(30, notAfter, firing.SourceAlertID, now), ids: suppressionIDs}
		if work, err := EvaluateCertAlertPaging(crossed.input(c, a)); err != nil || work != nil {
			t.Fatalf("threshold 30 already delivered, but %d days re-paged: %+v %v", days, work, err)
		}
	}
	if suppressionIDs.issued != 0 {
		t.Fatalf("suppressed re-alerts issued %d identities", suppressionIDs.issued)
	}
}

func TestCertAlertPagingThresholdAdvanceRetiresThenFires(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	opened := now.AddDate(0, 0, -5)
	notAfter := opened.AddDate(0, 0, 30)
	c, a := certFixture()
	openID := "11111111-1111-4111-8111-111111111111"
	ids := &certIDs{issued: 1}

	// 30 days were delivered on `opened`; 13 days remain now.
	work, err := EvaluateCertAlertPaging(certCase{now: now, evidence: certEvidence(13, notAfter),
		prior: certCursor(30, notAfter, opened, openID), priorOpen: certOpenIncident(30, notAfter, openID, opened), ids: ids}.input(c, a))
	if err != nil {
		t.Fatal(err)
	}
	if len(work.Transitions) != 2 {
		t.Fatalf("expected a retirement then a firing transition, got %+v", work.Transitions)
	}
	retired, fired := work.Transitions[0], work.Transitions[1]
	if retired.SourceAlertID != openID || retired.Status != domain.AlertStatusResolved || retired.TransitionVersion != 2 ||
		retired.ResolvedAt == nil || retired.ResolvedAt.Before(now) || retired.CertificateThreshold != 30 ||
		retired.CertificateNotAfter == nil || !retired.CertificateNotAfter.Equal(notAfter) ||
		!retired.StartedAt.Equal(opened) || retired.Reason == "" || len(retired.Reason) > 4096 {
		t.Fatalf("retirement altered the immutable subject or lifecycle: %+v", retired)
	}
	if fired.Status != domain.AlertStatusFiring || fired.TransitionVersion != 1 || fired.CertificateThreshold != 14 ||
		fired.SourceAlertID == openID || fired.CertificateNotAfter == nil || !fired.CertificateNotAfter.Equal(notAfter) {
		t.Fatalf("advance did not open a distinct identity for the same certificate: %+v", fired)
	}
	if len(work.Intents) != 1 || work.Intents[0].SourceAlertID != fired.SourceAlertID || work.Intents[0].SourceTransitionVersion != 1 {
		t.Fatalf("intents must reference the newly fired incident only: %+v", work.Intents)
	}
	if work.Cursor.AlertThreshold != 14 || work.Cursor.SourceAlertID != fired.SourceAlertID || work.Cursor.Version != 2 {
		t.Fatalf("cursor did not take over the new incident: %+v", work.Cursor)
	}
	if !strings.Contains(retired.Reason, "30") {
		t.Fatalf("retirement reason must name the superseded threshold: %q", retired.Reason)
	}
}

func TestCertAlertPagingRecoveryAndRenewal(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, a := certFixture()
	openID := "11111111-1111-4111-8111-111111111111"
	alerted := now.AddDate(0, 0, -3)
	old := alerted.AddDate(0, 0, 7)

	t.Run("recovery past every threshold retires without alerting", func(t *testing.T) {
		ids := &certIDs{issued: 1}
		work, err := EvaluateCertAlertPaging(certCase{now: now, evidence: certEvidence(90, now.AddDate(0, 0, 90)),
			prior: certCursor(7, old, alerted, openID), priorOpen: certOpenIncident(7, old, openID, alerted), ids: ids}.input(c, a))
		if err != nil {
			t.Fatal(err)
		}
		if len(work.Transitions) != 1 || work.Transitions[0].Status != domain.AlertStatusResolved {
			t.Fatalf("recovery must retire only: %+v", work.Transitions)
		}
		if len(work.Intents) != 0 || work.Certificate != nil {
			t.Fatal("recovery created provider work")
		}
		if work.Cursor.AlertThreshold != 0 || work.Cursor.AlertNotAfter != nil || work.Cursor.SourceAlertID != "" || work.Cursor.Version != 2 {
			t.Fatalf("recovered cursor still claims a delivered threshold: %+v", work.Cursor)
		}
		if !domain.ValidEdgeCertAlertState(&work.Cursor) {
			t.Fatal("recovered cursor violates its own invariant")
		}
		if ids.issued != 1 {
			t.Fatalf("recovery allocated %d extra identities", ids.issued-1)
		}
	})

	t.Run("renewal inside a threshold opens a new identity", func(t *testing.T) {
		renewed := now.AddDate(0, 0, 6)
		ids := &certIDs{issued: 1}
		work, err := EvaluateCertAlertPaging(certCase{now: now, evidence: certEvidence(5, renewed),
			prior: certCursor(7, old, alerted, openID), priorOpen: certOpenIncident(7, old, openID, alerted), ids: ids}.input(c, a))
		if err != nil {
			t.Fatal(err)
		}
		if len(work.Transitions) != 2 {
			t.Fatalf("renewal must retire the superseded incident then page the new one: %+v", work.Transitions)
		}
		if work.Transitions[0].CertificateNotAfter == nil || !work.Transitions[0].CertificateNotAfter.Equal(old) {
			t.Fatal("retirement lost the superseded certificate identity")
		}
		if work.Transitions[1].SourceAlertID == openID || !work.Transitions[1].CertificateNotAfter.Equal(renewed) {
			t.Fatalf("renewal reused the previous certificate incident: %+v", work.Transitions[1])
		}
		if work.Cursor.SourceAlertID != work.Transitions[1].SourceAlertID || work.Cursor.AlertThreshold != 7 {
			t.Fatalf("renewed cursor did not adopt the new identity: %+v", work.Cursor)
		}
	})
}

func TestCertAlertPagingMaintenanceConsumesNothing(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	notAfter := now.AddDate(0, 0, 5)
	c, a := certFixture()
	ids := &certIDs{}

	if work, err := EvaluateCertAlertPaging(certCase{now: now, evidence: certEvidence(5, notAfter), maintenance: true, ids: ids}.input(c, a)); err != nil || work != nil {
		t.Fatalf("maintenance produced work: %+v %v", work, err)
	}
	if ids.issued != 0 {
		t.Fatalf("maintenance consumed %d identities", ids.issued)
	}

	// The window neither consumed the threshold nor spent the alert: the next
	// observation outside the window pages normally.
	after, err := EvaluateCertAlertPaging(certCase{now: now.Add(time.Minute), evidence: certEvidence(5, notAfter), ids: ids}.input(c, a))
	if err != nil || after == nil || len(after.Transitions) != 1 || len(after.Intents) != 1 {
		t.Fatalf("post-maintenance check failed to page: %+v %v", after, err)
	}

	// Suppression also holds the administrative lifecycle: an incident opened
	// before the window stays open instead of being quietly retired.
	openID := "21111111-1111-4111-8111-111111111111"
	suppressed := certCase{now: now, evidence: certEvidence(45, now.AddDate(0, 0, 45)), maintenance: true,
		prior: certCursor(7, notAfter, now.AddDate(0, 0, -1), openID), priorOpen: certOpenIncident(7, notAfter, openID, now.AddDate(0, 0, -3)), ids: ids}
	if work, err := EvaluateCertAlertPaging(suppressed.input(c, a)); err != nil || work != nil {
		t.Fatalf("maintenance retired an incident behind the operator's back: %+v %v", work, err)
	}
}

func TestCertAlertPagingChannelGraphRules(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	evidence := certEvidence(5, now.AddDate(0, 0, 5))

	c, a := certFixture()
	c.Channels[8].Notification.Active = true
	work, err := EvaluateCertAlertPaging(certCase{now: now, evidence: evidence}.input(c, a))
	if err != nil {
		t.Fatal(err)
	}
	if len(work.Intents) != 2 || work.Intents[0].NotificationID != 7 || work.Intents[1].NotificationID != 8 {
		t.Fatalf("every active channel must receive work: %+v", work.Intents)
	}
	if work.Intents[0].SourceAlertID != work.Intents[1].SourceAlertID {
		t.Fatal("two channels of one alert must share one incident identity")
	}

	c, a = certFixture()
	a.NotificationLinks = []domain.MonitorNotification{{MonitorID: 1, NotificationID: 99}}
	c.Channels[99] = domain.EdgeResolvedChannel{Notification: nil, Version: 3}
	if _, err := EvaluateCertAlertPaging(certCase{now: now, evidence: evidence}.input(c, a)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a link with no channel in the accepted graph was skipped silently: %v", err)
	}

	c, a = certFixture()
	a.NotificationLinks = nil
	work, err = EvaluateCertAlertPaging(certCase{now: now, evidence: evidence}.input(c, a))
	if err != nil {
		t.Fatal(err)
	}
	if len(work.Intents) != 0 || len(work.Transitions) != 1 {
		t.Fatalf("an unlinked monitor must still open its incident: %+v", work)
	}
}

func TestCertAlertPagingClockAndExpiredEdges(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, a := certFixture()

	// An already-expired certificate is the tightest case, not an unhandled one.
	work, err := EvaluateCertAlertPaging(certCase{now: now, evidence: certEvidence(-2, now.AddDate(0, 0, -2))}.input(c, a))
	if err != nil {
		t.Fatal(err)
	}
	if work == nil || work.Transitions[0].CertificateThreshold != 7 {
		t.Fatalf("expired certificate did not page at the tightest threshold: %+v", work)
	}
	if work.Certificate.DaysRemaining != 0 || work.Certificate.Message == "" {
		t.Fatalf("an expired certificate must not render negative days: %+v", work.Certificate)
	}

	// A looser reading of the same certificate (a clock correction) must neither
	// reopen nor retire the incident that already delivered the tighter alert.
	notAfter := now.AddDate(0, 0, 7)
	openID := "11111111-1111-4111-8111-111111111111"
	ids := &certIDs{issued: 1}
	looser := certCase{now: now, evidence: certEvidence(13, notAfter),
		prior: certCursor(7, notAfter, now, openID), priorOpen: certOpenIncident(7, notAfter, openID, now), ids: ids}
	if work, err := EvaluateCertAlertPaging(looser.input(c, a)); err != nil || work != nil {
		t.Fatalf("a looser reading re-paged the same certificate: %+v %v", work, err)
	}
	if ids.issued != 1 {
		t.Fatal("suppressed path allocated an identity")
	}
}

func TestCertAlertPagingMalformedEvidenceIsRejected(t *testing.T) {
	now := time.Now().UTC()
	c, a := certFixture()
	// A zero expiry cannot identify a certificate, and ValidTLSObservation
	// rejects it, so it must fail closed rather than silently alert on nothing.
	in := certCase{now: now, evidence: &domain.TLSObservation{DaysRemaining: 1}}.input(c, a)
	if _, err := EvaluateCertAlertPaging(in); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a certificate without an exact expiry was accepted: %v", err)
	}
}
