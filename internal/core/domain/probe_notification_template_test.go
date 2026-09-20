package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProbeNotificationTemplateIdentityAndEscaping(t *testing.T) {
	alert := AlertContext{AlertScope: AlertScopeProbe, DeliveryScope: IncidentScopeProbeConnection, ProbeID: "22222222-2222-4222-8222-222222222222", ProbeName: `BKK "edge" <probe>`, ProbeLocation: "Thailand", SourceAlertID: "33333333-3333-4333-8333-333333333333"}
	text, err := RenderNotificationTemplate(`{"id":{{json.alert.id}},"scope":{{json.alert.scope}},"source":{{json.alert.source_id}},"delivery":{{json.alert.delivery_scope}},"name":{{json.probe.name}},"location":{{json.probe.location}}}`, alert, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	if value["id"] != alert.ProbeID || value["source"] != alert.SourceAlertID || value["scope"] != "probe" || value["delivery"] != "probe_connection" || value["name"] != alert.ProbeName || value["location"] != "Thailand" {
		t.Fatal("lost probe template identity", value)
	}
	html, err := RenderNotificationHTMLTemplate(`<p>{{probe.name}}</p>`, alert, time.Now())
	if err != nil || strings.Contains(html, "<probe>") || !strings.Contains(html, "&lt;probe&gt;") {
		t.Fatal("probe data bypassed HTML escaping", html, err)
	}
	for _, legacy := range []AlertContext{{AlertScope: AlertScopeMonitor, MonitorID: 7}, {AlertScope: AlertScopeGroup, GroupID: 7}} {
		text, err := RenderNotificationTemplate(`{"id":{{json.alert.id}}}`, legacy, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(text), &value); err != nil || value["id"] != float64(7) {
			t.Fatal("legacy numeric ID changed", text, err)
		}
	}
}
