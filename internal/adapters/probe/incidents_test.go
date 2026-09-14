package probe

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestMixedTelemetryPreservesIndependentEvidenceAndLifecycle(t *testing.T) {
	_, batch, err := DecodeTelemetryBatch(readFixture(t, "valid", "batch-mixed-lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 10 || batch.FirstSeq != 101 || batch.LastSeq != 110 {
		t.Fatalf("mixed sequence changed: %+v", batch)
	}
	observation := batch.Events[0].Data.(Observation)
	firing := batch.Events[1].Data.(IncidentTransition)
	retry := batch.Events[2].Data.(DeliveryResult)
	acked := batch.Events[3].Data.(IncidentTransition)
	sent := batch.Events[4].Data.(DeliveryResult)
	resolved := batch.Events[5].Data.(IncidentTransition)
	capacity := batch.Events[6].Data.(IncidentTransition)
	condition := batch.Events[7].Data.(ConditionTransition)
	certificate := batch.Events[8].Data.(IncidentTransition)
	watchdog := batch.Events[9].Data.(IncidentTransition)
	if observation.MonitorID != 43 || observation.Status != "UP" || firing.MonitorID == nil || *firing.MonitorID != 42 || firing.Status != "firing" {
		t.Fatal("another region/monitor's UP changed incident evidence")
	}
	if firing.TransitionVersion != 1 || acked.TransitionVersion != 2 || resolved.TransitionVersion != 3 || firing.SourceAlertID != resolved.SourceAlertID || acked.Acknowledgement == nil || resolved.Acknowledgement == nil || resolved.ResolvedAt == nil {
		t.Fatal("source lifecycle or acknowledgement identity changed")
	}
	if firing.Escalation == nil || firing.Escalation.Status != "pending" || acked.Escalation == nil || acked.Escalation.Status != "canceled" || acked.Escalation.NextStep != nil {
		t.Fatal("escalation progress changed")
	}
	if retry.DeliveryID != sent.DeliveryID || retry.Attempt != 1 || sent.Attempt != 2 || retry.Status != "retrying" || sent.Status != "sent" || sent.SourceTransitionVersion != 1 {
		t.Fatal("delivery attempts were collapsed or rebound to a newer incident transition")
	}
	if condition.SourceAlertID == nil || *condition.SourceAlertID != capacity.SourceAlertID || condition.State != "warning" || condition.PreviousState != nil || capacity.Subject.Kind != "capacity" {
		t.Fatal("condition transition was re-promoted or detached from its incident")
	}
	if certificate.Subject.CertificateThreshold == nil || *certificate.Subject.CertificateThreshold != 7 || certificate.Subject.CertificateNotAfter == nil || watchdog.Scope != "probe_connection" || watchdog.MonitorID != nil {
		t.Fatal("certificate or watchdog identity changed")
	}
	for _, event := range batch.Events {
		if event.ObservedAt != batch.Events[0].ObservedAt {
			t.Fatal("fixture must demonstrate same-second mixed-event ordering")
		}
	}
}

func TestLifecycleWireRoundTripsOnlyExplicitFields(t *testing.T) {
	for _, name := range []string{"batch-alert-availability.json", "batch-alert-acked.json", "batch-alert-resolved-acked.json", "batch-alert-capacity.json", "batch-alert-certificate.json", "batch-watchdog-firing.json", "batch-delivery-superseded.json", "batch-condition-recovery.json", "batch-mixed-lifecycle.json"} {
		t.Run(name, func(t *testing.T) {
			fixture := mutateJSON(t, readFixture(t, "valid", name), func(frame map[string]any) {
				observationData(frame)["future_optional"] = map[string]any{"metadata": true}
			})
			envelope, batch, err := DecodeTelemetryBatch(fixture)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "future_optional") || strings.Contains(string(encoded), `"SourceAlertID"`) || strings.Contains(string(encoded), "ack_token") {
				t.Fatalf("typed event leaked non-contract data: %s", encoded)
			}
			envelope.Payload = encoded
			_, again, err := DecodeTelemetryBatch(marshalStateTest(t, envelope))
			if err != nil || !reflect.DeepEqual(batch, again) {
				t.Fatalf("typed event changed on wire roundtrip: %v", err)
			}
		})
	}
}

func TestLifecycleRequiredAndNullFields(t *testing.T) {
	for _, test := range []struct {
		name, fixture string
		nested        string
		nulls         map[string]bool
	}{
		{"incident", "batch-alert-availability.json", "", map[string]bool{"resolved_at": true, "acked_at": true, "acknowledgement": true, "escalation": true}},
		{"watchdog", "batch-watchdog-firing.json", "", map[string]bool{"monitor_id": true, "assignment_generation": true, "resolved_at": true, "acked_at": true, "acknowledgement": true, "escalation": true}},
		{"subject", "batch-alert-availability.json", "subject", map[string]bool{"condition_kind": true, "certificate_threshold": true, "certificate_not_after": true}},
		{"certificate subject", "batch-alert-certificate.json", "subject", map[string]bool{"condition_kind": true}},
		{"acknowledgement", "batch-alert-acked.json", "acknowledgement", map[string]bool{"note": true}},
		{"escalation", "batch-alert-availability.json", "escalation", nil},
		{"delivery", "batch-delivery-sent.json", "", map[string]bool{"error_code": true}},
		{"condition", "batch-condition-recovery.json", "", map[string]bool{"previous_state": true, "source_alert_id": true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := readFixture(t, "valid", test.fixture)
			selectFields := func(v map[string]any) map[string]any {
				fields := observationData(v)
				if test.nested != "" {
					fields = fields[test.nested].(map[string]any)
				}
				return fields
			}
			var names []string
			_ = mutateJSON(t, fixture, func(v map[string]any) {
				for name := range selectFields(v) {
					names = append(names, name)
				}
			})
			for _, name := range names {
				for _, absent := range []bool{true, false} {
					data := mutateJSON(t, fixture, func(v map[string]any) {
						fields := selectFields(v)
						if absent {
							delete(fields, name)
						} else {
							fields[name] = nil
						}
					})
					_, _, err := DecodeTelemetryBatch(data)
					wantValid := !absent && test.nulls[name]
					if (err == nil) != wantValid {
						t.Fatalf("%s absent=%v wantValid=%v: %v", name, absent, wantValid, err)
					}
				}
			}
		})
	}
}

func TestIncidentClockRollbackAndMaximumVersions(t *testing.T) {
	fixture := readFixture(t, "valid", "batch-alert-resolved-acked.json")
	data := mutateJSON(t, fixture, func(v map[string]any) {
		incident := observationData(v)
		incident["transition_version"], incident["config_revision"] = "9223372036854775807", "9223372036854775807"
		incident["acked_at"], incident["resolved_at"] = "2026-09-14T08:59:00Z", "2026-09-14T08:58:00Z"
	})
	_, batch, err := DecodeTelemetryBatch(data)
	if err != nil {
		t.Fatal(err)
	}
	incident := batch.Events[0].Data.(IncidentTransition)
	if incident.TransitionVersion != math.MaxInt64 || incident.ConfigRevision != math.MaxInt64 || incident.Acknowledgement == nil || incident.Status != "resolved" {
		t.Fatal("clock rollback changed source ordering or acknowledgement")
	}
}

func TestIncidentSubjectAndLifecycleConflicts(t *testing.T) {
	for _, test := range []struct {
		name, fixture string
		mutate        func(map[string]any)
	}{
		{"watchdog generation", "batch-watchdog-firing.json", func(d map[string]any) { d["assignment_generation"] = "1" }},
		{"monitor with watchdog subject", "batch-alert-availability.json", func(d map[string]any) { d["subject"].(map[string]any)["kind"] = "watchdog" }},
		{"capacity wrong kind", "batch-alert-capacity.json", func(d map[string]any) { d["subject"].(map[string]any)["condition_kind"] = "cpu" }},
		{"availability auxiliary fields", "batch-alert-availability.json", func(d map[string]any) { d["subject"].(map[string]any)["certificate_threshold"] = 7 }},
		{"unknown subject", "batch-alert-availability.json", func(d map[string]any) { d["subject"].(map[string]any)["kind"] = "aggregate" }},
		{"partial ack pair", "batch-alert-resolved-acked.json", func(d map[string]any) { d["acked_at"] = nil }},
		{"acked and resolved", "batch-alert-acked.json", func(d map[string]any) { d["resolved_at"] = "2026-09-14T09:05:00Z" }},
		{"blank actor", "batch-alert-acked.json", func(d map[string]any) { d["acknowledgement"].(map[string]any)["actor_display_name"] = " \t" }},
		{"invalid command", "batch-alert-acked.json", func(d map[string]any) { d["acknowledgement"].(map[string]any)["command_id"] = "phx_secret" }},
		{"unbounded actor", "batch-alert-acked.json", func(d map[string]any) {
			d["acknowledgement"].(map[string]any)["actor_display_name"] = strings.Repeat("é", MaxMetadataBytes/2+1)
		}},
		{"unbounded note", "batch-alert-acked.json", func(d map[string]any) {
			d["acknowledgement"].(map[string]any)["note"] = strings.Repeat("x", MaxMessageBytes+1)
		}},
		{"invalid escalation state", "batch-alert-availability.json", func(d map[string]any) { d["escalation"].(map[string]any)["status"] = "running" }},
		{"negative policy ID", "batch-alert-availability.json", func(d map[string]any) { d["escalation"].(map[string]any)["policy_id"] = -1 }},
		{"overflow next step", "batch-alert-availability.json", func(d map[string]any) {
			d["escalation"].(map[string]any)["next_step"] = json.Number("9223372036854775808")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := mutateJSON(t, readFixture(t, "valid", test.fixture), func(v map[string]any) { test.mutate(observationData(v)) })
			if _, _, err := DecodeTelemetryBatch(data); err == nil {
				t.Fatal("conflicting incident accepted")
			}
		})
	}
	for _, threshold := range []int{30, 14, 7} {
		data := mutateJSON(t, readFixture(t, "valid", "batch-alert-certificate.json"), func(v map[string]any) {
			observationData(v)["subject"].(map[string]any)["certificate_threshold"] = threshold
		})
		if _, _, err := DecodeTelemetryBatch(data); err != nil {
			t.Fatalf("existing certificate threshold %d rejected: %v", threshold, err)
		}
	}
}

func TestDeliveryOutcomeContracts(t *testing.T) {
	fixture := readFixture(t, "valid", "batch-delivery-sent.json")
	for _, test := range []struct {
		status  string
		attempt int64
		code    any
		valid   bool
	}{
		{"sent", 1, nil, true}, {"retrying", 1, "provider_timeout", true},
		{"failed", math.MaxInt64, "invalid_configuration", true}, {"superseded", 0, nil, true},
		{"superseded", 2, nil, true}, {"superseded", -1, nil, false},
		{"retrying", 0, "provider_timeout", false}, {"failed", 1, "", false},
		{"failed", 1, strings.Repeat("x", MaxErrorCodeBytes+1), false},
		{"pending", 1, nil, false}, {"superseded", 0, "outdated", false},
	} {
		data := mutateJSON(t, fixture, func(v map[string]any) {
			d := observationData(v)
			d["status"], d["attempt"], d["error_code"] = test.status, test.attempt, test.code
		})
		_, _, err := DecodeTelemetryBatch(data)
		if (err == nil) != test.valid {
			t.Fatalf("status=%s attempt=%d valid=%v: %v", test.status, test.attempt, test.valid, err)
		}
	}
	for _, eventKind := range []string{"status_change", "certificate_expiry", "capacity_condition", "probe_connection", "incident_summary"} {
		data := mutateJSON(t, fixture, func(v map[string]any) { observationData(v)["event_kind"] = eventKind })
		if _, _, err := DecodeTelemetryBatch(data); err != nil {
			t.Fatalf("event kind %s rejected: %v", eventKind, err)
		}
	}
}

func TestEveryTelemetryKindKeepsBatchAndEventBounds(t *testing.T) {
	for _, fixture := range []string{"batch-alert-availability.json", "batch-watchdog-firing.json", "batch-delivery-sent.json", "batch-condition-recovery.json"} {
		t.Run(fixture, func(t *testing.T) {
			original := readFixture(t, "valid", fixture)
			for _, test := range []struct {
				name   string
				mutate func(map[string]any)
				valid  bool
			}{
				{"max events", func(v map[string]any) { setEventCount(v, MaxBatchEvents) }, true},
				{"excess events", func(v map[string]any) { setEventCount(v, MaxBatchEvents+1) }, false},
				{"event size", func(v map[string]any) { observationData(v)["optional"] = strings.Repeat("x", MaxEventBytes) }, false},
				{"batch size", func(v map[string]any) { v["optional"] = strings.Repeat("x", MaxBatchBytes) }, false},
				{"null event data", func(v map[string]any) {
					v["payload"].(map[string]any)["events"].([]any)[0].(map[string]any)["data"] = nil
				}, false},
				{"maximum sequence", func(v map[string]any) {
					p := v["payload"].(map[string]any)
					p["first_seq"], p["last_seq"] = "9223372036854775807", "9223372036854775807"
					p["events"].([]any)[0].(map[string]any)["seq"] = "9223372036854775807"
				}, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					data := mutateJSON(t, original, test.mutate)
					_, _, err := DecodeTelemetryBatch(data)
					if (err == nil) != test.valid {
						t.Fatalf("valid=%v: %v", test.valid, err)
					}
				})
			}
		})
	}
}

func TestMalformedSuffixRejectsWholeMixedBatch(t *testing.T) {
	fixture := readFixture(t, "valid", "batch-mixed-lifecycle.json")
	for _, bad := range []string{"unsupported.event", "observation"} {
		data := mutateJSON(t, fixture, func(v map[string]any) {
			events := v["payload"].(map[string]any)["events"].([]any)
			events[len(events)-1].(map[string]any)["kind"] = bad
		})
		_, batch, err := DecodeTelemetryBatch(data)
		if err == nil || batch.Events != nil {
			t.Fatalf("malformed suffix exposed a usable partial batch: %v", err)
		}
	}
}
