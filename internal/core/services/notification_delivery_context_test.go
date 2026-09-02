package services

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type deliveryContextNotifRepo struct {
	*fakeNotifRepo
	byMonitor map[int64][]*domain.Notification
}

func (r *deliveryContextNotifRepo) GetByMonitorID(_ context.Context, monitorID int64) ([]*domain.Notification, error) {
	return r.byMonitor[monitorID], nil
}

type deliveryContextSender struct {
	mu     sync.Mutex
	alerts []domain.AlertContext
}

func (s *deliveryContextSender) Type() string                    { return "delivery-context" }
func (s *deliveryContextSender) Validate(_ map[string]any) error { return nil }
func (s *deliveryContextSender) Send(_ context.Context, _ map[string]any, alert domain.AlertContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, alert)
	return nil
}

func (s *deliveryContextSender) last(t *testing.T) domain.AlertContext {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.alerts) == 0 {
		t.Fatal("sender received no alert")
	}
	return s.alerts[len(s.alerts)-1]
}

type deliveryContextTagReader struct {
	tags []MonitorTagDetail
}

func (r *deliveryContextTagReader) TagsForMonitor(_ context.Context, _ int64) ([]MonitorTagDetail, error) {
	return r.tags, nil
}

func TestNotificationService_MonitorTemplateContextUsesLifecycleAndTags(t *testing.T) {
	ctx := context.Background()
	baseRepo := newFakeNotifRepo()
	notification := &domain.Notification{
		UserID: 1, Name: "email", Type: "delivery-context", Active: true,
	}
	if err := baseRepo.Create(ctx, notification); err != nil {
		t.Fatalf("create notification: %v", err)
	}
	repo := &deliveryContextNotifRepo{
		fakeNotifRepo: baseRepo,
		byMonitor:     map[int64][]*domain.Notification{42: {notification}},
	}
	sender := &deliveryContextSender{}
	svc := NewNotificationService(repo, newFakeMonitorNotifLinkRepo())
	svc.RegisterSender(sender)
	svc.SetTagReader(&deliveryContextTagReader{tags: []MonitorTagDetail{
		{Name: "team", Value: "payments"},
		{Name: "region", Value: "ap-southeast-1"},
	}})

	createdAt := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, time.August, 8, 2, 1, 0, 0, time.UTC)
	monitor := &domain.Monitor{
		ID: 42, UserID: 1, Name: "Payments API", Type: "http",
		Config:    map[string]any{"url": "https://api.example.com/health"},
		CreatedAt: createdAt,
	}
	if err := svc.NotifyWithAlertDetails(
		ctx, monitor, domain.StatusUp, domain.StatusDown, "", "200 OK",
		startedAt, 3*time.Minute+12*time.Second,
	); err != nil {
		t.Fatalf("NotifyWithAlertDetails: %v", err)
	}

	alert := sender.last(t)
	if !alert.StartedAt.Equal(startedAt) || alert.StartedAt.Equal(createdAt) {
		t.Errorf("started_at = %s; want lifecycle start %s, never monitor creation %s",
			alert.StartedAt, startedAt, createdAt)
	}
	if alert.Duration != 3*time.Minute+12*time.Second {
		t.Errorf("duration = %s; want 3m12s", alert.Duration)
	}
	if alert.Tags["team"] != "payments" || alert.Tags["region"] != "ap-southeast-1" {
		t.Errorf("tags = %#v; want named monitor tag values", alert.Tags)
	}
}

func TestNotificationService_GroupContextLeavesUnsupportedMonitorValuesEmpty(t *testing.T) {
	ctx := context.Background()
	baseRepo := newFakeNotifRepo()
	notification := &domain.Notification{
		UserID: 1, Name: "email", Type: "delivery-context", Active: true,
	}
	if err := baseRepo.Create(ctx, notification); err != nil {
		t.Fatalf("create notification: %v", err)
	}
	repo := &deliveryContextNotifRepo{fakeNotifRepo: baseRepo}
	groupRepo := newGalertGroupNotifRepo(baseRepo)
	if err := groupRepo.Attach(ctx, 7, notification.ID); err != nil {
		t.Fatalf("attach group notification: %v", err)
	}
	sender := &deliveryContextSender{}
	svc := NewNotificationService(repo, newFakeMonitorNotifLinkRepo())
	svc.SetGroupNotificationRepo(groupRepo)
	svc.RegisterSender(sender)

	group := &domain.MonitorGroup{
		ID: 7, UserID: 1, Name: "Platform Services",
		Condition: domain.GroupConditionThreshold, Threshold: 2,
		CreatedAt: time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := svc.NotifyGroup(ctx, group, domain.StatusUp, domain.StatusDown); err != nil {
		t.Fatalf("NotifyGroup: %v", err)
	}

	alert := sender.last(t)
	if !alert.StartedAt.IsZero() || alert.Duration != 0 || alert.AckURL != "" || len(alert.Tags) != 0 {
		t.Errorf("group-only optional values = started:%s duration:%s ack:%q tags:%#v; want empty",
			alert.StartedAt, alert.Duration, alert.AckURL, alert.Tags)
	}
}

func TestNotificationService_IncludeAckURLToggle(t *testing.T) {
	const ackURL = "https://status.example.com/ack/token"

	cases := []struct {
		name          string
		includeAckURL bool
		wantAckURL    string
		wantAckLine   bool
	}{
		{name: "include", includeAckURL: true, wantAckURL: ackURL, wantAckLine: true},
		{name: "omit", includeAckURL: false, wantAckURL: "", wantAckLine: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			baseRepo := newFakeNotifRepo()
			notification := &domain.Notification{
				UserID: 1, Name: "pager", Type: "delivery-context", Active: true,
				IncludeAckURL: tc.includeAckURL,
			}
			if err := baseRepo.Create(ctx, notification); err != nil {
				t.Fatalf("create notification: %v", err)
			}
			repo := &deliveryContextNotifRepo{
				fakeNotifRepo: baseRepo,
				byMonitor:     map[int64][]*domain.Notification{9: {notification}},
			}
			sender := &deliveryContextSender{}
			svc := NewNotificationService(repo, newFakeMonitorNotifLinkRepo())
			svc.RegisterSender(sender)

			monitor := &domain.Monitor{
				ID: 9, UserID: 1, Name: "API", Type: "http",
				Config: map[string]any{"url": "https://api.example.com/health"},
			}
			if err := svc.NotifyWithAck(ctx, monitor, domain.StatusDown, domain.StatusUp, ackURL, "timeout"); err != nil {
				t.Fatalf("NotifyWithAck: %v", err)
			}

			alert := sender.last(t)
			if alert.AckURL != tc.wantAckURL {
				t.Errorf("AckURL = %q; want %q", alert.AckURL, tc.wantAckURL)
			}
			hasLine := strings.Contains(alert.Message, "Acknowledge: "+ackURL)
			if hasLine != tc.wantAckLine {
				t.Errorf("message %q: ack line present=%v; want %v", alert.Message, hasLine, tc.wantAckLine)
			}
		})
	}
}

func TestNotificationService_IncludeTargetToggle(t *testing.T) {
	const target = "https://api.example.com/health"

	cases := []struct {
		name          string
		includeTarget bool
		wantTarget    string
	}{
		{name: "include", includeTarget: true, wantTarget: target},
		{name: "omit", includeTarget: false, wantTarget: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			baseRepo := newFakeNotifRepo()
			notification := &domain.Notification{
				UserID: 1, Name: "pager", Type: "delivery-context", Active: true,
			}
			if err := baseRepo.Create(ctx, notification); err != nil {
				t.Fatalf("create notification: %v", err)
			}
			linkRepo := newFakeMonitorNotifLinkRepo()
			if err := linkRepo.Attach(ctx, 9, notification.ID, tc.includeTarget); err != nil {
				t.Fatalf("attach link: %v", err)
			}
			repo := &deliveryContextNotifRepo{
				fakeNotifRepo: baseRepo,
				byMonitor:     map[int64][]*domain.Notification{9: {notification}},
			}
			sender := &deliveryContextSender{}
			svc := NewNotificationService(repo, linkRepo)
			svc.RegisterSender(sender)

			monitor := &domain.Monitor{
				ID: 9, UserID: 1, Name: "API", Type: "http",
				Config: map[string]any{"url": target},
			}
			if err := svc.NotifyWithAck(ctx, monitor, domain.StatusDown, domain.StatusUp, "", "timeout"); err != nil {
				t.Fatalf("NotifyWithAck: %v", err)
			}

			alert := sender.last(t)
			if alert.MonitorTarget != tc.wantTarget {
				t.Errorf("MonitorTarget = %q; want %q", alert.MonitorTarget, tc.wantTarget)
			}
		})
	}
}
