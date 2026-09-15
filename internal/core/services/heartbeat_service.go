package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// certAlertEvaluator evaluates certificate-expiry alerts after a check.
// Satisfied by *CertificateAlertService. Optional.
type certAlertEvaluator interface {
	OnCheck(ctx context.Context, monitor *domain.Monitor, metadata map[string]string)
}

type monitorConditionEvaluator interface {
	OnCheck(ctx context.Context, monitor *domain.Monitor, observations []domain.ConditionObservation)
}

// HeartbeatService handles heartbeat recording and status transition evaluation.
type HeartbeatService struct {
	heartbeats  ports.HeartbeatRepository
	bus         ports.EventBus
	dispatcher  ports.NotificationDispatcher
	tlsInfo     ports.TLSInfoRepository
	certAlert   certAlertEvaluator
	conditions  monitorConditionEvaluator
	assignments ports.MonitorProbeAssignmentRepository
	regional    ports.RegionalCommitRepository
}

// NewHeartbeatService creates a new HeartbeatService.
func NewHeartbeatService(heartbeats ports.HeartbeatRepository, bus ports.EventBus) *HeartbeatService {
	return &HeartbeatService{heartbeats: heartbeats, bus: bus}
}

// SetDispatcher attaches a notification dispatcher so confirmed status
// transitions fire alerts. Optional: when nil, no notifications are dispatched
// (e.g. in API-only mode or in tests).
func (s *HeartbeatService) SetDispatcher(d ports.NotificationDispatcher) {
	s.dispatcher = d
}

// SetTLSInfoRepo attaches a TLS info repository so HTTPS checks can persist
// certificate metadata. Optional: when nil, TLS info is not stored.
func (s *HeartbeatService) SetTLSInfoRepo(repo ports.TLSInfoRepository) {
	s.tlsInfo = repo
}

// SetCertAlert attaches a certificate-expiry alert evaluator. Optional: when
// nil, no certificate alerts fire. Runs in the owning worker after TLS info is
// persisted so threshold state is available across restarts.
func (s *HeartbeatService) SetCertAlert(e certAlertEvaluator) {
	s.certAlert = e
}

// SetConditionEvaluator attaches auxiliary condition persistence and alerting.
// It runs in the owning worker, like certificate alerts, to avoid Redis fan-out
// duplicates in split mode.
func (s *HeartbeatService) SetConditionEvaluator(e monitorConditionEvaluator) {
	s.conditions = e
}

// SetRegionalRecorder attaches local assignment/state persistence. Optional:
// when nil, Record keeps today's heartbeat-only path so tests and API-only
// mode stay unchanged. It does not dispatch regional incidents.
func (s *HeartbeatService) SetRegionalRecorder(assignments ports.MonitorProbeAssignmentRepository, regional ports.RegionalCommitRepository) {
	s.assignments = assignments
	s.regional = regional
}

// Record saves a heartbeat from a check result and evaluates status transitions.
// It fetches the previous heartbeat before saving to detect status changes.
// DownCount is incremented on DOWN status and reset to 0 on UP status.
func (s *HeartbeatService) Record(ctx context.Context, monitor *domain.Monitor, result ports.CheckResult) error {
	// Fetch the previous heartbeat BEFORE saving so we can compare status.
	prevHB, prevErr := s.heartbeats.GetLatest(ctx, monitor.ID)

	latency := clampLatencyMs(result.LatencyMs)
	duration := clampLatencyMs(result.DurationMs)
	if duration == 0 {
		duration = latency
	}
	now := time.Now().UTC()
	hb := &domain.Heartbeat{
		MonitorID:  monitor.ID,
		ProbeID:    domain.LocalProbeID,
		Status:     result.Status,
		Time:       now,
		ReceivedAt: now,
		Msg:        result.Message,
		Ping:       latency,
		Duration:   duration,
	}

	var previous *domain.RetryState
	var oldStatus *domain.Status
	if prevErr == nil && prevHB != nil {
		previous = &domain.RetryState{Status: prevHB.Status, DownCount: prevHB.DownCount}
		oldStatus = &prevHB.Status
	}
	generation := int64(0)
	seq := int64(1)
	if state, gen, err := s.localRegionalState(ctx, monitor.ID); err != nil {
		return err
	} else if state != nil {
		previous = &domain.RetryState{Status: state.Status, DownCount: state.DownCount}
		oldStatus = &state.Status
		generation = state.AssignmentGeneration
		if state.Seq >= math.MaxInt64 {
			return fmt.Errorf("local stream sequence exhausted: %w", ports.ErrConflict)
		}
		seq = state.Seq + 1
	} else {
		generation = gen
	}
	evaluation := EvaluateRetry(previous, result.Status, monitor.MaxRetries)
	hb.Status = evaluation.State.Status
	hb.DownCount = evaluation.State.DownCount
	hb.Important = evaluation.Important
	if generation > 0 {
		hb.AssignmentGeneration = generation
		hb.StreamID = domain.LocalStreamID
		hb.SourceSeq = seq
	}
	transitioned := evaluation.Important

	// Save heartbeat. Return error if save fails (don't suppress).
	if err := s.heartbeats.Save(ctx, hb); err != nil {
		return fmt.Errorf("heartbeat service: save: %w", err)
	}
	if err := s.commitLocalRegional(ctx, monitor.ID, result.Status, hb); err != nil {
		return fmt.Errorf("heartbeat service: regional commit: %w", err)
	}

	// Persist TLS certificate info when present (best-effort).
	if s.tlsInfo != nil {
		s.persistTLSInfo(ctx, monitor.ID, result.Metadata)
	}

	// Certificate-expiry alerts (opt-in). Best-effort; never fail the heartbeat.
	// Runs in the owning worker so Redis EventBus fan-out cannot duplicate them.
	if s.certAlert != nil {
		s.certAlert.OnCheck(ctx, monitor, result.Metadata)
	}

	// Auxiliary conditions are persisted and notified independently from the
	// heartbeat status so capacity pressure never becomes fake downtime.
	if s.conditions != nil {
		s.conditions.OnCheck(ctx, monitor, result.Conditions)
	}

	// Publish heartbeat event (best-effort — never fail on bus.Publish).
	_ = s.bus.Publish(ctx, ports.Event{Type: "heartbeat", Payload: hb})

	// Publish status.change on first check and on every effective transition so the
	// dashboard moves off "pending" without requiring a prior heartbeat.
	var prev domain.Status
	if oldStatus != nil {
		prev = *oldStatus
	}
	if transitioned {
		_ = s.bus.Publish(ctx, ports.Event{
			Type: "status.change",
			Payload: map[string]any{
				"monitor_id": monitor.ID,
				"old_status": prev,
				"new_status": hb.Status,
				"monitor":    monitor,
			},
		})
	}

	// Hand the heartbeat to the notification dispatcher (when wired). It decides
	// whether to alert — applying maintenance suppression, confirmed-transition
	// gating, and resend throttling. Called on every heartbeat (not just
	// transitions) so resend-while-down works. Runs in the monitor's owning
	// worker, so each transition alerts exactly once.
	if s.dispatcher != nil {
		s.dispatcher.OnHeartbeat(ctx, monitor, hb, oldStatus)
	}

	return nil
}

// ListByMonitor returns heartbeats for a monitor within a time range.
//
// The bounds are normalized to UTC because heartbeats are stored with a UTC
// wall-clock (see Record). A local-zoned bound would be written into the SQL as
// its local wall-clock, shifting the window by the server's UTC offset and
// silently hiding recent heartbeats. Normalizing here means a caller that passes
// time.Now() instead of time.Now().UTC() still gets the right rows.
func (s *HeartbeatService) ListByMonitor(ctx context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
	return s.heartbeats.ListByMonitor(ctx, monitorID, from.UTC(), to.UTC())
}

// ListRecentByMonitor returns the newest `limit` heartbeats in [from, to].
//
// Prefers HeartbeatRecentReader so the database applies LIMIT. Test fakes that
// only implement ListByMonitor still work: we load the window and truncate
// here, newest first, with the id tie-break.
func (s *HeartbeatService) ListRecentByMonitor(ctx context.Context, monitorID int64, from, to time.Time, limit int) ([]*domain.Heartbeat, error) {
	from, to = from.UTC(), to.UTC()
	if limit <= 0 {
		return s.heartbeats.ListByMonitor(ctx, monitorID, from, to)
	}
	if recent, ok := s.heartbeats.(ports.HeartbeatRecentReader); ok {
		return recent.ListRecentByMonitor(ctx, monitorID, from, to, limit)
	}
	all, err := s.heartbeats.ListByMonitor(ctx, monitorID, from, to)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Time.Equal(all[j].Time) {
			return all[i].ID > all[j].ID
		}
		return all[i].Time.After(all[j].Time)
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// ClearHistory deletes all heartbeats for a monitor.
func (s *HeartbeatService) ClearHistory(ctx context.Context, monitorID int64) error {
	if err := s.heartbeats.DeleteByMonitor(ctx, monitorID); err != nil {
		return fmt.Errorf("heartbeat service: clear history: %w", err)
	}
	return nil
}

// DeleteOlderThan removes heartbeats older than before.
//
// The cutoff is normalized to UTC here so a caller that passes a local-zoned
// time cannot delete rows newer than intended (AGENTS.md rule 6). Repos also
// force .UTC() at the DB boundary as a second line of defense.
func (s *HeartbeatService) DeleteOlderThan(ctx context.Context, before time.Time) error {
	if err := s.heartbeats.DeleteOlderThan(ctx, before.UTC()); err != nil {
		return fmt.Errorf("heartbeat service: delete older than: %w", err)
	}
	return nil
}

func (s *HeartbeatService) localRegionalState(ctx context.Context, monitorID int64) (*domain.RegionalState, int64, error) {
	if s.regional == nil {
		return nil, 0, nil
	}
	generation := int64(1)
	if s.assignments != nil {
		set, err := s.assignments.GetByMonitorID(ctx, monitorID)
		if errors.Is(err, ports.ErrNotFound) {
			return nil, 0, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("load local assignment: %w", err)
		}
		found := false
		for _, assignment := range set.Assignments {
			if assignment.ProbeID == domain.LocalProbeID {
				generation = assignment.Generation
				found = true
				break
			}
		}
		if !found {
			return nil, 0, nil
		}
	}
	state, err := s.regional.GetState(ctx, monitorID, domain.LocalProbeID)
	if errors.Is(err, ports.ErrNotFound) {
		return nil, generation, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("load local regional state: %w", err)
	}
	return state, generation, nil
}

func (s *HeartbeatService) commitLocalRegional(ctx context.Context, monitorID int64, raw domain.Status, hb *domain.Heartbeat) error {
	if s.regional == nil || hb.AssignmentGeneration < 1 || hb.StreamID == "" || hb.SourceSeq < 1 {
		return nil
	}
	obs := domain.RegionalObservation{
		MonitorID:            monitorID,
		ProbeID:              domain.LocalProbeID,
		AssignmentGeneration: hb.AssignmentGeneration,
		StreamID:             hb.StreamID,
		Seq:                  hb.SourceSeq,
		ConfigRevision:       1,
		Status:               hb.Status,
		RawStatus:            raw,
		DownCount:            hb.DownCount,
		Ping:                 hb.Ping,
		DurationMS:           hb.Duration,
		Message:              hb.Msg,
		Important:            hb.Important,
		ObservedAt:           hb.Time.UTC(),
		ReceivedAt:           hb.ReceivedAt.UTC(),
	}
	state := domain.RegionalState{
		MonitorID:            obs.MonitorID,
		ProbeID:              obs.ProbeID,
		AssignmentGeneration: obs.AssignmentGeneration,
		StreamID:             obs.StreamID,
		Seq:                  obs.Seq,
		ConfigRevision:       obs.ConfigRevision,
		Status:               obs.Status,
		DownCount:            obs.DownCount,
		ObservedAt:           obs.ObservedAt,
		ReceivedAt:           obs.ReceivedAt,
	}
	if obs.Status == domain.StatusUp {
		t := obs.ObservedAt
		state.LastSuccessAt = &t
	}
	return s.regional.Commit(ctx, domain.RegionalCommit{Observation: obs, State: state})
}

func (s *HeartbeatService) persistTLSInfo(ctx context.Context, monitorID int64, metadata map[string]string) {
	if metadata == nil {
		return
	}
	daysStr, ok := metadata["tls_days_remaining"]
	if !ok || daysStr == "" {
		return
	}
	days, err := strconv.Atoi(daysStr)
	if err != nil {
		return
	}
	now := time.Now().UTC()

	// Prefer the exact certificate NotAfter instant from the checker. Reconstructing
	// by adding rounded whole days to "now" loses the real wall-clock and breaks
	// renewal detection for certificate-expiry alerts.
	var notAfter time.Time
	if raw, has := metadata["tls_not_after"]; has && raw != "" {
		if t, perr := time.Parse(time.RFC3339, raw); perr == nil {
			notAfter = t.UTC()
		} else if t, perr := time.Parse(time.RFC3339Nano, raw); perr == nil {
			notAfter = t.UTC()
		}
	}
	if notAfter.IsZero() {
		// Legacy metadata without tls_not_after — last-resort reconstruction.
		notAfter = now.AddDate(0, 0, days)
	}

	info := &ports.TLSInfo{
		MonitorID:     monitorID,
		DaysRemaining: days,
		NotAfter:      notAfter,
		Issuer:        metadata["tls_issuer"],
		CheckedAt:     now,
	}

	// Preserve certificate-alert threshold state across ordinary heartbeat
	// upserts. Without this, every check would wipe last_cert_alert_* and
	// CertificateAlertService would re-fire the same threshold forever.
	if prev, gerr := s.tlsInfo.GetByMonitorID(ctx, monitorID); gerr == nil && prev != nil {
		if !prev.LastCertAlertNotAfter.IsZero() && prev.LastCertAlertNotAfter.UTC().Equal(notAfter) {
			info.LastCertAlertThreshold = prev.LastCertAlertThreshold
			info.LastCertAlertNotAfter = prev.LastCertAlertNotAfter
		}
		// Different NotAfter (renewal): leave alert fields zero so the next
		// CertificateAlertService evaluation starts fresh.
	}

	_ = s.tlsInfo.Upsert(ctx, info)
}

// clampLatencyMs converts a check latency (int64 ms) into the domain Heartbeat
// int field without overflowing on 32-bit platforms (or absurd values).
// Negative latencies are treated as "not measured" (0).
func clampLatencyMs(ms int64) int {
	if ms <= 0 {
		return 0
	}
	if ms > math.MaxInt {
		return math.MaxInt
	}
	return int(ms)
}
