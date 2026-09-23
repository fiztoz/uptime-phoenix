package repository_test

import (
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// certExpiry keeps a fractional second so a storage round-trip cannot silently
// round the immutable certificate identity.
func certExpiry(at time.Time) time.Time {
	return at.Add(14*24*time.Hour + 123456*time.Microsecond).UTC()
}

func certificateEvent(r replayFixture, seq, version int64, threshold int64, expiry time.Time, alertID string) domain.ProbeReplayEvent {
	i := domain.RegionalIncident{
		SourceAlertID: alertID, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectCertificate,
		MonitorID: r.monitor, ProbeID: r.session.ProbeID, AssignmentGeneration: 1, Status: domain.AlertStatusFiring,
		TransitionVersion: version, StartedAt: r.at.Add(-time.Second), ConfigRevision: 1,
		CertificateThreshold: threshold, Reason: "certificate expiring",
	}
	copied := expiry
	i.CertificateNotAfter = &copied
	if version == 2 {
		at := r.at
		i.Status, i.ResolvedAt = domain.AlertStatusResolved, &at
	}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindAlertTransition, ObservedAt: r.at, Incident: &i}
}

func certificateDeliveryEvent(r replayFixture, seq int64, alertID string, attempt int64) domain.ProbeReplayEvent {
	d := domain.RegionalDelivery{
		DeliveryID: "99999999-9999-4999-8999-999999999999", SourceAlertID: alertID, SourceTransitionVersion: 1,
		ProbeID: r.session.ProbeID, NotificationID: r.channel, NotificationVersion: 1,
		EventKind: domain.DeliveryEventCertificateExpiry, Attempt: attempt, Status: domain.DeliveryStatusSent, ObservedAt: r.at,
	}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindDeliveryResult, ObservedAt: r.at, Delivery: &d}
}

const certAlertID = "aaaaaaaa-0000-4000-8000-000000000001"

func TestProbeCertificatePagingAcceptance(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("MirrorLifecycleWithoutHubProviderWork", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				observation := r.observation(1)
				observation.Observation.TLS = testTLS(r.at, "paging CA")
				expiry := certExpiry(r.at)
				batch := r.batch(
					observation,
					certificateEvent(r, 2, 1, 30, expiry, certAlertID),
					certificateDeliveryEvent(r, 3, certAlertID, 1),
					certificateEvent(r, 4, 2, 30, expiry, certAlertID),
				)
				got := r.ingest(t, batch)
				if got.AcceptedCount != 4 || len(got.Rejected) != 0 {
					t.Fatalf("certificate lifecycle rejected: %+v", got)
				}

				stored := mustCertificateIncident(t, r)
				if stored.SubjectKind != domain.IncidentSubjectCertificate || stored.Status != domain.AlertStatusResolved ||
					stored.TransitionVersion != 2 || stored.CertificateThreshold != 30 ||
					stored.CertificateNotAfter == nil || !stored.CertificateNotAfter.Equal(expiry.Truncate(time.Microsecond)) ||
					stored.MonitorID != r.monitor || stored.AssignmentGeneration != 1 || stored.ProbeID != r.session.ProbeID {
					t.Fatalf("mirrored certificate incident lost its identity: %+v", stored)
				}
				// The source reported one delivered attempt; the hub mirrors that
				// outcome and never treats it as a send request of its own.
				deliveries := mustCertificateDeliveries(t, r)
				if len(deliveries) != 1 || deliveries[0].EventKind != domain.DeliveryEventCertificateExpiry ||
					deliveries[0].Attempt != 1 || deliveries[0].Status != domain.DeliveryStatusSent {
					t.Fatalf("mirrored certificate delivery differs: %+v", deliveries)
				}
				if n := replayCount(t, r.f, "probe_delivery_intents"); n != 0 {
					t.Fatalf("mirrored certificate history created %d hub provider intents", n)
				}
				// The delivered-threshold cursor stays source-owned.
				if n := replayCount(t, r.f, "tls_info"); n != 1 {
					t.Fatalf("certificate mirror changed tls_info rows: %d", n)
				}
				_, repo := boundAuxiliary(t, r.f, r.session.ProbeID, 1)
				info, err := repo.GetByMonitorID(t.Context(), r.monitor)
				if err != nil || info.LastCertAlertThreshold != 0 || !info.LastCertAlertNotAfter.IsZero() {
					t.Fatalf("hub inferred a source notification cursor: %+v %v", info, err)
				}

				// A lost ACK replays the whole batch: no new rows, no new work.
				replayed := r.ingest(t, batch)
				if replayed.DuplicateCount != 4 || replayCount(t, r.f, "probe_incidents") != 1 ||
					replayCount(t, r.f, "probe_delivery_events") != 1 || replayCount(t, r.f, "probe_delivery_intents") != 0 {
					t.Fatalf("duplicate receipt duplicated certificate effects: %+v", replayed)
				}
			})

			t.Run("SubjectIdentityIsImmutable", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				expiry := certExpiry(r.at)
				r.ingest(t, r.batch(certificateEvent(r, 1, 1, 30, expiry, certAlertID)))

				// The same incident ID may never restate its threshold...
				restated := certificateEvent(r, 2, 2, 14, expiry, certAlertID)
				if got := r.ingest(t, r.batch(restated)); got.AcceptedCount != 0 || len(got.Rejected) != 1 ||
					got.Rejected[0].Code != "transition_identity_conflict" {
					t.Fatalf("threshold restatement accepted: %+v", got)
				}
				// ...or its exact expiry.
				shifted := certificateEvent(r, 3, 2, 30, expiry.Add(time.Hour), certAlertID)
				if got := r.ingest(t, r.batch(shifted)); got.AcceptedCount != 0 || len(got.Rejected) != 1 ||
					got.Rejected[0].Code != "transition_identity_conflict" {
					t.Fatalf("expiry restatement accepted: %+v", got)
				}
				stored := mustCertificateIncident(t, r)
				if stored.TransitionVersion != 1 || stored.Status != domain.AlertStatusFiring || stored.CertificateThreshold != 30 ||
					!stored.CertificateNotAfter.Equal(expiry.Truncate(time.Microsecond)) {
					t.Fatalf("rejected transitions mutated the mirror: %+v", stored)
				}
				// A distinct identity for the next threshold is admitted.
				next := certificateEvent(r, 4, 1, 14, expiry, "aaaaaaaa-0000-4000-8000-000000000002")
				if got := r.ingest(t, r.batch(next)); got.AcceptedCount != 1 {
					t.Fatalf("distinct threshold identity rejected: %+v", got)
				}
				if n := replayCount(t, r.f, "probe_incidents"); n != 2 {
					t.Fatalf("unexpected certificate incident count %d", n)
				}
			})

			t.Run("AcknowledgementAndMalformedSubjectRejected", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				expiry := certExpiry(r.at)
				acked := certificateEvent(r, 1, 2, 30, expiry, certAlertID)
				at := r.at
				note := "looked at it"
				acked.Incident.Status = domain.AlertStatusAcked
				acked.Incident.AckedAt, acked.Incident.AckCommandID, acked.Incident.AckActorDisplayName, acked.Incident.AckNote = &at, certAlertID, "operator", &note
				if got := r.ingest(t, r.batch(acked)); got.AcceptedCount != 0 || len(got.Rejected) != 1 || got.Rejected[0].Code != "event_invalid" {
					t.Fatalf("certificate acknowledgement accepted: %+v", got)
				}
				if replayCount(t, r.f, "probe_incidents") != 0 {
					t.Fatal("rejected acknowledgement persisted")
				}

				// No exact expiry means no certificate identity at all.
				orphan := certificateEvent(r, 2, 1, 30, expiry, certAlertID)
				orphan.Incident.CertificateNotAfter = nil
				if got := r.ingest(t, r.batch(orphan)); got.AcceptedCount != 0 || len(got.Rejected) != 1 || got.Rejected[0].Code != "event_invalid" {
					t.Fatalf("certificate incident without expiry accepted: %+v", got)
				}
				// An unfixed threshold is not part of the V1 contract.
				unknown := certificateEvent(r, 3, 1, 5, expiry, certAlertID)
				if got := r.ingest(t, r.batch(unknown)); got.AcceptedCount != 0 || len(got.Rejected) != 1 || got.Rejected[0].Code != "event_invalid" {
					t.Fatalf("unsupported certificate threshold accepted: %+v", got)
				}
				if replayCount(t, r.f, "probe_incidents") != 0 {
					t.Fatal("malformed certificate subjects persisted")
				}
			})

			t.Run("DeliveryWithoutAuthorizedTransition", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				if got := r.ingest(t, r.batch(certificateDeliveryEvent(r, 1, certAlertID, 1))); got.AcceptedCount != 0 ||
					len(got.Rejected) != 1 || got.Rejected[0].Code != "delivery_parent_not_found" {
					t.Fatalf("orphan certificate delivery accepted: %+v", got)
				}
				if replayCount(t, r.f, "probe_delivery_events") != 0 {
					t.Fatal("orphan delivery persisted")
				}
			})

			t.Run("MigrationRoundTripAndCertificateGuard", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				// A successful 067 down really does remove a column that every other
				// probe_incidents reader selects, and MariaDB cannot roll DDL back. The
				// shared disposable CI schema is used by parallel packages, so the
				// destructive direction is exercised only against this test's private
				// SQLite file; on MariaDB the 067 up path is exercised by RunMigrations
				// in every fixture, and here only the guard that must never drop is.
				if engine == "sqlite" {
					for _, direction := range []string{"down", "up"} {
						if err := runEngineMigration(t, r.f.db, engine, "067_probe_certificate_paging", direction); err != nil {
							t.Fatalf("067 %s on an empty certificate column: %v", direction, err)
						}
					}
				}
				r.ingest(t, r.batch(certificateEvent(r, 1, 1, 30, certExpiry(r.at), certAlertID)))
				// The guard is the only thing standing between this script and a real
				// DROP COLUMN on a schema that every other MariaDB reader in the package
				// shares. Refuse to run the destructive script at all unless the row that
				// must trip the guard is provably present.
				if n := replayCount(t, r.f, "probe_incidents"); n != 1 || mustCertificateIncident(t, r).CertificateNotAfter == nil {
					t.Fatalf("certificate row absent before the guard rehearsal (count=%d); refusing to run 067 down", n)
				}
				err := runEngineMigration(t, r.f.db, engine, "067_probe_certificate_paging", "down")
				if err == nil {
					t.Fatal("downgrade discarded mirrored certificate history")
				}
				// Each engine names what it rejected differently: SQLite reports the
				// check expression, MariaDB the table-qualified constraint. Both must be
				// a guard violation, not an unrelated failure that happens to error.
				guard := "CHECK constraint failed"
				if engine == "mariadb" {
					guard = "probe_certificate_paging_downgrade_guard"
				}
				if !strings.Contains(err.Error(), guard) {
					t.Fatalf("downgrade failed for the wrong reason: %v", err)
				}
				stored := mustCertificateIncident(t, r)
				if stored.CertificateNotAfter == nil || !stored.CertificateNotAfter.Equal(certExpiry(r.at).Truncate(time.Microsecond)) {
					t.Fatalf("aborted downgrade lost the certificate expiry: %+v", stored)
				}
			})
		})
	}
}

func mustCertificateIncident(t *testing.T, r replayFixture) domain.RegionalIncident {
	t.Helper()
	var stored struct {
		SubjectKind, Status                             string
		TransitionVersion                               int64
		CertificateThreshold                            *int64
		CertificateNotAfter                             *time.Time
		MonitorID, AssignmentGeneration, ConfigRevision int64
		ProbeID                                         string
	}
	if err := r.f.db.NewRaw(`SELECT subject_kind, status, transition_version, certificate_threshold, certificate_not_after,
 monitor_id, assignment_generation, config_revision, probe_id FROM probe_incidents WHERE source_alert_id = ?`, certAlertID).
		Scan(t.Context(), &stored); err != nil {
		t.Fatalf("read mirrored certificate incident: %v", err)
	}
	if stored.CertificateThreshold == nil || stored.CertificateNotAfter == nil {
		t.Fatalf("mirrored certificate incident lost its subject: %+v", stored)
	}
	return domain.RegionalIncident{
		SubjectKind: stored.SubjectKind, Status: stored.Status, TransitionVersion: stored.TransitionVersion,
		CertificateThreshold: *stored.CertificateThreshold, CertificateNotAfter: stored.CertificateNotAfter,
		MonitorID: stored.MonitorID, AssignmentGeneration: stored.AssignmentGeneration, ConfigRevision: stored.ConfigRevision,
		ProbeID: stored.ProbeID,
	}
}

func mustCertificateDeliveries(t *testing.T, r replayFixture) []domain.RegionalDelivery {
	t.Helper()
	var rows []struct {
		EventKind, Status string
		Attempt           int64
	}
	if err := r.f.db.NewRaw(`SELECT event_kind, status, attempt FROM probe_delivery_events WHERE source_alert_id = ?`, certAlertID).
		Scan(t.Context(), &rows); err != nil {
		t.Fatalf("read mirrored certificate deliveries: %v", err)
	}
	out := make([]domain.RegionalDelivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.RegionalDelivery{EventKind: row.EventKind, Status: row.Status, Attempt: row.Attempt})
	}
	return out
}
