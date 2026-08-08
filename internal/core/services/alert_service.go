package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AlertService owns the monitor alert lifecycle (F2.2): open on confirmed DOWN,
// acknowledge from UI/API/deep-link, resolve on recovery, and answer the
// dispatcher's "is this still-DOWN resend suppressed?" question.
type AlertService struct {
	repo       ports.AlertRepository
	escalation escalationCanceller // optional — F2.3
	now        func() time.Time
}

// escalationCanceller stops a pending escalation ladder. Satisfied by
// *EscalationService. Optional — when nil, F2.3 is simply not installed.
type escalationCanceller interface {
	CancelForAlert(ctx context.Context, alertID int64) error
}

// NewAlertService creates an AlertService backed by the given repository.
func NewAlertService(repo ports.AlertRepository) *AlertService {
	return &AlertService{
		repo: repo,
		now:  time.Now,
	}
}

// SetEscalationCanceller wires F2.3 escalation cancellation. Both acknowledgement
// and resolution cancel the ladder, and they do it HERE rather than in each
// caller: ack arrives from the admin API, from the public deep link, and
// resolution from the heartbeat path, and a cancellation forgotten at any one of
// those sites would keep paging a human who has already responded.
func (s *AlertService) SetEscalationCanceller(c escalationCanceller) {
	s.escalation = c
}

// cancelEscalation stops the ladder for an alert. Failure is logged, never
// propagated: the ack or resolve itself has already been persisted and must not
// be reported as failed because a follow-up write did not land. The runner's
// own re-read of the alert status is the backstop — it refuses to send a step
// for an alert that is no longer firing.
func (s *AlertService) cancelEscalation(ctx context.Context, alertID int64) {
	if s.escalation == nil {
		return
	}
	if err := s.escalation.CancelForAlert(ctx, alertID); err != nil {
		slog.Error("alert service: cancel escalation failed", "alert_id", alertID, "error", err)
	}
}

// OpenOnDown ensures a firing alert exists for the monitor. If one is already
// open (firing or acked), it is returned unchanged — acknowledgement must not
// be clobbered by a resend, and a second DOWN after a partial race is a no-op.
//
// A UNIQUE conflict on open_monitor_id (two workers opening the same outage)
// is recovered by re-reading the winner's row.
func (s *AlertService) OpenOnDown(ctx context.Context, monitor *domain.Monitor, at time.Time) (*domain.Alert, error) {
	if s == nil || s.repo == nil || monitor == nil {
		return nil, fmt.Errorf("alert service: open: not configured")
	}
	at = at.UTC()

	open, err := s.repo.GetOpenByMonitorID(ctx, monitor.ID)
	if err == nil && open != nil {
		return open, nil
	}
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return nil, fmt.Errorf("alert service: get open: %w", err)
	}

	token, err := newAckToken()
	if err != nil {
		return nil, fmt.Errorf("alert service: ack token: %w", err)
	}
	mid := monitor.ID
	a := &domain.Alert{
		MonitorID:     monitor.ID,
		Status:        domain.AlertStatusFiring,
		Message:       fmt.Sprintf("%s is DOWN", monitor.Name),
		FiredAt:       at,
		AckToken:      token,
		OpenMonitorID: &mid,
	}
	if err := s.repo.Create(ctx, a); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			// Lost the open race — return the winner.
			won, getErr := s.repo.GetOpenByMonitorID(ctx, monitor.ID)
			if getErr != nil {
				return nil, fmt.Errorf("alert service: open race recovery: %w", getErr)
			}
			return won, nil
		}
		return nil, fmt.Errorf("alert service: create: %w", err)
	}
	return a, nil
}

// ResolveOpen marks the monitor's open alert resolved. Missing open alerts are
// a no-op (e.g. the original DOWN was suppressed by maintenance).
func (s *AlertService) ResolveOpen(ctx context.Context, monitorID int64, at time.Time) error {
	_, err := s.ResolveOpenWithAlert(ctx, monitorID, at)
	return err
}

// ResolveOpenWithAlert resolves the open alert and returns the lifecycle row
// that was closed. NotificationDispatcher uses FiredAt from this row to expose
// truthful outage start and duration variables on recovery notifications.
// Missing open alerts return (nil, nil).
func (s *AlertService) ResolveOpenWithAlert(ctx context.Context, monitorID int64, at time.Time) (*domain.Alert, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	at = at.UTC()
	open, err := s.repo.GetOpenByMonitorID(ctx, monitorID)
	if errors.Is(err, ports.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("alert service: resolve get: %w", err)
	}
	open.Status = domain.AlertStatusResolved
	open.ResolvedAt = &at
	open.OpenMonitorID = nil
	open.UpdatedAt = at
	if err := s.repo.Update(ctx, open); err != nil {
		return nil, fmt.Errorf("alert service: resolve update: %w", err)
	}
	s.cancelEscalation(ctx, open.ID)
	return open, nil
}

// IsOpenAcked reports whether the monitor has an open alert in the acked state.
// Used by the dispatcher to suppress resend notifications.
func (s *AlertService) IsOpenAcked(ctx context.Context, monitorID int64) (bool, error) {
	if s == nil || s.repo == nil {
		return false, nil
	}
	open, err := s.repo.GetOpenByMonitorID(ctx, monitorID)
	if errors.Is(err, ports.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("alert service: is acked: %w", err)
	}
	return open.Status == domain.AlertStatusAcked, nil
}

// Acknowledge moves a firing alert to acked. Already-acked is idempotent.
// Resolved alerts cannot be re-acked (ErrConflict). userID may be nil when the
// caller is the unauthenticated deep-link path.
func (s *AlertService) Acknowledge(ctx context.Context, id int64, userID *int64) (*domain.Alert, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("alert service: acknowledge: not configured")
	}
	a, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.ackAlert(ctx, a, userID)
}

// AcknowledgeByToken acknowledges via the deep-link token. Returns ErrNotFound
// for unknown tokens and for tokens belonging to already-resolved alerts (so a
// reused link cannot re-open state).
func (s *AlertService) AcknowledgeByToken(ctx context.Context, token string) (*domain.Alert, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("alert service: acknowledge by token: not configured")
	}
	if token == "" {
		return nil, domain.ErrValidation
	}
	a, err := s.repo.GetByAckToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if a.Status == domain.AlertStatusResolved {
		// Do not confirm the token belonged to a real alert once resolved —
		// the link is spent. Callers still get a clean "not found" rather than
		// a conflict that teaches attackers which tokens were once valid.
		return nil, ports.ErrNotFound
	}
	return s.ackAlert(ctx, a, nil)
}

func (s *AlertService) ackAlert(ctx context.Context, a *domain.Alert, userID *int64) (*domain.Alert, error) {
	switch a.Status {
	case domain.AlertStatusAcked:
		return a, nil
	case domain.AlertStatusResolved:
		return nil, domain.ErrConflict
	case domain.AlertStatusFiring:
		// proceed
	default:
		return nil, domain.ErrValidation
	}
	now := s.now().UTC()
	a.Status = domain.AlertStatusAcked
	a.AckedAt = &now
	a.AckedByUserID = userID
	a.UpdatedAt = now
	// OpenMonitorID stays set — the outage is still open, just acked.
	if err := s.repo.Update(ctx, a); err != nil {
		return nil, fmt.Errorf("alert service: acknowledge update: %w", err)
	}
	// Acked means STOP ESCALATING, not resolved. Only the ladder is canceled;
	// the outage stays open and resend suppression stays in force (handoff §4.8).
	s.cancelEscalation(ctx, a.ID)
	return a, nil
}

// GetByID returns one alert by primary key.
func (s *AlertService) GetByID(ctx context.Context, id int64) (*domain.Alert, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("alert service: get: not configured")
	}
	return s.repo.GetByID(ctx, id)
}

// List returns alerts matching the filter, newest first.
func (s *AlertService) List(ctx context.Context, filter ports.AlertFilter) ([]*domain.Alert, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("alert service: list: not configured")
	}
	return s.repo.List(ctx, filter)
}

func newAckToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
