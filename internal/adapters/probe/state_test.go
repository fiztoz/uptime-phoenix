package probe

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStatePreservesSourcePromotionWithoutReplayEvaluation(t *testing.T) {
	for _, test := range []struct {
		fixture, observed, candidate, effective string
		count                                   int64
	}{
		{"first-warning", "warning", "warning", "", 1},
		{"first-ok", "ok", "ok", "ok", 2},
		{"warning-after-ok", "warning", "warning", "ok", 1},
		{"confirmed-warning", "warning", "warning", "warning", 2},
		{"first-recovery", "ok", "ok", "warning", 1},
		{"confirmed-recovery", "ok", "ok", "ok", 2},
		{"hysteresis", "ok", "warning", "warning", 3},
		{"first-error", "error", "error", "", 1},
		{"stale", "warning", "warning", "warning", 2},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			snapshot, err := DecodeStateSnapshot(readFixture(t, "valid", "state-snapshot-"+test.fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			state := snapshot.States[0]
			condition := state.Conditions[0]
			effective := ""
			if condition.EffectiveState != nil {
				effective = *condition.EffectiveState
			}
			if condition.ObservedState != test.observed || condition.ConsecutiveState != test.candidate || effective != test.effective || condition.ConsecutiveCount != test.count {
				t.Fatalf("source promotion changed: %+v", condition)
			}
			if state.Status != "UP" || state.DownCount != 0 || state.ActiveSourceAlertID != nil {
				t.Fatalf("capacity changed availability/incident evidence: %+v", state)
			}
			if test.fixture == "stale" && !time.Time(snapshot.CreatedAt).After(time.Time(condition.StaleAfter)) {
				t.Fatal("stale fixture does not contain expired evidence")
			}
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			roundtrip, err := DecodeStateSnapshot(encoded)
			if err != nil || !reflect.DeepEqual(roundtrip, snapshot) {
				t.Fatalf("source state changed on roundtrip: %v", err)
			}
			if strings.Contains(string(encoded), `"state":`) || strings.Contains(string(encoded), `"MonitorID"`) || strings.Contains(string(encoded), `"last_notified`) {
				t.Fatalf("snapshot leaked another wire dialect or delivery cursor: %s", encoded)
			}
		})
	}
}

func TestStatePreservesSequencePrecisionAndSameSecondEvidence(t *testing.T) {
	snapshot, err := DecodeStateSnapshot(readFixture(t, "valid", "state-snapshot-max-sequence.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, last := snapshot.States[0], snapshot.States[1]
	if snapshot.LastCreatedSeq != math.MaxInt64 || first.LastObservationSeq != math.MaxInt64-1 || last.LastObservationSeq != math.MaxInt64 || first.ObservedAt != last.ObservedAt {
		t.Fatalf("sequence/timestamp evidence changed: %+v", snapshot)
	}
	if first.Status != "PENDING" || first.DownCount != 1 || last.Status != "DOWN" || last.DownCount != 2 || last.ActiveSourceAlertID == nil || last.TLS == nil || last.TLS.DaysRemaining != -1 {
		t.Fatal("snapshot changed retries, incident identity or expired certificate")
	}
}

func TestStateRequiredAndNullableFields(t *testing.T) {
	fixture := readFixture(t, "valid", "state-snapshot-first-warning.json")
	for _, level := range []struct {
		name     string
		fields   []string
		nullable map[string]bool
		selectAt func(map[string]any) map[string]any
	}{
		{"snapshot", []string{"stream_id", "config_revision", "created_at", "last_created_seq", "states"}, nil, func(v map[string]any) map[string]any { return v }},
		{"monitor", []string{"monitor_id", "assignment_generation", "last_observation_seq", "observed_at", "status", "down_count", "ping", "message", "conditions", "tls", "active_source_alert_id"}, map[string]bool{"tls": true, "active_source_alert_id": true}, firstSnapshotState},
		{"condition", []string{"kind", "observed_state", "effective_state", "consecutive_state", "consecutive_count", "last_success_at", "message", "used", "limit", "percent", "threshold", "unit", "resource", "scope", "source", "observed_at", "stale_after"}, map[string]bool{"effective_state": true, "used": true, "limit": true, "percent": true, "threshold": true}, firstSnapshotCondition},
	} {
		for _, name := range level.fields {
			t.Run(level.name+"/"+name, func(t *testing.T) {
				for _, absent := range []bool{true, false} {
					data := mutateJSON(t, fixture, func(v map[string]any) {
						object := level.selectAt(v)
						if absent {
							delete(object, name)
						} else {
							object[name] = nil
						}
					})
					_, err := DecodeStateSnapshot(data)
					wantValid := !absent && level.nullable[name]
					if (err == nil) != wantValid {
						t.Fatalf("absent=%v wantValid=%v: %v", absent, wantValid, err)
					}
				}
			})
		}
	}
}

func TestStateBoundsAndClockRollback(t *testing.T) {
	fixture := readFixture(t, "valid", "state-snapshot-first-warning.json")
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{"unknown optional", func(v map[string]any) { firstSnapshotCondition(v)["future"] = []any{false, nil, 1} }, true},
		{"clock rollback", func(v map[string]any) { v["created_at"] = "2026-09-14T08:00:00Z" }, true},
		{"prior success after error wall clock", func(v map[string]any) {
			c := firstSnapshotCondition(v)
			c["observed_state"], c["consecutive_state"] = "error", "error"
			c["last_success_at"] = "2026-09-14T09:05:00Z"
		}, true},
		{"zero measurement", func(v map[string]any) { firstSnapshotCondition(v)["used"] = 0 }, true},
		{"negative measurement", func(v map[string]any) { firstSnapshotCondition(v)["used"] = -1 }, false},
		{"too early stale deadline", func(v map[string]any) { firstSnapshotCondition(v)["stale_after"] = "2026-09-14T08:00:00Z" }, false},
		{"wrong timestamp zone", func(v map[string]any) { firstSnapshotState(v)["observed_at"] = "2026-09-14T16:00:00+07:00" }, false},
		{"message at bound", func(v map[string]any) { firstSnapshotState(v)["message"] = strings.Repeat("x", MaxMessageBytes) }, true},
		{"UTF8 bytes over bound", func(v map[string]any) { firstSnapshotState(v)["message"] = strings.Repeat("é", MaxMessageBytes/2+1) }, false},
		{"metadata over bound", func(v map[string]any) { firstSnapshotCondition(v)["source"] = strings.Repeat("x", MaxMetadataBytes+1) }, false},
		{"state byte bound", func(v map[string]any) { firstSnapshotState(v)["future"] = strings.Repeat("x", MaxEventBytes) }, false},
		{"condition count bound", func(v map[string]any) { firstSnapshotState(v)["conditions"] = []any{nil, nil, nil} }, false},
		{"zero generation", func(v map[string]any) { firstSnapshotState(v)["assignment_generation"] = "0" }, false},
		{"negative latency", func(v map[string]any) { firstSnapshotState(v)["ping"] = -1 }, false},
		{"fractional count", func(v map[string]any) { firstSnapshotCondition(v)["consecutive_count"] = 1.5 }, false},
		{"zero count", func(v map[string]any) { firstSnapshotCondition(v)["consecutive_count"] = 0 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := mutateJSON(t, fixture, test.mutate)
			_, err := DecodeStateSnapshot(data)
			if (err == nil) != test.valid {
				t.Fatalf("want valid=%v: %v", test.valid, err)
			}
		})
	}
	for name, data := range map[string][]byte{
		"total bytes":           []byte(strings.Repeat(" ", MaxStateSnapshotBytes+1)),
		"duplicate escaped key": []byte(strings.Replace(string(fixture), `"monitor_id": 42`, `"monitor_id": 42, "monitor_\u0069d": 43`, 1)),
		"deep optional":         []byte(strings.Replace(string(fixture), `"tls": null`, `"tls": null, "future": `+strings.Repeat("[", MaxJSONDepth)+"0"+strings.Repeat("]", MaxJSONDepth), 1)),
		"invalid UTF8":          append([]byte{0xff}, fixture...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeStateSnapshot(data); err == nil {
				t.Fatal("unbounded/malformed snapshot accepted")
			}
		})
	}
}

func TestSnapshotAssignmentLimit(t *testing.T) {
	snapshot, err := DecodeStateSnapshot(readFixture(t, "valid", "state-snapshot-max-sequence.json"))
	if err != nil {
		t.Fatal(err)
	}
	source := snapshot.States[0]
	snapshot.States = make([]MonitorState, MaxSnapshotStates+1)
	for i := range snapshot.States {
		snapshot.States[i] = source
		snapshot.States[i].MonitorID = int64(i + 1)
		snapshot.States[i].LastObservationSeq = Decimal(i + 1)
	}
	for _, count := range []int{MaxSnapshotStates + 1, MaxSnapshotStates} {
		snapshot.States = snapshot.States[:count]
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeStateSnapshot(data)
		if count == MaxSnapshotStates && (err != nil || len(decoded.States) != count) || count > MaxSnapshotStates && err == nil {
			t.Fatalf("assignment bound %d: %v", count, err)
		}
	}
}

func TestStateAvailabilityPreservesRawPendingAndOpenIncidents(t *testing.T) {
	fixture := readFixture(t, "valid", "state-snapshot-first-warning.json")
	for _, test := range []struct {
		status string
		count  int
		valid  bool
	}{
		{"PENDING", 0, true}, {"PENDING", 1, true}, {"MAINTENANCE", 0, true},
		{"DOWN", 2, true}, {"DOWN", 0, false}, {"UP", 1, false},
		{"MAINTENANCE", 1, false}, {"PENDING", -1, false}, {"unknown", 0, false},
	} {
		data := mutateJSON(t, fixture, func(v map[string]any) {
			s := firstSnapshotState(v)
			s["status"], s["down_count"] = test.status, test.count
			s["active_source_alert_id"] = "7c73a9c5-77ad-4dc0-926d-123bbab94540"
		})
		decoded, err := DecodeStateSnapshot(data)
		if (err == nil) != test.valid {
			t.Fatalf("status %s count %d: %v", test.status, test.count, err)
		}
		if err == nil && decoded.States[0].ActiveSourceAlertID == nil {
			t.Fatal("decoder inferred an incident recovery")
		}
	}
}

func firstSnapshotState(v map[string]any) map[string]any {
	return v["states"].([]any)[0].(map[string]any)
}

func firstSnapshotCondition(v map[string]any) map[string]any {
	return firstSnapshotState(v)["conditions"].([]any)[0].(map[string]any)
}
