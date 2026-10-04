package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func capacityEvidenceSample(kind string, state domain.ConditionState, percent float64, at time.Time) domain.ConditionObservation {
	threshold := 80.0
	limit := 100.0
	used := percent
	return domain.ConditionObservation{
		Kind: kind, State: state, Used: &used, Limit: &limit, Percent: &percent, Threshold: &threshold,
		Unit: "bytes", Resource: "Database size", Scope: "database", Source: "fixed engine query",
		Message: "sampled", ObservedAt: at, StaleAfter: at.Add(3 * time.Minute),
	}
}

func capacityEvidence(kind string, observed, effective domain.ConditionState, percent float64, at time.Time, count int) domain.ConditionEvidence {
	evidence := domain.ConditionEvidence{
		ConditionObservation: capacityEvidenceSample(kind, observed, percent, at),
		ConsecutiveState:     observed,
		ConsecutiveCount:     count,
	}
	if effective != "" {
		state := effective
		evidence.EffectiveState = &state
		evidence.ConsecutiveState = effective
	}
	if observed != domain.ConditionStateError {
		last := at
		evidence.LastSuccessAt = &last
	}
	return evidence
}

func capacityObservationEvent(r replayFixture, seq int64, samples ...domain.ConditionObservation) domain.ProbeReplayEvent {
	event := r.observation(seq)
	event.Observation.Conditions = samples
	return event
}

func capacityTransitionEvent(r replayFixture, seq int64, previous *domain.ConditionState, state domain.ConditionState) domain.ProbeReplayEvent {
	transition := domain.ConditionTransition{MonitorID: r.monitor, AssignmentGeneration: 1, ConfigRevision: 1, Kind: domain.MonitorConditionStorage, PreviousState: previous, State: state, Message: "promoted"}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindConditionTransition, ObservedAt: r.at, Condition: &transition}
}

func assertCondition(t *testing.T, r replayFixture, wantState domain.ConditionState, wantEffective bool, wantPercent float64) {
	t.Helper()
	repo, _ := boundAuxiliary(t, r.f, r.session.ProbeID, 1)
	condition, err := repo.Get(t.Context(), r.monitor, domain.MonitorConditionStorage)
	if err != nil {
		t.Fatal("mirrored condition missing:", err)
	}
	if condition.State != wantState || (condition.State != "") != wantEffective {
		t.Fatalf("mirrored promoted state: got %q want %q", condition.State, wantState)
	}
	if condition.Percent == nil || *condition.Percent != wantPercent {
		t.Fatalf("mirrored measurement: got %+v want %v", condition.Percent, wantPercent)
	}
	if condition.ProbeID != r.session.ProbeID || condition.AssignmentGeneration != 1 {
		t.Fatalf("mirror escaped its assignment scope: %+v", condition)
	}
}

func TestProbeCapacityStateAcceptance(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("RawHistoryAndUnconfirmedState", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				sample := capacityEvidenceSample(domain.MonitorConditionStorage, domain.ConditionStateWarning, 84, r.at)
				if got := r.ingest(t, r.batch(capacityObservationEvent(r, 1, sample))); got.AcceptedCount != 1 || len(got.Rejected) != 0 {
					t.Fatal("capacity evidence rejected", got)
				}
				// A raw sample never establishes a promoted condition: the mirror
				// starts unconfirmed with the observed candidate.
				assertCondition(t, r, "", false, 84)
				history, err := r.f.commits.ListObservations(t.Context(), r.monitor, r.session.ProbeID, r.at.Add(-time.Minute), r.at.Add(time.Minute))
				if err != nil || len(history) != 1 || len(history[0].Conditions) != 1 {
					t.Fatal("raw capacity history lost", err, history)
				}
				stored := history[0].Conditions[0]
				if stored.State != domain.ConditionStateWarning || stored.Percent == nil || *stored.Percent != 84 || !stored.ObservedAt.Equal(r.at) || !stored.StaleAfter.Equal(sample.StaleAfter) {
					t.Fatalf("history changed raw evidence: %+v", stored)
				}
				for _, table := range []string{"probe_incidents", "probe_delivery_intents", "alerts", "alert_escalations"} {
					if replayCount(t, r.f, table) != 0 {
						t.Fatal("capacity evidence created alert/provider work", table)
					}
				}
				state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Status != domain.StatusUp {
					t.Fatal("capacity evidence changed availability", state, err)
				}
			})

			t.Run("PromotedTransitionMirrorsStateWithoutReevaluation", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				warning := domain.ConditionStateWarning
				ok := domain.ConditionStateOK
				r.ingest(t, r.batch(
					capacityObservationEvent(r, 1, capacityEvidenceSample(domain.MonitorConditionStorage, warning, 84, r.at)),
					capacityObservationEvent(r, 2, capacityEvidenceSample(domain.MonitorConditionStorage, warning, 85, r.at)),
					capacityTransitionEvent(r, 3, nil, warning),
				))
				assertCondition(t, r, warning, true, 85)
				repo, _ := boundAuxiliary(t, r.f, r.session.ProbeID, 1)
				condition, err := repo.Get(t.Context(), r.monitor, domain.MonitorConditionStorage)
				if err != nil || condition.ConsecutiveState != warning || condition.ConsecutiveCount < 2 {
					t.Fatalf("promotion mirror lost its candidate: %+v %v", condition, err)
				}
				// A duplicate receipt adds nothing.
				duplicate := r.ingest(t, r.batch(
					capacityObservationEvent(r, 1, capacityEvidenceSample(domain.MonitorConditionStorage, warning, 84, r.at)),
					capacityObservationEvent(r, 2, capacityEvidenceSample(domain.MonitorConditionStorage, warning, 85, r.at)),
					capacityTransitionEvent(r, 3, nil, warning),
				))
				if duplicate.DuplicateCount != 3 {
					t.Fatal("duplicate receipt re-applied promotion", duplicate)
				}
				r.ingest(t, r.batch(capacityTransitionEvent(r, 4, &warning, ok)))
				assertCondition(t, r, ok, true, 85)
				// The recovery transition copied the source state; it did not
				// recompute counts or invent delivery state.
				condition, err = repo.Get(t.Context(), r.monitor, domain.MonitorConditionStorage)
				if err != nil || condition.LastNotifiedState != "" || condition.LastNotifiedAt != nil {
					t.Fatal("mirror invented a delivery cursor", condition, err)
				}
			})

			t.Run("UnsupportedConditionTransitionsAreRejectedPermanently", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				warning := domain.ConditionStateWarning
				got := r.ingest(t, r.batch(capacityTransitionEvent(r, 1, nil, warning)))
				if got.AcceptedCount != 0 || len(got.Rejected) != 1 || got.Rejected[0].Code != "condition_evidence_not_found" {
					t.Fatal("transition without raw evidence must be rejected", got)
				}
				// Rejection must not block later valid events.
				r.ingest(t, r.batch(capacityObservationEvent(r, 2, capacityEvidenceSample(domain.MonitorConditionStorage, warning, 84, r.at))))
				assertCondition(t, r, "", false, 84)
				// A fabricated incident reference must correlate with a capacity
				// incident for the same monitor/generation/condition.
				forged := capacityTransitionEvent(r, 3, nil, warning)
				id := "99999999-9999-4999-8999-999999999999"
				forged.Condition.SourceAlertID = &id
				got = r.ingest(t, r.batch(forged))
				if len(got.Rejected) != 1 || got.Rejected[0].Code != "incident_mismatch" {
					t.Fatal("unrelated incident reference must be rejected", got)
				}
			})

			t.Run("SnapshotReplacesAndOmissionClears", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				entry := currentEntry(r, 10, domain.StatusUp)
				entry.Conditions = []domain.ConditionEvidence{capacityEvidence(domain.MonitorConditionStorage, domain.ConditionStateWarning, domain.ConditionStateWarning, 90, r.at, 2)}
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 10, entry), &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
				assertCondition(t, r, domain.ConditionStateWarning, true, 90)
				changed := entry
				changed.Conditions = []domain.ConditionEvidence{capacityEvidence(domain.MonitorConditionStorage, domain.ConditionStateWarning, domain.ConditionStateWarning, 91, r.at, 2)}
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 10, changed), &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("same source sequence changed evaluated state", err)
				}
				assertCondition(t, r, domain.ConditionStateWarning, true, 90)
				// Complete omission clears the projection but keeps history.
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 11, currentEntry(r, 11, domain.StatusUp)), &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
				repo, _ := boundAuxiliary(t, r.f, r.session.ProbeID, 1)
				if _, err := repo.Get(t.Context(), r.monitor, domain.MonitorConditionStorage); !errors.Is(err, ports.ErrNotFound) {
					t.Fatal("omission left a stale capacity condition", err)
				}
			})

			t.Run("OlderEvidenceCannotReplaceNewerState", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				entry := currentEntry(r, 10, domain.StatusUp)
				entry.Conditions = []domain.ConditionEvidence{capacityEvidence(domain.MonitorConditionStorage, domain.ConditionStateWarning, domain.ConditionStateWarning, 90, r.at, 2)}
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 10, entry), &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
				ok := domain.ConditionStateOK
				r.ingest(t, r.batch(
					capacityObservationEvent(r, 1, capacityEvidenceSample(domain.MonitorConditionStorage, ok, 10, r.at.Add(-time.Second))),
					capacityTransitionEvent(r, 2, nil, ok),
				))
				assertCondition(t, r, domain.ConditionStateWarning, true, 90)
				state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 10 || len(state.Conditions) != 1 || state.Conditions[0].Percent == nil || *state.Conditions[0].Percent != 90 {
					t.Fatal("older replay replaced newer evaluated state", state, err)
				}
			})

			t.Run("MigrationRoundTripAndEvidenceGuard", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				r.ingest(t, r.batch(capacityObservationEvent(r, 1, capacityEvidenceSample(domain.MonitorConditionStorage, domain.ConditionStateWarning, 84, r.at))))
				if err := runEngineMigration(t, r.f.db, engine, "068_probe_capacity_state", "down"); err == nil {
					t.Fatal("downgrade must refuse to drop populated capacity evidence")
				}
				assertCondition(t, r, "", false, 84)

				clean := newReplayFixture(t, engine)
				if err := runEngineMigration(t, clean.f.db, engine, "068_probe_capacity_state", "down"); err != nil {
					t.Fatal(err)
				}
				if err := runEngineMigration(t, clean.f.db, engine, "068_probe_capacity_state", "up"); err != nil {
					t.Fatal(err)
				}
				clean.ingest(t, clean.batch(capacityObservationEvent(clean, 1, capacityEvidenceSample(domain.MonitorConditionStorage, domain.ConditionStateWarning, 84, clean.at))))
				assertCondition(t, clean, "", false, 84)
			})
		})
	}
}
