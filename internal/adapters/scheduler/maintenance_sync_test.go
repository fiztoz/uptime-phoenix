package scheduler

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestPublishedMaintenanceAndNotificationContextApplies proves a remote snapshot
// keeps the schedules, links, templates, visibility, tags, owner and escalation
// policy the edge actually executes. Evaluation is stateless, so a second decode
// is the offline-restart case.
func TestPublishedMaintenanceAndNotificationContextApplies(t *testing.T) {
	at := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	policyID, templateID := int64(20), int64(30)
	definition := domain.LocalProbeConfigDefinition{
		Target:      domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222"},
		Revision:    7,
		CreatedAt:   at,
		EffectiveAt: at,
		Probe:       domain.ProbeDisplay{Name: "Bangkok edge", Location: "TH"},
		Assignments: []domain.ProbeConfigAssignment{{
			Monitor:            &domain.Monitor{ID: 1, Name: "checkout", Type: "http", Active: true, Interval: 30, Timeout: 1, Owner: "contact", Config: map[string]any{"url": "https://secret.example"}},
			Generation:         1,
			EffectiveOwner:     "parent owner",
			NotificationLinks:  []domain.MonitorNotification{{MonitorID: 1, NotificationID: 101, IncludeTarget: false}},
			MaintenanceIDs:     []int64{50},
			EscalationPolicyID: &policyID,
			Tags:               []domain.ProbeConfigTag{{Name: "team", Value: "ops"}},
		}},
		Notifications: []*domain.Notification{{ID: 101, Name: "direct", Type: "webhook", Active: true, IncludeAckURL: true, TemplateID: &templateID, Config: map[string]any{"url": "https://example.test/hook"}}},
		Templates:     []*domain.NotificationTemplate{{ID: 30, Name: "layout", Provider: "webhook", TitleTemplate: "{{probe.name}}", BodyTemplate: "{{monitor.owner}}"}},
		Policies:      []*domain.EscalationPolicy{{ID: 20, Enabled: true, Steps: []domain.EscalationStep{{StepOrder: 1, WaitMinutes: 5, NotificationIDs: []int64{101}}}}},
		Maintenance: []domain.ProbeConfigMaintenance{{
			Window:     &domain.MaintenanceWindow{ID: 50, Active: true, Strategy: "cron", CronExpr: "0 2 * * *", Duration: 60, Timezone: "Asia/Bangkok"},
			MonitorIDs: []int64{1},
		}},
	}
	document, err := (probe.RemoteConfigEncoder{}).EncodeRemote(definition)
	if err != nil {
		t.Fatal(err)
	}
	eval := NewCronEvaluator()
	decode := func() *domain.EdgeResolvedConfig {
		t.Helper()
		resolved, err := probe.NewEdgeConfigDecoder(checker.Get, notifier.Get).DecodeEdge(t.Context(), document, definition.Target)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	resolved := decode()
	assignment := resolved.Assignments[0]
	if resolved.Probe.Name != "Bangkok edge" || resolved.Probe.Location != "TH" || assignment.EffectiveOwner != "parent owner" || assignment.Monitor.Owner != "contact" {
		t.Fatalf("owner or region context dropped: %+v", assignment)
	}
	if len(assignment.Tags) != 1 || assignment.Tags[0].Name != "team" || assignment.Tags[0].Value != "ops" {
		t.Fatalf("tags dropped: %+v", assignment.Tags)
	}
	if len(assignment.NotificationLinks) != 1 || assignment.NotificationLinks[0].IncludeTarget || assignment.NotificationLinks[0].NotificationID != 101 {
		t.Fatalf("direct link or target visibility dropped: %+v", assignment.NotificationLinks)
	}
	channel := resolved.Channels[101]
	if channel.Notification == nil || channel.Notification.IncludeAckURL || channel.Notification.Config["url"] != "https://example.test/hook" {
		t.Fatalf("provider configuration dropped: %+v", channel.Notification)
	}
	template := resolved.Templates[30]
	if template == nil || template.TitleTemplate != "{{probe.name}}" || template.BodyTemplate != "{{monitor.owner}}" {
		t.Fatalf("template dropped: %+v", template)
	}
	policy := resolved.Policies[20]
	if assignment.EscalationPolicyID == nil || *assignment.EscalationPolicyID != 20 || policy == nil || !policy.Enabled || len(policy.Steps) != 1 || policy.Steps[0].WaitMinutes != 5 || policy.Steps[0].NotificationIDs[0] != 101 {
		t.Fatalf("effective escalation policy dropped: %+v %+v", assignment.EscalationPolicyID, policy)
	}
	window := resolved.Maintenance[50]
	if window == nil || window.Timezone != "Asia/Bangkok" || window.Duration != 60 || window.CronExpr != "0 2 * * *" || len(assignment.MaintenanceIDs) != 1 {
		t.Fatalf("maintenance schedule dropped: %+v", window)
	}

	bkk, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Fatal(err)
	}
	inside := time.Date(2026, 3, 15, 2, 15, 0, 0, bkk)
	active, err := services.EdgeMaintenanceActive(resolved, assignment, eval, inside)
	if err != nil || !active {
		t.Fatal("02:15 Asia/Bangkok did not suppress", err)
	}
	// A restarted process reloads the accepted bytes. It must not depend on the
	// previous process having remembered that the window was open.
	reloaded := decode()
	active, err = services.EdgeMaintenanceActive(reloaded, reloaded.Assignments[0], eval, inside)
	if err != nil || !active {
		t.Fatal("reloaded schedule did not suppress", err)
	}
	outside := time.Date(2026, 3, 15, 2, 15, 0, 0, time.UTC)
	active, err = services.EdgeMaintenanceActive(resolved, assignment, eval, outside)
	if err != nil || active {
		t.Fatal("02:15 UTC matched an Asia/Bangkok window", err)
	}
	unlinked := assignment
	unlinked.MaintenanceIDs = nil
	resolved.Maintenance[99] = &domain.MaintenanceWindow{ID: 99, Active: true, Strategy: "cron", CronExpr: "0 2 * * *", Duration: 60, Timezone: "Asia/Bangkok"}
	active, err = services.EdgeMaintenanceActive(resolved, unlinked, eval, inside)
	if err != nil || active {
		t.Fatal("unlinked window suppressed", err)
	}

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	dst := &domain.EdgeResolvedConfig{Maintenance: map[int64]*domain.MaintenanceWindow{
		1: {ID: 1, Active: true, Strategy: "cron", CronExpr: "30 1 * * *", Duration: 120, Timezone: "America/New_York"},
	}}
	linked := domain.EdgeResolvedAssignment{MaintenanceIDs: []int64{1}}
	for _, sample := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before spring-forward", time.Date(2026, 3, 8, 1, 45, 0, 0, ny), true},
		{"inside duration after the gap", time.Date(2026, 3, 8, 3, 15, 0, 0, ny), true},
		{"after the window", time.Date(2026, 3, 8, 5, 0, 0, 0, ny), false},
	} {
		active, err = services.EdgeMaintenanceActive(dst, linked, eval, sample.at)
		if err != nil || active != sample.want {
			t.Fatalf("%s = %v %v, want %v", sample.name, active, err, sample.want)
		}
	}
	utc := &domain.EdgeResolvedConfig{Maintenance: map[int64]*domain.MaintenanceWindow{
		1: {ID: 1, Active: true, Strategy: "cron", CronExpr: "0 2 * * *", Duration: 60, Timezone: ""},
	}}
	active, err = services.EdgeMaintenanceActive(utc, linked, eval, outside)
	if err != nil || !active {
		t.Fatal("empty timezone was not UTC", err)
	}
	utc.Maintenance[1].Timezone = "Not/AZone"
	if _, err = services.EdgeMaintenanceActive(utc, linked, eval, outside); err == nil {
		t.Fatal("unknown timezone suppressed on UTC")
	}
}
