package services

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Fixed certificate-expiry thresholds (days). Not user-configurable in Sprint C.
var certExpiryThresholds = []int{30, 14, 7}

// certAlertNotifier dispatches a pre-built certificate AlertContext to the
// monitor's attached providers. Satisfied by *NotificationService.
type certAlertNotifier interface {
	Dispatch(ctx context.Context, monitor *domain.Monitor, alert domain.AlertContext) error
}

// CertificateAlertService evaluates HTTPS check metadata and fires once-per-
// threshold certificate-expiry alerts. It runs in the owning heartbeat worker
// (called from HeartbeatService.Record), never as an EventBus subscriber, so
// Redis fan-out cannot duplicate alerts under split mode.
//
// Alert state (last threshold + certificate NotAfter) is persisted inside
// TLSInfo so a worker restart does not re-send the same threshold.
type CertificateAlertService struct {
	tlsInfo     ports.TLSInfoRepository
	notifier    certAlertNotifier
	maintenance maintenanceChecker
	now         func() time.Time
}

// NewCertificateAlertService creates a certificate-expiry alert evaluator.
func NewCertificateAlertService(
	tlsInfo ports.TLSInfoRepository,
	notifier certAlertNotifier,
	maintenance maintenanceChecker,
) *CertificateAlertService {
	return &CertificateAlertService{
		tlsInfo:     tlsInfo,
		notifier:    notifier,
		maintenance: maintenance,
		now:         time.Now,
	}
}

// OnCheck evaluates TLS metadata from a check result and may dispatch a
// certificate-expiry alert. Failures are logged; they never fail the heartbeat.
//
// Lifecycle (handoff §3.5):
//   - opt-in via monitor.CertExpiryNotify
//   - exact NotAfter from metadata (never reconstruct from rounded days)
//   - thresholds 30/14/7; alert the most urgent applicable once
//   - persist last sent threshold + NotAfter; renewal resets
//   - maintenance suppresses send AND does not mark threshold sent
//   - only notifications attached to the monitor are used
func (s *CertificateAlertService) OnCheck(ctx context.Context, monitor *domain.Monitor, metadata map[string]string) {
	if s == nil || monitor == nil || !monitor.CertExpiryNotify {
		return
	}
	if s.tlsInfo == nil || s.notifier == nil {
		return
	}
	if metadata == nil {
		return
	}

	notAfter, days, issuer, ok := parseCertMetadata(metadata, s.now())
	if !ok {
		return
	}

	threshold, has := mostUrgentThreshold(days)
	if !has {
		// Still refresh the cached TLS row's non-alert fields via HeartbeatService;
		// no alert work needed above 30 days.
		return
	}

	// Maintenance: do not send and do not mark the threshold as sent.
	if s.maintenance != nil {
		if active, err := s.maintenance.IsActive(ctx, monitor.ID); err != nil {
			slog.Warn("certificate alert: maintenance check failed, continuing",
				"monitor_id", monitor.ID, "error", err)
		} else if active {
			return
		}
	}

	existing, err := s.tlsInfo.GetByMonitorID(ctx, monitor.ID)
	if err != nil && err != ports.ErrNotFound && err != domain.ErrNotFound {
		slog.Warn("certificate alert: load TLS info failed",
			"monitor_id", monitor.ID, "error", err)
		// Fall through with empty prior state rather than drop the alert entirely.
		existing = nil
	} else if err != nil {
		existing = nil
	}

	lastThreshold := 0
	if existing != nil {
		// Renewal: a different NotAfter resets threshold state.
		if !existing.LastCertAlertNotAfter.IsZero() &&
			!existing.LastCertAlertNotAfter.UTC().Equal(notAfter.UTC()) {
			lastThreshold = 0
		} else {
			lastThreshold = existing.LastCertAlertThreshold
		}
	}

	// Already sent this threshold (or a more urgent one) for this certificate.
	// More urgent = smaller threshold number. Sending 7 covers 14/30 for this cert.
	if lastThreshold > 0 && threshold >= lastThreshold {
		return
	}

	alert := domain.AlertContext{
		AlertScope:         domain.AlertScopeMonitor,
		MonitorID:          monitor.ID,
		MonitorName:        monitor.Name,
		MonitorType:        monitor.Type,
		MonitorTarget:      monitor.Target(),
		MonitorDescription: monitor.Description,
		MonitorOwner:       monitor.Owner,
		EventKind:          domain.AlertEventCertificateExpiry,
		Message:            formatCertExpiryMessage(monitor.Name, threshold, days, issuer, notAfter),
		CertThreshold:      threshold,
		CertDaysRemaining:  days,
		CertIssuer:         issuer,
		CertNotAfter:       &notAfter,
		StartedAt:          s.now().UTC(),
	}

	if err := s.notifier.Dispatch(ctx, monitor, alert); err != nil {
		slog.Error("certificate alert: dispatch failed",
			"monitor_id", monitor.ID, "threshold", threshold, "error", err)
		// Do not mark sent — a later check can retry the same threshold.
		return
	}

	// Persist threshold state on top of the current cert metadata.
	info := &ports.TLSInfo{
		MonitorID:              monitor.ID,
		DaysRemaining:          days,
		NotAfter:               notAfter,
		Issuer:                 issuer,
		CheckedAt:              s.now().UTC(),
		LastCertAlertThreshold: threshold,
		LastCertAlertNotAfter:  notAfter,
	}
	// Preserve prior issuer/days if we somehow lost them (should not happen).
	if existing != nil && info.Issuer == "" {
		info.Issuer = existing.Issuer
	}
	if err := s.tlsInfo.Upsert(ctx, info); err != nil {
		// Alert already went out; log so operators can inspect. A restart may
		// re-send once — acceptable vs losing the alert on a write failure.
		slog.Error("certificate alert: persist threshold state failed",
			"monitor_id", monitor.ID, "threshold", threshold, "error", err)
	}
}

// mostUrgentThreshold returns the tightest threshold that daysRemaining has
// entered. days=20 → 30; days=13 → 14; days=5 → 7; days=45 → none.
func mostUrgentThreshold(daysRemaining int) (int, bool) {
	// Walk from most urgent to least so the first hit is the right one.
	for _, t := range []int{7, 14, 30} {
		if daysRemaining <= t {
			return t, true
		}
	}
	return 0, false
}

func parseCertMetadata(metadata map[string]string, now time.Time) (notAfter time.Time, days int, issuer string, ok bool) {
	issuer = metadata["tls_issuer"]

	// Prefer exact NotAfter. Never reconstruct by adding rounded days to now
	// when the checker provides the real instant.
	if raw, has := metadata["tls_not_after"]; has && raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			// Try RFC3339Nano as a fallback.
			t, err = time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return time.Time{}, 0, "", false
			}
		}
		notAfter = t.UTC()
	} else {
		return time.Time{}, 0, "", false
	}

	if daysStr, has := metadata["tls_days_remaining"]; has && daysStr != "" {
		d, err := strconv.Atoi(daysStr)
		if err != nil {
			// Derive whole days from NotAfter if the days field is corrupt.
			d = int(notAfter.Sub(now.UTC()).Hours() / 24)
			if d < 0 {
				d = 0
			}
		}
		days = d
	} else {
		d := int(notAfter.Sub(now.UTC()).Hours() / 24)
		if d < 0 {
			d = 0
		}
		days = d
	}
	return notAfter, days, issuer, true
}

func formatCertExpiryMessage(name string, threshold, days int, issuer string, notAfter time.Time) string {
	msg := fmt.Sprintf(
		"TLS certificate for %s expires in %d day(s) (threshold: %d days, not after %s)",
		name, days, threshold, notAfter.UTC().Format(time.RFC3339),
	)
	if issuer != "" {
		msg += fmt.Sprintf("; issuer: %s", issuer)
	}
	return msg
}

// Ensure unused threshold table is referenced for documentation/tests.
var _ = certExpiryThresholds
