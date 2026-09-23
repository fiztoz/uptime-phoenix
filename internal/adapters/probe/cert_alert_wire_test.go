package probe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func wireCertificate(threshold int64, expiry time.Time) *domain.RegionalIncident {
	at := time.Now().UTC()
	copied := expiry
	return &domain.RegionalIncident{
		SourceAlertID: uuid.NewString(), Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectCertificate,
		MonitorID: 7, ProbeID: uuid.NewString(), AssignmentGeneration: 3, Status: domain.AlertStatusFiring, TransitionVersion: 1,
		StartedAt: at.Add(-time.Hour), Reason: "TLS certificate expires in 13 day(s)", ConfigRevision: 4,
		CertificateThreshold: threshold, CertificateNotAfter: &copied,
	}
}

// TestCertificateIncidentWireRoundTrip proves a source certificate incident
// survives encode, durable bytes, and hub-side mapping with its exact immutable
// subject, including a fractional expiry second.
func TestCertificateIncidentWireRoundTrip(t *testing.T) {
	expiry := time.Now().UTC().Add(13 * 24 * time.Hour).Truncate(time.Second).Add(123456789 * time.Nanosecond)
	incident := wireCertificate(14, expiry)
	payload, err := (EdgeTelemetryEncoder{}).EncodeIncident(2, incident.StartedAt, *incident)
	if err != nil {
		t.Fatalf("encode certificate transition: %v", err)
	}
	event, err := decodeTelemetryEvent(payload)
	if err != nil {
		t.Fatalf("decode persisted certificate event: %v", err)
	}
	if event.Kind != "alert.transition" {
		t.Fatalf("certificate transition used kind %q", event.Kind)
	}
	transition, ok := event.Data.(IncidentTransition)
	if !ok {
		t.Fatalf("unexpected decoded payload type %T", event.Data)
	}
	if transition.Subject.Kind != domain.IncidentSubjectCertificate || transition.Subject.CertificateThreshold == nil ||
		*transition.Subject.CertificateThreshold != 14 || transition.Subject.CertificateNotAfter == nil ||
		!time.Time(*transition.Subject.CertificateNotAfter).Equal(expiry) || transition.Subject.ConditionKind != nil {
		t.Fatalf("certificate subject changed on the wire: %+v", transition.Subject)
	}
	if !strings.Contains(string(payload), `"certificate_threshold":14`) {
		t.Fatalf("threshold not emitted explicitly: %s", payload)
	}

	// The hub-side mapper must produce the same domain identity, not a
	// re-labeled availability incident.
	batch := &domain.EdgeReplayBatch{StreamID: uuid.NewString(), FirstSeq: 2, LastSeq: 2, Items: []domain.EdgeReplayItem{{Seq: 2, Kind: "alert.transition", ObservedAt: incident.StartedAt, Payload: payload}}}
	frame, err := buildReplayBatchFrame(batch.StreamID, 2, batch)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeReplayBatch(frame, incident.ProbeID)
	if err != nil || len(decoded.Events) != 1 || decoded.Events[0].Incident == nil {
		t.Fatalf("certificate transition unmapped on replay: %+v %v", decoded, err)
	}
	mapped := decoded.Events[0].Incident
	if mapped.SubjectKind != domain.IncidentSubjectCertificate || mapped.Status != domain.AlertStatusFiring ||
		mapped.TransitionVersion != 1 || mapped.MonitorID != incident.MonitorID || mapped.AssignmentGeneration != 3 ||
		mapped.ConfigRevision != 4 || mapped.CertificateThreshold != 14 || mapped.CertificateNotAfter == nil ||
		!mapped.CertificateNotAfter.Equal(expiry) || !domain.ValidCertificateIncident(mapped) {
		t.Fatalf("mapped certificate incident differs: %+v", mapped)
	}

	// A resolution of the same identity keeps the original subject.
	resolved := *incident
	at := time.Now().UTC()
	resolved.Status, resolved.TransitionVersion, resolved.ResolvedAt = domain.AlertStatusResolved, 2, &at
	resolved.Reason = "TLS certificate was renewed"
	resolvedPayload, err := (EdgeTelemetryEncoder{}).EncodeIncident(3, at, resolved)
	if err != nil {
		t.Fatalf("encode certificate resolution: %v", err)
	}
	resolvedEvent, err := decodeTelemetryEvent(resolvedPayload)
	if err != nil {
		t.Fatal(err)
	}
	value := resolvedEvent.Data.(IncidentTransition)
	if value.Status != domain.AlertStatusResolved || value.ResolvedAt == nil ||
		value.Subject.CertificateThreshold == nil || *value.Subject.CertificateThreshold != 14 ||
		value.Subject.CertificateNotAfter == nil || !time.Time(*value.Subject.CertificateNotAfter).Equal(expiry) {
		t.Fatalf("resolution lost the immutable subject: %+v", value)
	}
}

// TestCertificateIncidentEncoderRejectsMalformedSubjects pins the encoder to the
// V1 certificate identity and keeps malformed auxiliary subjects unemittable.
func TestCertificateIncidentEncoderRejectsMalformedSubjects(t *testing.T) {
	expiry := time.Now().UTC().AddDate(0, 0, 13)
	for name, mutate := range map[string]func(*domain.RegionalIncident){
		"unfixed threshold":           func(i *domain.RegionalIncident) { i.CertificateThreshold = 5 },
		"zero threshold":              func(i *domain.RegionalIncident) { i.CertificateThreshold = 0 },
		"missing expiry":              func(i *domain.RegionalIncident) { i.CertificateNotAfter = nil },
		"availability with an expiry": func(i *domain.RegionalIncident) { i.SubjectKind = domain.IncidentSubjectAvailability },
		"watchdog with a threshold": func(i *domain.RegionalIncident) {
			i.SubjectKind, i.Scope = domain.IncidentSubjectWatchdog, domain.IncidentScopeProbeConnection
			i.MonitorID, i.AssignmentGeneration = 0, 0
		},
		"capacity subject without a kind": func(i *domain.RegionalIncident) {
			i.SubjectKind, i.ConditionKind = domain.IncidentSubjectCapacity, ""
			i.CertificateThreshold, i.CertificateNotAfter = 0, nil
		},
		"escalation on certificate": func(i *domain.RegionalIncident) {
			step, next := int64(1), time.Now().UTC()
			i.EscalationPolicyID, i.EscalationPolicyVersion, i.EscalationStatus, i.EscalationNextStep, i.EscalationNextRunAt = 3, 1, domain.AlertStatusFiring, &step, &next
		},
		"aggregate scope":                       func(i *domain.RegionalIncident) { i.Scope = domain.IncidentScopeAggregate },
		"zero monitor":                          func(i *domain.RegionalIncident) { i.MonitorID = 0 },
		"zero generation":                       func(i *domain.RegionalIncident) { i.AssignmentGeneration = 0 },
		"certificate in probe connection scope": func(i *domain.RegionalIncident) { i.Scope = domain.IncidentScopeProbeConnection },
	} {
		t.Run(name, func(t *testing.T) {
			incident := wireCertificate(14, expiry)
			mutate(incident)
			if _, err := (EdgeTelemetryEncoder{}).EncodeIncident(2, time.Now().UTC(), *incident); err == nil {
				t.Fatalf("encoder published a malformed subject: %+v", incident)
			}
		})
	}
}

// TestCertificateIncidentDecoderRequiresFullSubject proves a wire certificate
// subject cannot arrive with a partial identity, so the hub can never store an
// incident it cannot compare against a later transition.
func TestCertificateIncidentDecoderRequiresFullSubject(t *testing.T) {
	expiry := time.Now().UTC().AddDate(0, 0, 13).Truncate(time.Millisecond)
	payload, err := (EdgeTelemetryEncoder{}).EncodeIncident(2, expiry, *wireCertificate(14, expiry))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(subject map[string]any) { subject["certificate_threshold"] = nil },
		func(subject map[string]any) { subject["certificate_not_after"] = nil },
		func(subject map[string]any) { subject["certificate_threshold"] = float64(5) },
		func(subject map[string]any) { subject["condition_kind"] = "storage" },
	} {
		event := map[string]any{}
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		data := event["data"].(map[string]any)
		subject := data["subject"].(map[string]any)
		mutate(subject)
		mutated, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeTelemetryEvent(mutated); err == nil {
			t.Fatalf("decoder accepted a malformed certificate subject: %s", mutated)
		}
	}
}
