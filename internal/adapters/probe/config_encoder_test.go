package probe

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func localDefinitionFixture() domain.LocalProbeConfigDefinition {
	at := time.Date(2026, 9, 18, 12, 0, 0, 123456789, time.FixedZone("offset", 7*3600))
	proxyID, policyID, templateID := int64(40), int64(20), int64(30)
	return domain.LocalProbeConfigDefinition{
		Target: domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "local"}, Revision: 7, CreatedAt: at, EffectiveAt: at,
		Assignments: []domain.ProbeConfigAssignment{
			{Monitor: &domain.Monitor{ID: 2, UserID: 123, Name: "push", Type: "push", Interval: 60, Timeout: 30, PushToken: "do-not-export-token"}, Generation: 3},
			{Monitor: &domain.Monitor{ID: 1, Name: "http", Type: "http", Active: true, Interval: 30, Timeout: 1.25, ProxyID: &proxyID, Config: map[string]any{"url": "https://example.test", "authorization": "fixture-check-secret"}, AcceptedStatusCodes: []string{"200-299"}}, Generation: 1, EffectiveOwner: "ops", NotificationLinks: []domain.MonitorNotification{{MonitorID: 1, NotificationID: 101, IncludeTarget: false}}, MaintenanceIDs: []int64{50}, EscalationPolicyID: &policyID, Tags: []domain.ProbeConfigTag{{Name: "z", Value: "second"}, {Name: "a", Value: "first"}}},
		},
		Notifications: []*domain.Notification{{ID: 102, Name: "disabled escalation", Type: "smtp"}, {ID: 101, Name: "direct", Type: "webhook", Active: true, IncludeAckURL: true, TemplateID: &templateID, Config: map[string]any{"url": "https://example.test/hook", "token": "fixture-provider-secret"}}},
		Templates:     []*domain.NotificationTemplate{{ID: 30, Name: "layout", Provider: "webhook", BodyTemplate: "{{monitor.name}}"}},
		Proxies:       []*domain.Proxy{{ID: 40, Protocol: "http", Host: "proxy.test", Port: 8080, Password: "fixture-proxy-secret", Active: false}},
		Policies:      []*domain.EscalationPolicy{{ID: 20, Enabled: false, Steps: []domain.EscalationStep{{StepOrder: 1, WaitMinutes: 5, NotificationIDs: []int64{102}}}}},
		Maintenance:   []domain.ProbeConfigMaintenance{{Window: &domain.MaintenanceWindow{ID: 50, Active: true, Strategy: "single", Timezone: "UTC", StartDate: at, EndDate: at.Add(time.Hour)}, MonitorIDs: []int64{1}}},
	}
}

func TestLocalConfigEncoderCompleteDeterministicDocument(t *testing.T) {
	d := localDefinitionFixture()
	doc, err := (LocalConfigEncoder{}).EncodeLocal(d)
	if err != nil {
		t.Fatal(err)
	}
	s, err := DecodeLocalConfigSnapshot(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeConfigSnapshot(doc); err == nil {
		t.Fatal("local document entered remote dialect")
	}
	if s.Assignments[0].MonitorID != 1 || s.Assignments[1].Active || s.Assignments[1].Generation != 3 || s.Assignments[0].Monitor.Timeout != 1.25 || s.Assignments[0].Monitor.Tags[0].Name != "a" || s.Assignments[0].NotificationLinks[0].IncludeTarget || !s.NotificationChannels[0].IncludeAckURL {
		t.Fatal("changed local execution or notification metadata")
	}
	if s.NotificationChannels[0].Version != 7 || s.NotificationTemplates[0].Version != 7 || s.ProxyBindings[0].Version != 7 || s.EscalationPolicies[0].Version != 7 || s.EscalationPolicies[0].Steps[0].DelaySeconds != 300 || s.EscalationPolicies[0].Enabled || s.Watchdog.Enabled {
		t.Fatal("lost version, delay or disabled state")
	}
	for _, forbidden := range []string{"do-not-export-token", "user_id", "UserID", "created_by", "push_token", "accepted_status_codes"} {
		if bytes.Contains(doc, []byte(forbidden)) {
			t.Fatalf("exported forbidden field %s", forbidden)
		}
	}
	if !bytes.Contains(doc, []byte(`"accepted_statuscodes"`)) || time.Time(s.CreatedAt).Location() != time.UTC || time.Time(s.CreatedAt).Nanosecond() != 123456000 {
		t.Fatal("wire names or UTC precision changed")
	}
	// Reordering source collections must not change the exact protected bytes.
	d.Assignments[0], d.Assignments[1] = d.Assignments[1], d.Assignments[0]
	d.Notifications[0], d.Notifications[1] = d.Notifications[1], d.Notifications[0]
	doc2, err := (LocalConfigEncoder{}).EncodeLocal(d)
	if err != nil || !bytes.Equal(doc, doc2) {
		t.Fatal("source order changed snapshot hash")
	}
	if d.Assignments[0].Tags[0].Name != "z" {
		t.Fatal("encoder sorted caller-owned data")
	}
}

func TestLocalConfigEncoderRejectsInvalidAndOversizedInput(t *testing.T) {
	for name, mutate := range map[string]func(*domain.LocalProbeConfigDefinition){
		"remote":            func(d *domain.LocalProbeConfigDefinition) { d.Target.ProbeID = d.Target.HubID },
		"integer overflow":  func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor.Interval = math.MaxInt32 + 1 },
		"delay overflow":    func(d *domain.LocalProbeConfigDefinition) { d.Policies[0].Steps[0].WaitMinutes = math.MaxInt },
		"missing channel":   func(d *domain.LocalProbeConfigDefinition) { d.Notifications = d.Notifications[1:] },
		"template provider": func(d *domain.LocalProbeConfigDefinition) { d.Templates[0].Provider = "smtp" },
		"nonfinite":         func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor.Timeout = math.Inf(1) },
		"large object": func(d *domain.LocalProbeConfigDefinition) {
			d.Notifications[0].Config = map[string]any{"secret": strings.Repeat("s", MaxConfigObjectBytes)}
		},
		"invalid object": func(d *domain.LocalProbeConfigDefinition) {
			d.Notifications[0].Config = map[string]any{"fixture-secret-value": make(chan int)}
		},
		"duplicate tag":               func(d *domain.LocalProbeConfigDefinition) { d.Assignments[1].Tags[1].Name = "z" },
		"missing maintenance reverse": func(d *domain.LocalProbeConfigDefinition) { d.Maintenance[0].MonitorIDs = []int64{2} },
		"nil monitor":                 func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := localDefinitionFixture()
			mutate(&d)
			data, err := (LocalConfigEncoder{}).EncodeLocal(d)
			if !errors.Is(err, domain.ErrValidation) || data != nil || strings.Contains(err.Error(), "fixture-secret-value") {
				t.Fatal("invalid source succeeded or exposed credentials")
			}
		})
	}
	// Empty local snapshots are complete replacements, with real empty arrays.
	d := localDefinitionFixture()
	d.Assignments, d.Notifications, d.Templates, d.Proxies, d.Policies, d.Maintenance = nil, nil, nil, nil, nil, nil
	if _, err := (LocalConfigEncoder{}).EncodeLocal(d); err != nil {
		t.Fatal(err)
	}
}
