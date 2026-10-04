package domain

import (
	"math"
	"testing"
	"time"
)

const watchdogProbeID = "952157e0-3bf2-4b2a-8f7e-5b27a956f43e"

func watchdogCommitFixture() (ProbeWatchdogState, ProbeWatchdogRecord) {
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	inc := &RegionalIncident{SourceAlertID: "f3459ce0-a9c0-44b2-bcc0-98dc336c3010", ProbeID: watchdogProbeID, Scope: IncidentScopeProbeConnection, SubjectKind: IncidentSubjectWatchdog, Status: AlertStatusFiring, TransitionVersion: 1, ConfigRevision: 1, StartedAt: at, Reason: "Connection lost"}
	r := ProbeWatchdogRecord{ConfigRevision: 1, At: at, Checkpoint: ProbeWatchdogCheckpoint{Armed: true, LossElapsed: 90 * time.Second, PendingLoss: true}, Status: ProbeWatchdogLost, Incident: inc, DeliveryIntents: []DeliveryIntent{{DeliveryID: "69508878-0c55-434d-aef6-d232c8d33310", ProbeID: watchdogProbeID, SourceAlertID: inc.SourceAlertID, SourceTransitionVersion: 1, NotificationID: 10, NotificationVersion: 1, EventKind: DeliveryEventProbeConnection, AvailableAt: at}}}
	return ProbeWatchdogState{Status: ProbeWatchdogUnarmed}, r
}

func TestProbeWatchdogCommitRejectsInvalidSourceEffects(t *testing.T) {
	for name, mutate := range map[string]func(*ProbeWatchdogState, *ProbeWatchdogRecord){
		"negative measured time":     func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Checkpoint.LossElapsed = -1 },
		"unarmed loss":               func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Checkpoint.Armed = false },
		"state overflow":             func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.ExpectedVersion = math.MaxInt64 },
		"invalid diagnostic":         func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Status = "unknown" },
		"healthy with open incident": func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Status = ProbeWatchdogHealthy },
		"resolving unknown incident": func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) {
			r.Incident.Status = AlertStatusResolved
			r.Incident.ResolvedAt = &r.At
		},
		"fake monitor scope":      func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Incident.MonitorID = 123 },
		"fake assignment scope":   func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Incident.AssignmentGeneration = 1 },
		"escalation payload":      func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Incident.EscalationStatus = "pending" },
		"invalid reason encoding": func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Incident.Reason = string([]byte{255}) },
		"stray ack metadata":      func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.Incident.AckActorDisplayName = "operator" },
		"duplicate delivery id": func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) {
			d := r.DeliveryIntents[0]
			d.NotificationID++
			r.DeliveryIntents = append(r.DeliveryIntents, d)
		},
		"duplicate channel": func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) {
			d := r.DeliveryIntents[0]
			d.DeliveryID = r.Incident.SourceAlertID
			r.DeliveryIntents = append(r.DeliveryIntents, d)
		},
		"foreign delivery incident": func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) {
			r.DeliveryIntents[0].SourceAlertID = watchdogProbeID
		},
		"future transition":       func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.DeliveryIntents[0].SourceTransitionVersion++ },
		"foreign channel version": func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) { r.DeliveryIntents[0].NotificationVersion++ },
		"availability event": func(_ *ProbeWatchdogState, r *ProbeWatchdogRecord) {
			r.DeliveryIntents[0].EventKind = DeliveryEventStatusChange
		},
	} {
		t.Run(name, func(t *testing.T) {
			before, r := watchdogCommitFixture()
			mutate(&before, &r)
			if ValidateProbeWatchdogCommit(watchdogProbeID, before, r) == nil {
				t.Fatal("invalid effects accepted")
			}
		})
	}
}

func TestProbeWatchdogRecoveryCannotInventAcknowledgement(t *testing.T) {
	_, r := watchdogCommitFixture()
	before := ProbeWatchdogState{Version: 1, Incident: r.Incident}
	inc := *r.Incident
	inc.Status, inc.TransitionVersion = AlertStatusResolved, 2
	inc.ResolvedAt = &r.At
	inc.AckedAt = &r.At
	inc.AckCommandID = "5813b742-4719-474a-93c3-2688b634d392"
	inc.AckActorDisplayName = "Operator"
	r.ExpectedVersion, r.Status, r.Incident = 1, ProbeWatchdogHealthy, &inc
	r.Checkpoint = ProbeWatchdogCheckpoint{Armed: true}
	r.DeliveryIntents = nil
	if ValidateProbeWatchdogCommit(watchdogProbeID, before, r) == nil {
		t.Fatal("recovery invented an ACK that never committed")
	}
}

func TestProbeWatchdogRecoveryCheckpointCannotPageAgain(t *testing.T) {
	_, r := watchdogCommitFixture()
	r.Incident.Status = AlertStatusResolved
	r.Incident.TransitionVersion = 2
	r.Incident.ResolvedAt = &r.At
	before := ProbeWatchdogState{Version: 2, Incident: r.Incident}
	r.ExpectedVersion, r.Status, r.Incident = 2, ProbeWatchdogHealthy, nil
	r.Checkpoint = ProbeWatchdogCheckpoint{Armed: true}
	r.DeliveryIntents[0].SourceTransitionVersion = 2
	if ValidateProbeWatchdogCommit(watchdogProbeID, before, r) == nil {
		t.Fatal("healthy checkpoint repeated a recovery page")
	}
}

func TestProbeWatchdogCommitPreservesAcknowledgementOnRecovery(t *testing.T) {
	_, r := watchdogCommitFixture()
	prior := *r.Incident
	prior.Status, prior.TransitionVersion = AlertStatusAcked, 2
	ackedAt := r.At
	prior.AckedAt = &ackedAt
	prior.AckCommandID = "5813b742-4719-474a-93c3-2688b634d392"
	prior.AckActorDisplayName = "Operator"
	note := "Working on it"
	prior.AckNote = &note
	before := ProbeWatchdogState{Version: 2, Incident: &prior}
	inc := prior
	inc.Status, inc.TransitionVersion = AlertStatusResolved, 3
	backward := r.At.Add(-time.Minute)
	inc.ResolvedAt = &backward
	r.ExpectedVersion, r.Status, r.Incident, r.At = 2, ProbeWatchdogHealthy, &inc, backward
	r.Checkpoint = ProbeWatchdogCheckpoint{Armed: true}
	r.DeliveryIntents[0].SourceTransitionVersion = 3
	if err := ValidateProbeWatchdogCommit(watchdogProbeID, before, r); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RegionalIncident){
		"command": func(i *RegionalIncident) { i.AckCommandID = watchdogProbeID },
		"actor":   func(i *RegionalIncident) { i.AckActorDisplayName = "another operator" },
		"note":    func(i *RegionalIncident) { i.AckNote = nil },
		"time":    func(i *RegionalIncident) { i.AckedAt = &backward },
		"reopen": func(i *RegionalIncident) {
			i.Status = AlertStatusFiring
			i.ResolvedAt = nil
			i.AckedAt = nil
			i.AckCommandID = ""
			i.AckActorDisplayName = ""
			i.AckNote = nil
		},
		"identity": func(i *RegionalIncident) { i.SourceAlertID = watchdogProbeID },
	} {
		t.Run(name, func(t *testing.T) {
			copy := inc
			mutate(&copy)
			bad := r
			bad.Incident = &copy
			if ValidateProbeWatchdogCommit(watchdogProbeID, before, bad) == nil {
				t.Fatal("acknowledgement changed")
			}
		})
	}
}

func TestProbeWatchdogCheckpointAndResendDoNotChangeLifecycle(t *testing.T) {
	_, r := watchdogCommitFixture()
	before := ProbeWatchdogState{Version: 1, Incident: r.Incident, IncidentSeq: 15}
	r.ExpectedVersion, r.Incident = 1, nil
	if err := ValidateProbeWatchdogCommit(watchdogProbeID, before, r); err != nil {
		t.Fatal(err)
	}
	r.DeliveryIntents = nil
	if err := ValidateProbeWatchdogCommit(watchdogProbeID, before, r); err != nil {
		t.Fatal(err)
	}
	before.Incident.Status = AlertStatusAcked
	before.Incident.AckedAt = &r.At
	before.Incident.AckCommandID = "5813b742-4719-474a-93c3-2688b634d392"
	before.Incident.AckActorDisplayName = "Operator"
	if err := ValidateProbeWatchdogCommit(watchdogProbeID, before, r); err != nil {
		t.Fatal(err)
	}
	_, opening := watchdogCommitFixture()
	r.DeliveryIntents = opening.DeliveryIntents
	if ValidateProbeWatchdogCommit(watchdogProbeID, before, r) == nil {
		t.Fatal("ACKed incident paged again")
	}
}
