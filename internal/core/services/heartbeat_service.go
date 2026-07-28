package services

import (
	"context"
	"fmt"
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

// HeartbeatService handles heartbeat recording and status transition evaluation.
type HeartbeatService struct {
	heartbeats ports.HeartbeatRepository
	bus        ports.EventBus
	dispatcher ports.NotificationDispatcher
	tlsInfo    ports.TLSInfoRepository
	certAlert  certAlertEvaluator
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

// Record saves a heartbeat from a check result and evaluates status transitions.
// It fetches the previous heartbeat before saving to detect status changes.
// DownCount is incremented on DOWN status and reset to 0 on UP status.
func (s *HeartbeatService) Record(ctx context.Context, monitor *domain.Monitor, result ports.CheckResult) error {
	// Fetch the previous heartbeat BEFORE saving so we can compare status.
	prevHB, prevErr := s.heartbeats.GetLatest(ctx, monitor.ID)

	hb := &domain.Heartbeat{
		MonitorID: monitor.ID,
		Status:    result.Status,
		Time:      time.Now().UTC(),
		Msg:       result.Message,
		Ping:      int(result.LatencyMs),
		Duration:  int(result.LatencyMs),
	}

	// Compute DownCount based on status transition.
	var prevDownCount int
	var oldStatus *domain.Status
	if prevErr == nil && prevHB != nil {
		prevDownCount = prevHB.DownCount
		oldStatus = &prevHB.Status
	}

	if result.Status == domain.StatusDown {
		hb.DownCount = prevDownCount + 1
		// Retry-confirm: while still inside the retry window, report the check as
		// PENDING rather than DOWN so a transient failure doesn't immediately fire
		// alerts. The monitor only transitions to confirmed DOWN once it has failed
		// more than MaxRetries consecutive times. MaxRetries == 0 means no retries
		// (fail immediately), preserving the original behavior.
		if monitor.MaxRetries > 0 && hb.DownCount <= monitor.MaxRetries {
			hb.Status = domain.StatusPending
		}
	} else {
		hb.DownCount = 0
	}

	// A transition is any change in effective status (including the first check).
	// Mark such heartbeats "important" so the UI and history can highlight them.
	transitioned := oldStatus == nil || *oldStatus != hb.Status
	hb.Important = transitioned

	// Save heartbeat. Return error if save fails (don't suppress).
	if err := s.heartbeats.Save(ctx, hb); err != nil {
		return fmt.Errorf("heartbeat service: save: %w", err)
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
