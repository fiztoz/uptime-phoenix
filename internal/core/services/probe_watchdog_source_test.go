package services

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// This fake validates and persists transitions, versions and delivery effects.
// Database authority/atomicity is exercised separately against both real engines.
type watchdogSourceRepo struct {
	mu           sync.Mutex
	state        domain.ProbeWatchdogState
	deliveries   []domain.DeliveryIntent
	fail         error
	beforeCommit func(context.Context) error
}

func (r *watchdogSourceRepo) ReadWatchdog(context.Context, domain.ProbeWatchdogAuthority) (domain.ProbeWatchdogState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneWatchdogState(r.state), nil
}

func cloneWatchdogState(s domain.ProbeWatchdogState) domain.ProbeWatchdogState {
	if s.Incident != nil {
		v := *s.Incident
		s.Incident = &v
	}
	if s.LastEnqueuedAt != nil {
		v := *s.LastEnqueuedAt
		s.LastEnqueuedAt = &v
	}
	return s
}

func (r *watchdogSourceRepo) CommitWatchdog(ctx context.Context, a domain.ProbeWatchdogAuthority, v domain.ProbeWatchdogRecord) (domain.ProbeWatchdogState, error) {
	if r.beforeCommit != nil {
		if err := r.beforeCommit(ctx); err != nil {
			return domain.ProbeWatchdogState{}, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return domain.ProbeWatchdogState{}, r.fail
	}
	if r.state.Version != v.ExpectedVersion {
		return domain.ProbeWatchdogState{}, ports.ErrStaleLocalState
	}
	if err := domain.ValidateProbeWatchdogCommit(a.ProbeID, r.state, v); err != nil {
		return domain.ProbeWatchdogState{}, err
	}
	r.state.Version++
	r.state.ConfigRevision, r.state.Checkpoint, r.state.Status = v.ConfigRevision, v.Checkpoint, v.Status
	if v.Incident != nil {
		inc := *v.Incident
		r.state.Incident = &inc
		r.state.IncidentSeq++
	}
	if len(v.DeliveryIntents) != 0 {
		at := v.At
		if r.state.LastEnqueuedAt != nil && r.state.LastEnqueuedAt.After(at) {
			at = *r.state.LastEnqueuedAt
		}
		r.state.LastEnqueuedAt = &at
		r.deliveries = append(r.deliveries, v.DeliveryIntents...)
	}
	return cloneWatchdogState(r.state), nil
}

func watchdogSourceFixture(t *testing.T) (*ProbeWatchdogSource, *watchdogSourceRepo, domain.ProbeWatchdogAuthority, *domain.EdgeResolvedConfig) {
	t.Helper()
	repo := &watchdogSourceRepo{}
	source, err := NewProbeWatchdogSource(repo, "hub")
	if err != nil {
		t.Fatal(err)
	}
	a := domain.ProbeWatchdogAuthority{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222", StreamID: "33333333-3333-4333-8333-333333333333", HealthGeneration: 1}
	c := &domain.EdgeResolvedConfig{Watchdog: domain.DefaultProbeWatchdogSettings(), Metadata: domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: a.HubID, ProbeID: a.ProbeID}, Revision: 1}, Channels: map[int64]domain.EdgeResolvedChannel{1: {Notification: &domain.Notification{ID: 1, Active: true}, Version: 1}}}
	c.Watchdog.Enabled, c.Watchdog.NotificationIDs, c.Watchdog.ResendInterval = true, []int64{1}, 1
	return source, repo, a, c
}

var watchdogTestEpoch = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

func watchdogInput(seconds int, health *bool) ProbeWatchdogInput {
	return ProbeWatchdogInput{Elapsed: time.Duration(seconds) * time.Second, At: watchdogTestEpoch.Add(time.Duration(seconds) * time.Second), Health: health}
}

func watchdogStep(t *testing.T, s *ProbeWatchdogSource, a domain.ProbeWatchdogAuthority, c *domain.EdgeResolvedConfig, seconds int, health *bool) domain.ProbeWatchdogState {
	t.Helper()
	v, err := s.Step(t.Context(), a, c, watchdogInput(seconds, health))
	if err != nil {
		t.Fatalf("step %d: %v", seconds, err)
	}
	return v
}

func watchdogAck(t *testing.T, r *watchdogSourceRepo) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	inc := *r.state.Incident
	inc.Status, inc.TransitionVersion = domain.AlertStatusAcked, inc.TransitionVersion+1
	at := watchdogTestEpoch.Add(100 * time.Second)
	inc.AckedAt, inc.AckCommandID, inc.AckActorDisplayName = &at, "44444444-4444-4444-8444-444444444444", "Operator"
	r.state.Incident = &inc
	r.state.Version++
	r.state.IncidentSeq++
}

func TestProbeWatchdogSourceLifecycleAndACK(t *testing.T) {
	s, r, a, c := watchdogSourceFixture(t)
	good, bad := true, false
	for _, seconds := range []int{0, 45, 89} {
		v := watchdogStep(t, s, a, c, seconds, nil)
		if v.Incident != nil || len(r.deliveries) != 0 {
			t.Fatal("loss opened before 90 seconds")
		}
	}
	opened := watchdogStep(t, s, a, c, 90, nil)
	if opened.Incident == nil || opened.Incident.Status != domain.AlertStatusFiring || len(r.deliveries) != 1 {
		t.Fatal("missing atomic opening", opened)
	}
	watchdogStep(t, s, a, c, 95, nil)
	if len(r.deliveries) != 1 {
		t.Fatal("resend throttle lost")
	}
	// A new applied channel version changes resend context, not outage identity.
	c.Metadata.Revision = 2
	ch := c.Channels[1]
	ch.Version = 2
	c.Channels[1] = ch
	resent := watchdogStep(t, s, a, c, 150, nil)
	if len(r.deliveries) != 2 || r.deliveries[1].NotificationVersion != 2 || resent.IncidentSeq != 1 || resent.Incident.SourceAlertID != opened.Incident.SourceAlertID {
		t.Fatal("resend created another transition")
	}
	watchdogStep(t, s, a, c, 165, &good)
	watchdogAck(t, r)
	watchdogStep(t, s, a, c, 180, &bad)
	for _, seconds := range []int{195, 210} {
		v := watchdogStep(t, s, a, c, seconds, &good)
		if v.Incident.Status != domain.AlertStatusAcked {
			t.Fatal("degraded frame did not interrupt recovery")
		}
	}
	resolved := watchdogStep(t, s, a, c, 225, &good)
	if resolved.Incident.Status != domain.AlertStatusResolved || resolved.Incident.TransitionVersion != 3 || resolved.Incident.AckCommandID == "" || resolved.Incident.SourceAlertID != opened.Incident.SourceAlertID || len(r.deliveries) != 3 || r.deliveries[2].SourceTransitionVersion != 3 {
		t.Fatal("ACK recovery lost identity or UP", resolved)
	}
	watchdogStep(t, s, a, c, 240, &good)
	if len(r.deliveries) != 3 {
		t.Fatal("duplicate recovery send")
	}
}

func TestProbeWatchdogSourceRejectedHealthCannotRecover(t *testing.T) {
	s, r, a, c := watchdogSourceFixture(t)
	good := true
	watchdogStep(t, s, a, c, 0, nil)
	watchdogStep(t, s, a, c, 90, nil)
	before := cloneWatchdogState(r.state)
	r.fail = ports.ErrConflict
	if _, err := s.Step(t.Context(), a, c, watchdogInput(105, &good)); !errors.Is(err, ports.ErrConflict) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, r.state) {
		t.Fatal("rejected transaction published state")
	}
	r.fail = nil
	for _, seconds := range []int{120, 135} {
		v := watchdogStep(t, s, a, c, seconds, &good)
		if v.Incident.Status != domain.AlertStatusFiring {
			t.Fatal("rejected health advanced recovery", seconds)
		}
	}
	if v := watchdogStep(t, s, a, c, 150, &good); v.Incident.Status != domain.AlertStatusResolved || len(r.deliveries) != 2 {
		t.Fatal("accepted 30-second recovery missing")
	}
}

func TestProbeWatchdogSourceOpeningRetryAndConcurrentACK(t *testing.T) {
	s, r, a, c := watchdogSourceFixture(t)
	watchdogStep(t, s, a, c, 0, nil)
	r.fail = ports.ErrConflict
	if _, err := s.Step(t.Context(), a, c, watchdogInput(90, nil)); err == nil {
		t.Fatal("fault accepted")
	}
	if r.state.Incident != nil || len(r.deliveries) != 0 {
		t.Fatal("partial opening")
	}
	r.fail = nil
	watchdogStep(t, s, a, c, 90, nil)
	good := true
	watchdogStep(t, s, a, c, 105, &good)
	watchdogStep(t, s, a, c, 120, &good)
	first := true
	r.beforeCommit = func(context.Context) error {
		if first {
			first = false
			watchdogAck(t, r)
		}
		return nil
	}
	v := watchdogStep(t, s, a, c, 135, &good)
	if v.Incident.Status != domain.AlertStatusResolved || v.Incident.TransitionVersion != 3 || v.Incident.AckCommandID == "" || len(r.deliveries) != 2 {
		t.Fatal("CAS retry lost ACK or committed duplicate effects")
	}
}

func TestProbeWatchdogSourceDisableRestartAndClockRollback(t *testing.T) {
	s, r, a, c := watchdogSourceFixture(t)
	good := true
	watchdogStep(t, s, a, c, 0, nil)
	opened := watchdogStep(t, s, a, c, 90, nil)
	watchdogStep(t, s, a, c, 105, &good)
	watchdogAck(t, r)
	// A fresh process must rebuild its healthy streak and retain the same incident.
	s, _ = NewProbeWatchdogSource(r, "hub")
	for _, seconds := range []int{0, 15, 30} {
		in := watchdogInput(seconds, &good)
		in.At = watchdogTestEpoch.Add(-time.Hour)
		v, err := s.Step(t.Context(), a, c, in)
		if err != nil {
			t.Fatal(err)
		}
		if v.Incident.SourceAlertID != opened.Incident.SourceAlertID || seconds < 30 && v.Incident.Status != domain.AlertStatusAcked {
			t.Fatal("restart inherited recovery or changed identity")
		}
		if seconds == 30 && v.Incident.Status != domain.AlertStatusResolved {
			t.Fatal("monotonic recovery broken by wall clock rollback")
		}
	}
	watchdogStep(t, s, a, c, 120, nil)
	watchdogAck(t, r)
	c.Watchdog.Enabled = false
	disabled := watchdogStep(t, s, a, c, 121, nil)
	if disabled.Status != domain.ProbeWatchdogUnarmed || disabled.Incident.Status != domain.AlertStatusResolved || disabled.Incident.AckCommandID == "" || len(r.deliveries) != 3 {
		t.Fatal("disable manufactured healthy delivery", disabled, len(r.deliveries))
	}
	c.Watchdog.Enabled = true
	watchdogStep(t, s, a, c, 500, nil)
	watchdogStep(t, s, a, c, 589, nil)
	if len(r.deliveries) != 3 {
		t.Fatal("re-enable inherited elapsed outage")
	}
	watchdogStep(t, s, a, c, 590, nil)
	if len(r.deliveries) != 4 {
		t.Fatal("fresh grace did not expire")
	}
}

func TestProbeWatchdogSourceRestartRetainsMeasuredLoss(t *testing.T) {
	s, r, a, c := watchdogSourceFixture(t)
	watchdogStep(t, s, a, c, 0, nil)
	watchdogStep(t, s, a, c, 60, nil)
	s, _ = NewProbeWatchdogSource(r, "hub")
	for _, seconds := range []int{0, 29} {
		if v := watchdogStep(t, s, a, c, seconds, nil); v.Incident != nil {
			t.Fatal("uncertain restart gap counted as loss")
		}
	}
	if v := watchdogStep(t, s, a, c, 30, nil); v.Incident == nil || len(r.deliveries) != 1 {
		t.Fatal("measured checkpoint lost on restart")
	}
}
