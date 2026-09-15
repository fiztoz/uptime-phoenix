package probe

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAPIViewsPreserveLocalIdentityAndMaxGeneration(t *testing.T) {
	view, err := DecodeProbeView(readFixture(t, "valid", "http-probe-view-local.json"))
	if err != nil || view.ID != "local" || view.Kind != "local" {
		t.Fatalf("local probe view: %v", err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "phx_probe_") || strings.Contains(string(encoded), "PasswordHash") {
		t.Fatalf("probe view leaked a secret: %s", encoded)
	}
	result, err := DecodeAssignmentReplacementResult(readFixture(t, "valid", "http-assignment-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Assignments[1].Generation != math.MaxInt64 || result.Assignments[1].DesiredConfigRevision != math.MaxInt64 {
		t.Fatal("assignment generation lost signed-64-bit precision")
	}
	health, err := DecodeHealthView(readFixture(t, "valid", "http-health-view.json"))
	if err != nil || health.Status != "down" || health.ProbeCounts.Assigned != 2 || len(health.Regions) != 2 {
		t.Fatalf("health view: %v %+v", err, health)
	}
}

func TestBrowserEventsRejectUnknownAndSecrets(t *testing.T) {
	event, err := DecodeBrowserEvent(readFixture(t, "valid", "http-browser-monitor-health.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := event.Payload.(HealthView); !ok {
		t.Fatalf("monitor.health payload type %T", event.Payload)
	}
	if _, err := DecodeBrowserEvent(readFixture(t, "invalid", "http-browser-unknown-event.json")); err == nil {
		t.Fatal("unknown browser event accepted")
	}
	if view, err := DecodeProbeView(readFixture(t, "invalid", "http-probe-view-secret.json")); err == nil || !reflect.DeepEqual(view, ProbeView{}) {
		t.Fatal("secret-bearing probe view accepted")
	}
}

func TestAssignmentReplacementRejectsEmptyAndDuplicates(t *testing.T) {
	if req, err := DecodeAssignmentReplacementRequest(readFixture(t, "invalid", "http-assignment-replace-empty.json")); err == nil || !reflect.DeepEqual(req, AssignmentReplacementRequest{}) {
		t.Fatal("empty assignment list accepted")
	}
	if _, err := DecodeAssignmentReplacementRequest(readFixture(t, "invalid", "http-assignment-replace-duplicate.json")); err == nil {
		t.Fatal("duplicate probe ids accepted")
	}
	req, err := DecodeAssignmentReplacementRequest(readFixture(t, "valid", "http-assignment-replace.json"))
	if err != nil || len(req.ProbeIDs) != 2 || req.Bindings[0].BindingKey != "docker-local" {
		t.Fatalf("valid replacement: %v", err)
	}
}

func TestRegionalHeartbeatKeepsMessageAndLowercaseStatus(t *testing.T) {
	row, err := DecodeRegionalHeartbeat(readFixture(t, "valid", "http-regional-heartbeat.json"))
	if err != nil || row.Message != "OK" || row.Status != "up" {
		t.Fatalf("regional heartbeat: %v %+v", err, row)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"msg"`) || strings.Contains(string(encoded), `"Msg"`) {
		t.Fatalf("regional heartbeat renamed message: %s", encoded)
	}
	if _, err := DecodeRegionalHeartbeat(readFixture(t, "invalid", "http-regional-heartbeat-uppercase.json")); err == nil {
		t.Fatal("uppercase probe status accepted on HTTP")
	}
}

func TestBaselineFixturesPreserveExistingFieldNames(t *testing.T) {
	required := map[string][]string{
		"heartbeat.json":             {"id", "monitor_id", "status", "ping", "message", "time", "important"},
		"monitor.json":               {"accepted_statuscodes", "message"},
		"alert.json":                 {"id", "monitor_id", "status", "message"},
		"maintenance.json":           {"monitor_ids", "timezone", "duration"},
		"notification-template.json": {"title_template", "body_template", "provider"},
		"browser-heartbeat.json":     {"monitor_id", "status", "time", "ping", "msg"},
		"status-page-access.json":    {"access_code"},
	}
	// monitor.json uses accepted_statuscodes; it has no message field.
	required["monitor.json"] = []string{"accepted_statuscodes", "type", "timeout"}
	for name, keys := range required {
		raw, err := os.ReadFile(filepath.Join("testdata", "v1", "baseline", name))
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if _, ok := object[key]; !ok {
				t.Fatalf("%s missing %s", name, key)
			}
		}
		if name == "heartbeat.json" {
			if _, ok := object["msg"]; ok {
				t.Fatal("HTTP heartbeat fixture used browser msg")
			}
		}
		if name == "browser-heartbeat.json" {
			if _, ok := object["message"]; ok {
				t.Fatal("browser heartbeat fixture used HTTP message")
			}
		}
	}
}
