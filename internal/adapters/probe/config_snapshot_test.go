package probe

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestConfigSnapshotPreservesResolvedBehavior(t *testing.T) {
	snapshot, err := DecodeConfigSnapshot(readFixture(t, "valid", "config-snapshot-complete.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Assignments) != 2 || snapshot.Assignments[0].Monitor.Timeout != 2.5 || snapshot.Assignments[0].Monitor.RetryInterval != 0 || snapshot.Assignments[0].Monitor.ResendInterval != 5 || snapshot.Assignments[0].NotificationLinks[0].IncludeTarget || !snapshot.Assignments[1].NotificationLinks[0].IncludeTarget {
		t.Fatal("monitor timing or per-link visibility changed")
	}
	if snapshot.EscalationPolicies[0].Enabled || snapshot.EscalationPolicies[0].Steps[0].DelaySeconds != 0 || snapshot.EscalationPolicies[0].Steps[1].DelaySeconds != 604800 || snapshot.MaintenanceWindows[0].Duration != 15 || snapshot.MaintenanceWindows[0].Timezone != "Asia/Bangkok" {
		t.Fatal("disabled policy, minute delays, or maintenance semantics changed")
	}
	if snapshot.ProxyBindings[0].Password != "fixture-secret" || snapshot.Assignments[1].ResourceBindings[0].BindingKey != "docker-local" {
		t.Fatal("confidential proxy config or declared binding lost")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := DecodeConfigSnapshot(encoded)
	if err != nil || !reflect.DeepEqual(snapshot, roundTrip) {
		t.Fatalf("typed round trip changed config: %v", err)
	}
	maximum, err := DecodeConfigSnapshot(readFixture(t, "valid", "config-snapshot-max-versions.json"))
	if err != nil || maximum.Revision != math.MaxInt64 || maximum.Assignments[0].Generation != math.MaxInt64 || maximum.NotificationChannels[0].Version != math.MaxInt64 {
		t.Fatalf("maximum version lost precision: %v", err)
	}
}

func TestConfigSnapshotEverySchemaMemberIsRequired(t *testing.T) {
	fixture := readFixture(t, "valid", "config-snapshot-complete.json")
	var value map[string]any
	if err := json.Unmarshal(fixture, &value); err != nil {
		t.Fatal(err)
	}
	var walk func(any, []any)
	walk = func(value any, path []any) {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				childPath := append(append([]any(nil), path...), key)
				for _, absent := range []bool{true, false} {
					// Nullability is further constrained by strategy/reference semantics.
					if !absent && (key == "proxy_binding_key" || key == "escalation_policy_id" || key == "template_id" || key == "start_date" || key == "end_date") {
						continue
					}
					frame := mutateJSON(t, fixture, func(root map[string]any) {
						parent := configTestNode(root, path).(map[string]any)
						if absent {
							delete(parent, key)
						} else {
							parent[key] = nil
						}
					})
					if snapshot, err := DecodeConfigSnapshot(frame); err == nil || !reflect.DeepEqual(snapshot, ConfigSnapshot{}) {
						t.Fatalf("accepted missing/null schema member %v absent=%v", childPath, absent)
					}
				}
				// These are explicit extension objects, with existing runtime validators.
				if key != "config" {
					walk(child, childPath)
				}
			}
		case []any:
			for index, child := range node {
				walk(child, append(append([]any(nil), path...), index))
			}
		}
	}
	walk(value, nil)
}

func TestConfigSnapshotWhitelistsAuthorityFields(t *testing.T) {
	fixture := mutateJSON(t, readFixture(t, "valid", "config-snapshot-http.json"), func(root map[string]any) {
		root["user_id"] = 123
		assignment := root["assignments"].([]any)[0].(map[string]any)
		assignment["is_admin"] = true
		assignment["monitor"].(map[string]any)["push_token"] = "must-not-travel"
		root["notification_channels"].([]any)[0].(map[string]any)["user_id"] = 123
	})
	snapshot, err := DecodeConfigSnapshot(fixture)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"user_id", "is_admin", "push_token", "must-not-travel", "AcceptedStatusCodes"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("unrecognized authority field survived: %s", forbidden)
		}
	}
	if !strings.Contains(string(encoded), `"accepted_statuscodes"`) {
		t.Fatal("existing HTTP status code spelling changed")
	}
}

func TestConfigSnapshotCollectionAndObjectLimits(t *testing.T) {
	fixture := readFixture(t, "valid", "config-snapshot-http.json")
	for _, field := range []string{"notification_channels", "notification_templates", "maintenance_windows", "proxy_bindings", "escalation_policies"} {
		t.Run("duplicate "+field, func(t *testing.T) {
			frame := mutateJSON(t, fixture, func(root map[string]any) {
				items := root[field].([]any)
				root[field] = append(items, items[0])
			})
			if _, err := DecodeConfigSnapshot(frame); err == nil {
				t.Fatal("accepted duplicate dependency")
			}
		})
	}
	for _, test := range []struct {
		path  []any
		key   string
		value any
	}{
		{nil, "future", strings.Repeat("x", MaxConfigSnapshotBytes)},
		{[]any{"assignments", 0}, "future", strings.Repeat("x", MaxConfigEntryBytes)},
		{[]any{"assignments", 0, "monitor"}, "name", strings.Repeat("é", 128)},
		{[]any{"assignments", 0, "monitor"}, "timeout", float64(math.MaxInt32) + 1},
		{[]any{"assignments", 0, "monitor"}, "interval", int64(math.MaxInt32) + 1},
		{[]any{"assignments", 0, "monitor"}, "config", map[string]any{"secret": strings.Repeat("x", MaxConfigObjectBytes)}},
		{[]any{"notification_templates", 0}, "config", map[string]any{"html": strings.Repeat("x", MaxTemplateConfigBytes)}},
		{[]any{"notification_templates", 0}, "body_template", strings.Repeat("x", (64<<10)+1)},
		{[]any{"proxy_bindings", 0}, "port", 65536},
		{[]any{"proxy_bindings", 0}, "protocol", "socks4"},
		{[]any{"watchdog"}, "lost_after_seconds", 0},
	} {
		frame := mutateJSON(t, fixture, func(root map[string]any) { configTestNode(root, test.path).(map[string]any)[test.key] = test.value })
		if _, err := DecodeConfigSnapshot(frame); err == nil {
			t.Fatalf("accepted out-of-bounds %v/%s", test.path, test.key)
		}
	}
	// The streaming collection decoder stops before invoking a DTO decoder on
	// an excess element; nested DTO arrays must never be unmarshaled unbounded.
	for _, limit := range []int{1, MaxConfigTags, MaxConfigDependencies, MaxConfigAssignments} {
		input := []byte("[" + strings.TrimSuffix(strings.Repeat("{},", limit+1), ",") + "]")
		called := 0
		_, err := decodeConfigList(input, limit, func([]byte) (int, error) { called++; return called, nil })
		if err == nil || called != limit {
			t.Fatalf("collection limit %d invoked decoder %d times: %v", limit, called, err)
		}
	}
	for _, ids := range [][]int64{nil, {0}, {-1}, {1, 1}, {1, 2}} {
		if validConfigIDs(ids, 1) {
			t.Fatal("invalid reference ID list accepted")
		}
	}
}

func TestConfigSnapshotAcceptsCompleteAssignmentLimit(t *testing.T) {
	base, err := DecodeConfigSnapshot(readFixture(t, "valid", "config-snapshot-http.json"))
	if err != nil {
		t.Fatal(err)
	}
	assignment := base.Assignments[0]
	assignment.MaintenanceIDs = []int64{}
	base.MaintenanceWindows = []ConfigMaintenance{}
	base.Assignments = make([]ConfigAssignment, MaxConfigAssignments)
	for i := range base.Assignments {
		base.Assignments[i] = assignment
		base.Assignments[i].MonitorID = int64(i + 1)
	}
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, err := DecodeConfigSnapshot(data); err != nil || len(snapshot.Assignments) != MaxConfigAssignments {
		t.Fatalf("assignment limit rejected: %v", err)
	}
	base.Assignments = append(base.Assignments, assignment)
	data, err = json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeConfigSnapshot(data); err == nil {
		t.Fatal("excess assignment accepted")
	}
}

func TestConfigSnapshotRecognizesOnlyExistingRemoteInventory(t *testing.T) {
	fixture := readFixture(t, "valid", "config-snapshot-empty.json")
	base, err := DecodeConfigSnapshot(readFixture(t, "valid", "config-snapshot-http.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"http", "tcp", "ping", "dns", "websocket", "docker", "mqtt", "rabbitmq", "grpc", "snmp", "database", "s3", "push", "unknown"} {
		assignment := base.Assignments[0]
		assignment.Monitor.Type = kind
		assignment.NotificationIDs, assignment.NotificationLinks, assignment.MaintenanceIDs = []int64{}, []ConfigNotificationLink{}, []int64{}
		assignment.ProxyBindingKey, assignment.EscalationPolicyID = nil, nil
		assignment.RequiredCapabilities = []string{"checker." + kind + ".v1"}
		if kind == "docker" {
			assignment.ResourceBindings = []ResourceBinding{{BindingKey: "docker", Kind: "docker_api"}}
		}
		frame := mutateJSON(t, fixture, func(root map[string]any) { root["assignments"] = []ConfigAssignment{assignment} })
		_, err := DecodeConfigSnapshot(frame)
		if (err == nil) != (kind != "push" && kind != "unknown") {
			t.Fatalf("inventory %s: %v", kind, err)
		}
	}
	for _, provider := range []string{"telegram", "discord", "slack", "smtp", "webhook", "teams", "mattermost", "gotify", "bark", "feishu", "line", "unknown"} {
		channel := base.NotificationChannels[0]
		channel.Type, channel.TemplateID = provider, nil
		frame := mutateJSON(t, fixture, func(root map[string]any) { root["notification_channels"] = []ConfigChannel{channel} })
		_, err := DecodeConfigSnapshot(frame)
		if (err == nil) != (provider != "unknown") {
			t.Fatalf("provider %s: %v", provider, err)
		}
	}
	// Shape recognition deliberately does not claim checker configuration validity.
	// These inventory tests use the same HTTP extension object for every type.
}

func configTestNode(root any, path []any) any {
	node := root
	for _, component := range path {
		switch key := component.(type) {
		case string:
			node = node.(map[string]any)[key]
		case int:
			node = node.([]any)[key]
		default:
			return fmt.Sprintf("invalid test path component %v", key)
		}
	}
	return node
}
