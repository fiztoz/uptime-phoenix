package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// fakeDeliveryOutboxRepo implements ports.DeliveryOutboxRepository in-memory for testing.
type fakeDeliveryOutboxRepo struct {
	mu         sync.Mutex
	intents    map[string]*domain.QueuedDelivery
	finishLogs []domain.DeliveryResult
}

func newFakeDeliveryOutboxRepo() *fakeDeliveryOutboxRepo {
	return &fakeDeliveryOutboxRepo{
		intents: make(map[string]*domain.QueuedDelivery),
	}
}

func (r *fakeDeliveryOutboxRepo) ClaimDeliveries(_ context.Context, probeID string, at time.Time, lease time.Duration, limit int) ([]domain.QueuedDelivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var claimed []domain.QueuedDelivery
	until := at.Add(lease)
	for _, intent := range r.intents {
		if intent.ProbeID != probeID {
			continue
		}
		if intent.Status == domain.DeliveryStatusPending ||
			intent.Status == domain.DeliveryStatusRetrying ||
			(intent.Status == domain.DeliveryStatusLeased && intent.LeaseUntil != nil && at.After(*intent.LeaseUntil)) {
			if at.Before(intent.AvailableAt) {
				continue
			}
			intent.Status = domain.DeliveryStatusLeased
			intent.Attempt++
			intent.LeaseToken = fmt.Sprintf("token-%d", intent.Attempt)
			intent.LeasedAt = &at
			intent.LeaseUntil = &until
			claimed = append(claimed, *intent)
			if len(claimed) >= limit {
				break
			}
		}
	}
	return claimed, nil
}

func (r *fakeDeliveryOutboxRepo) FinishDelivery(_ context.Context, claim domain.DeliveryClaim, result domain.DeliveryResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	intent, ok := r.intents[claim.DeliveryID]
	if !ok {
		return ports.ErrNotFound
	}
	if intent.Attempt != claim.Attempt || intent.LeaseToken != claim.LeaseToken {
		return ports.ErrConflict
	}
	intent.Status = result.Status
	intent.OutcomeAt = &result.At
	if result.ErrorCode != "" {
		intent.ErrorCode = result.ErrorCode
	}
	if result.Status == domain.DeliveryStatusRetrying {
		intent.AvailableAt = result.RetryAt
	}
	r.finishLogs = append(r.finishLogs, result)
	return nil
}

func (r *fakeDeliveryOutboxRepo) GetDeliveryIntent(_ context.Context, probeID, deliveryID string) (*domain.QueuedDelivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	intent, ok := r.intents[deliveryID]
	if !ok || intent.ProbeID != probeID {
		return nil, ports.ErrNotFound
	}
	cp := *intent
	return &cp, nil
}

type mockSender struct {
	mu     sync.Mutex
	alerts []domain.AlertContext
	err    error
}

func (s *mockSender) Type() string                    { return "mock" }
func (s *mockSender) Validate(_ map[string]any) error { return nil }
func (s *mockSender) Send(_ context.Context, _ map[string]any, alert domain.AlertContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, alert)
	return s.err
}

func (s *mockSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.alerts)
}

func (s *mockSender) last() domain.AlertContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alerts[len(s.alerts)-1]
}

type fakeProbeAssignmentRepo struct {
	sets map[int64]*domain.MonitorProbeAssignments
}

func (r *fakeProbeAssignmentRepo) InitializeLocal(_ context.Context, _ int64) (*domain.MonitorProbeAssignments, error) {
	return nil, nil
}

func (r *fakeProbeAssignmentRepo) GetByMonitorID(_ context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error) {
	s, ok := r.sets[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return s, nil
}

func (r *fakeProbeAssignmentRepo) Replace(_ context.Context, _ int64, _ int64, _ []string, _ domain.HealthPolicy) (*domain.MonitorProbeAssignments, error) {
	return nil, nil
}

func (r *fakeProbeAssignmentRepo) ExecutableByLocal(_ context.Context, _ []int64) (map[int64]int64, error) {
	return nil, nil
}

func (r *fakeProbeAssignmentRepo) ListHistory(_ context.Context, _ int64, _, _ time.Time) ([]domain.AssignmentInterval, error) {
	return nil, nil
}

type consumerTestActivationRepo struct {
	active map[string]*domain.ProbeActiveConfig
}

func (r *consumerTestActivationRepo) GetActive(_ context.Context, probeID string) (*domain.ProbeActiveConfig, error) {
	a, ok := r.active[probeID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return a, nil
}

func (r *consumerTestActivationRepo) GetReceipt(_ context.Context, _ string, _ int64) (*domain.ProbeActiveConfig, error) {
	return nil, ports.ErrNotFound
}

func (r *consumerTestActivationRepo) ActivateLocal(_ context.Context, _ ports.LocalActivationParams) (*domain.ProbeActiveConfig, error) {
	return nil, nil
}

type consumerTestIncidentRepo struct {
	incidents map[string]*domain.RegionalIncident
}

func (r *consumerTestIncidentRepo) PutIncident(_ context.Context, inc *domain.RegionalIncident) error {
	r.incidents[inc.SourceAlertID] = inc
	return nil
}

func (r *consumerTestIncidentRepo) GetIncident(_ context.Context, sourceAlertID string) (*domain.RegionalIncident, error) {
	inc, ok := r.incidents[sourceAlertID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return inc, nil
}

func (r *consumerTestIncidentRepo) ListIncidentsByMonitor(_ context.Context, _ int64) ([]domain.RegionalIncident, error) {
	return nil, nil
}

type consumerMaintenanceChecker struct {
	active map[int64]bool
}

func (m *consumerMaintenanceChecker) IsActive(_ context.Context, monitorID int64) (bool, error) {
	if m == nil || m.active == nil {
		return false, nil
	}
	return m.active[monitorID], nil
}

func TestDeliveryConsumer_Reconcile_ExpiredLease(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{}
	notifs := newFakeNotifRepo()

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	consumer.now = func() time.Time { return now }

	expired := now.Add(-1 * time.Minute)
	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID: "d-1",
			ProbeID:    domain.LocalProbeID,
		},
		Status:     domain.DeliveryStatusLeased,
		Attempt:    1,
		LeaseToken: "token-1",
		LeaseUntil: &expired,
	}

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.count() != 0 {
		t.Errorf("sender called %d times; want 0 for expired lease", sender.count())
	}
	if len(outbox.finishLogs) != 0 {
		t.Errorf("finish called %d times; want 0 for dropped expired lease", len(outbox.finishLogs))
	}
}

func TestDeliveryConsumer_Reconcile_AssignmentSuperseded(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{}
	notifs := newFakeNotifRepo()

	assignments := &fakeProbeAssignmentRepo{
		sets: map[int64]*domain.MonitorProbeAssignments{
			1: {
				MonitorID: 1,
				Assignments: []domain.ProbeAssignment{
					{
						MonitorID:  1,
						ProbeID:    domain.LocalProbeID,
						Generation: 2, // generation bumped!
					},
				},
			},
		},
	}

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.SetAssignmentRepository(assignments)
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	consumer.now = func() time.Time { return now }

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID: "d-1",
			ProbeID:    domain.LocalProbeID,
		},
		MonitorID:            1,
		AssignmentGeneration: 1, // older generation!
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeaseUntil:           &until,
	}
	outbox.intents["d-1"] = &delivery

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.count() != 0 {
		t.Errorf("sender called %d times; want 0", sender.count())
	}
	if len(outbox.finishLogs) != 1 || outbox.finishLogs[0].Status != domain.DeliveryStatusSuperseded {
		t.Fatalf("finishLogs = %#v; want 1 superseded", outbox.finishLogs)
	}
}

func TestDeliveryConsumer_Reconcile_ChannelVersionRotated(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{}
	notifs := newFakeNotifRepo()

	activations := &consumerTestActivationRepo{
		active: map[string]*domain.ProbeActiveConfig{
			domain.LocalProbeID: {
				Revision: 5, // active revision rotated to 5!
			},
		},
	}

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.SetActivationRepository(activations)
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	consumer.now = func() time.Time { return now }

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:          "d-1",
			ProbeID:             domain.LocalProbeID,
			NotificationVersion: 4, // older revision!
		},
		MonitorID:            1,
		AssignmentGeneration: 1,
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeaseUntil:           &until,
	}
	outbox.intents["d-1"] = &delivery

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.count() != 0 {
		t.Errorf("sender called %d times; want 0 for rotated channel version", sender.count())
	}
	if len(outbox.finishLogs) != 1 || outbox.finishLogs[0].Status != domain.DeliveryStatusSuperseded {
		t.Fatalf("finishLogs = %#v; want 1 superseded", outbox.finishLogs)
	}
}

func TestDeliveryConsumer_Reconcile_ChannelDisabled(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{}
	notifs := newFakeNotifRepo()

	notif := &domain.Notification{
		UserID: 1,
		Type:   "mock",
		Active: false, // disabled!
	}
	_ = notifs.Create(ctx, notif)

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	consumer.now = func() time.Time { return now }

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:     "d-1",
			ProbeID:        domain.LocalProbeID,
			NotificationID: notif.ID,
		},
		MonitorID:            1,
		AssignmentGeneration: 1,
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeaseUntil:           &until,
	}
	outbox.intents["d-1"] = &delivery

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.count() != 0 {
		t.Errorf("sender called %d times; want 0 for disabled channel", sender.count())
	}
	if len(outbox.finishLogs) != 1 || outbox.finishLogs[0].Status != domain.DeliveryStatusSuperseded {
		t.Fatalf("finishLogs = %#v; want 1 superseded", outbox.finishLogs)
	}
}

func TestDeliveryConsumer_Reconcile_MaintenanceSuppressed(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{}
	notifs := newFakeNotifRepo()

	notif := &domain.Notification{
		UserID: 1,
		Type:   "mock",
		Active: true,
	}
	_ = notifs.Create(ctx, notif)

	maint := &consumerMaintenanceChecker{active: map[int64]bool{1: true}}

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.SetMaintenanceChecker(maint)
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	consumer.now = func() time.Time { return now }

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:     "d-1",
			ProbeID:        domain.LocalProbeID,
			NotificationID: notif.ID,
		},
		MonitorID:            1,
		AssignmentGeneration: 1,
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeaseUntil:           &until,
	}
	outbox.intents["d-1"] = &delivery

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.count() != 0 {
		t.Errorf("sender called %d times; want 0 during maintenance", sender.count())
	}
	if len(outbox.finishLogs) != 1 || outbox.finishLogs[0].Status != domain.DeliveryStatusSuperseded {
		t.Fatalf("finishLogs = %#v; want 1 superseded", outbox.finishLogs)
	}
}

func TestDeliveryConsumer_Reconcile_OldDownAfterRecovery_DelayedSummary(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{}
	notifs := newFakeNotifRepo()

	notif := &domain.Notification{
		UserID: 1,
		Type:   "mock",
		Active: true,
	}
	_ = notifs.Create(ctx, notif)

	startedAt := time.Date(2026, 9, 19, 9, 50, 0, 0, time.UTC)
	resolvedAt := time.Date(2026, 9, 19, 9, 55, 0, 0, time.UTC)

	incidents := &consumerTestIncidentRepo{
		incidents: map[string]*domain.RegionalIncident{
			"alert-1": {
				SourceAlertID: "alert-1",
				Status:        domain.AlertStatusResolved, // incident resolved while DOWN was leased!
				StartedAt:     startedAt,
				ResolvedAt:    &resolvedAt,
			},
		},
	}

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.SetIncidentRepository(incidents)
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	consumer.now = func() time.Time { return now }

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:     "d-1",
			SourceAlertID:  "alert-1",
			ProbeID:        domain.LocalProbeID,
			NotificationID: notif.ID,
			EventKind:      domain.DeliveryEventStatusChange,
		},
		MonitorID:            1,
		AssignmentGeneration: 1,
		CheckStatus:          domain.StatusDown,
		StartedAt:            startedAt,
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeaseUntil:           &until,
	}
	outbox.intents["d-1"] = &delivery

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. The old DOWN delivery was marked superseded in outbox.
	if len(outbox.finishLogs) != 1 || outbox.finishLogs[0].Status != domain.DeliveryStatusSuperseded {
		t.Fatalf("finishLogs = %#v; want 1 superseded", outbox.finishLogs)
	}

	// 2. The sender received a delayed incident summary instead of a raw DOWN.
	if sender.count() != 1 {
		t.Fatalf("sender called %d times; want 1", sender.count())
	}
	alert := sender.last()
	if alert.EventKind != domain.DeliveryEventIncidentSummary {
		t.Errorf("EventKind = %q; want %q", alert.EventKind, domain.DeliveryEventIncidentSummary)
	}
}

func TestDeliveryConsumer_SendSuccess(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{}
	notifs := newFakeNotifRepo()

	notif := &domain.Notification{
		UserID: 1,
		Type:   "mock",
		Active: true,
	}
	_ = notifs.Create(ctx, notif)

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	consumer.now = func() time.Time { return now }

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:     "d-1",
			SourceAlertID:  "alert-1",
			ProbeID:        domain.LocalProbeID,
			NotificationID: notif.ID,
			EventKind:      domain.DeliveryEventStatusChange,
		},
		MonitorID:            1,
		AssignmentGeneration: 1,
		CheckStatus:          domain.StatusDown,
		CheckOutput:          "Connection refused",
		StartedAt:            now.Add(-2 * time.Minute),
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeaseUntil:           &until,
	}
	outbox.intents["d-1"] = &delivery

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.count() != 1 {
		t.Fatalf("sender called %d times; want 1", sender.count())
	}
	if len(outbox.finishLogs) != 1 || outbox.finishLogs[0].Status != domain.DeliveryStatusSent {
		t.Fatalf("finishLogs = %#v; want 1 sent", outbox.finishLogs)
	}
}

func TestDeliveryConsumer_SendTransientError_Backoff(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{err: errors.New("dial tcp: i/o timeout")}
	notifs := newFakeNotifRepo()

	notif := &domain.Notification{
		UserID: 1,
		Type:   "mock",
		Active: true,
	}
	_ = notifs.Create(ctx, notif)

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	consumer.now = func() time.Time { return now }

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:     "d-1",
			SourceAlertID:  "alert-1",
			ProbeID:        domain.LocalProbeID,
			NotificationID: notif.ID,
			EventKind:      domain.DeliveryEventStatusChange,
		},
		MonitorID:            1,
		AssignmentGeneration: 1,
		CheckStatus:          domain.StatusDown,
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeaseUntil:           &until,
	}
	outbox.intents["d-1"] = &delivery

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(outbox.finishLogs) != 1 {
		t.Fatalf("finishLogs count = %d; want 1", len(outbox.finishLogs))
	}
	res := outbox.finishLogs[0]
	if res.Status != domain.DeliveryStatusRetrying {
		t.Errorf("status = %q; want %q", res.Status, domain.DeliveryStatusRetrying)
	}
	if res.ErrorCode != domain.ErrCodeNetworkTimeout {
		t.Errorf("error code = %q; want %q", res.ErrorCode, domain.ErrCodeNetworkTimeout)
	}
	if !res.RetryAt.After(now) {
		t.Errorf("retryAt = %s; want > now (%s)", res.RetryAt, now)
	}
}

func TestDeliveryConsumer_SendPermanentError_Failed(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{err: errors.New("401 Unauthorized: token invalid")}
	notifs := newFakeNotifRepo()

	notif := &domain.Notification{
		UserID: 1,
		Type:   "mock",
		Active: true,
	}
	_ = notifs.Create(ctx, notif)

	consumer := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer.RegisterSender(sender)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	consumer.now = func() time.Time { return now }

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:     "d-1",
			SourceAlertID:  "alert-1",
			ProbeID:        domain.LocalProbeID,
			NotificationID: notif.ID,
			EventKind:      domain.DeliveryEventStatusChange,
		},
		MonitorID:            1,
		AssignmentGeneration: 1,
		CheckStatus:          domain.StatusDown,
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeaseUntil:           &until,
	}
	outbox.intents["d-1"] = &delivery

	err := consumer.ProcessOne(ctx, delivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(outbox.finishLogs) != 1 {
		t.Fatalf("finishLogs count = %d; want 1", len(outbox.finishLogs))
	}
	res := outbox.finishLogs[0]
	if res.Status != domain.DeliveryStatusFailed {
		t.Errorf("status = %q; want %q", res.Status, domain.DeliveryStatusFailed)
	}
	if res.ErrorCode != domain.ErrCodeAuthFailed {
		t.Errorf("error code = %q; want %q", res.ErrorCode, domain.ErrCodeAuthFailed)
	}
}

// T26: Provider accepts then probe crashes: stable intent identity; external duplicate window documented and observable.
func TestDeliveryConsumer_T26_ProviderAcceptsThenProbeCrashes(t *testing.T) {
	ctx := context.Background()
	outbox := newFakeDeliveryOutboxRepo()
	sender := &mockSender{}
	notifs := newFakeNotifRepo()

	notif := &domain.Notification{
		UserID: 1,
		Type:   "mock",
		Active: true,
	}
	_ = notifs.Create(ctx, notif)

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)

	delivery := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:     "d-stable-1",
			SourceAlertID:  "alert-1",
			ProbeID:        domain.LocalProbeID,
			NotificationID: notif.ID,
			EventKind:      domain.DeliveryEventStatusChange,
		},
		MonitorID:            1,
		AssignmentGeneration: 1,
		CheckStatus:          domain.StatusDown,
		Status:               domain.DeliveryStatusLeased,
		Attempt:              1,
		LeaseToken:           "token-1",
		LeasedAt:             &now,
		LeaseUntil:           &until,
	}
	outbox.intents["d-stable-1"] = &delivery

	// Worker 1 claims and sends to provider, but crashes before FinishDelivery commits.
	_ = sender.Send(ctx, notif.Config, domain.AlertContext{MonitorID: 1})
	if sender.count() != 1 {
		t.Fatalf("sender count = %d; want 1", sender.count())
	}
	// The intent remains leased with stable ID "d-stable-1".
	curIntent, err := outbox.GetDeliveryIntent(ctx, domain.LocalProbeID, "d-stable-1")
	if err != nil || curIntent.Status != domain.DeliveryStatusLeased {
		t.Fatalf("intent status = %v; want leased", curIntent)
	}

	// Fast forward past lease expiry. Worker 2 starts up and reclaims the same delivery intent.
	consumer2 := NewDeliveryOutboxConsumer(outbox, notifs, DefaultDeliveryConsumerConfig())
	consumer2.RegisterSender(sender)
	reclaimTime := until.Add(1 * time.Second)
	consumer2.now = func() time.Time { return reclaimTime }

	processed, err := consumer2.ProcessBatch(ctx, domain.LocalProbeID, reclaimTime, 10)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if processed != 1 {
		t.Fatalf("processed = %d; want 1", processed)
	}

	// Provider received second send (external duplicate window).
	if sender.count() != 2 {
		t.Errorf("sender count = %d; want 2 (duplicate delivery)", sender.count())
	}

	// Now intent is confirmed sent.
	finalIntent, _ := outbox.GetDeliveryIntent(ctx, domain.LocalProbeID, "d-stable-1")
	if finalIntent.Status != domain.DeliveryStatusSent {
		t.Errorf("final status = %q; want sent", finalIntent.Status)
	}
	if finalIntent.Attempt != 2 {
		t.Errorf("attempt = %d; want 2", finalIntent.Attempt)
	}
}

// T39: Disabled inherited escalation policy does not fall through and page another group; step zero remains dispatcher-owned.
func TestDeliveryConsumer_T39_StepZeroRemainsDispatcherOwned(t *testing.T) {
	ctx := context.Background()
	notifier := &fakeNotifier{}
	dispatcher := NewNotificationDispatcher(notifier, &consumerMaintenanceChecker{})
	dispatcher.SetOutboxDelivery(true) // outbox delivery enabled!
	dispatcher.SetAlertLifecycle(NewAlertService(newFakeAlertRepo()))

	escalationStarted := false
	escalationMock := &mockEscalationStarter{
		startFunc: func(_ context.Context, _ *domain.Alert, _ *domain.Monitor) error {
			escalationStarted = true
			return nil
		},
	}
	dispatcher.SetEscalationStarter(escalationMock)

	monitor := &domain.Monitor{ID: 1, Name: "Test Monitor"}
	hb := &domain.Heartbeat{
		MonitorID: 1,
		ProbeID:   domain.LocalProbeID,
		Status:    domain.StatusDown,
		Msg:       "Down",
	}

	// Dispatcher receives DOWN heartbeat.
	prev := domain.StatusUp
	dispatcher.OnHeartbeat(ctx, monitor, hb, &prev)

	// Step zero is NOT sent via legacy dispatcher.dispatch (outboxDelivery is true).
	if notifier.count() != 0 {
		t.Errorf("notifier called %d times via legacy dispatcher; want 0", notifier.count())
	}
	// But escalation ladder was still evaluated!
	if !escalationStarted {
		t.Errorf("escalation ladder was not started")
	}
}

type mockEscalationStarter struct {
	startFunc func(ctx context.Context, alert *domain.Alert, monitor *domain.Monitor) error
}

func (m *mockEscalationStarter) StartForAlert(ctx context.Context, alert *domain.Alert, monitor *domain.Monitor) error {
	if m.startFunc != nil {
		return m.startFunc(ctx, alert, monitor)
	}
	return nil
}
