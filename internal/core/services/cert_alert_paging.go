package services

import (
	"fmt"
	"math"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// CertAlertPagingInput is one trusted assignment's evaluation context. The
// caller supplies already-authorized state: the accepted configuration graph,
// the durable cursor and open incident read alongside it in the same
// transaction, and the maintenance verdict resolved from the windows named by
// that assignment. Nothing here is inferred from hub-side mirror rows.
type CertAlertPagingInput struct {
	Config      *domain.EdgeResolvedConfig
	Monitor     *domain.Monitor
	Assignment  domain.EdgeResolvedAssignment
	Observation domain.RegionalObservation
	Prior       *domain.EdgeCertAlertState
	PriorCursor *domain.RegionalIncident
	Maintenance bool
	Now         time.Time
	NewID       func() (string, error)
}

// EvaluateCertAlertPaging decides the durable certificate paging work for one
// source observation without performing any I/O. It is the remote counterpart of
// CertificateAlertService.onCheck and keeps the same delivery rules:
//
//   - opt-in through monitor.CertExpiryNotify only
//   - exact certificate NotAfter required; rounded days never identify a cert
//   - thresholds 30/14/7, firing the most urgent crossed threshold once
//   - a renewed certificate resets the cursor and retires the stale incident
//   - maintenance sends nothing, retires nothing and marks nothing as sent
//
// A nil result means the observation requires no stored change, so a steady
// state check never advances the cursor fence. When work is returned, callers
// must persist and emit Transitions in order: an administrative retirement
// always precedes the new firing incident, and every intent references the
// final transition.
func EvaluateCertAlertPaging(in CertAlertPagingInput) (*domain.EdgeCertAlertWork, error) {
	if in.Config == nil || in.Monitor == nil || in.Monitor.ID <= 0 || in.Assignment.Generation <= 0 || in.Now.IsZero() || in.NewID == nil {
		return nil, fmt.Errorf("certificate paging requires a config graph, monitor, assignment, clock and ID source: %w", domain.ErrValidation)
	}
	if !in.Monitor.CertExpiryNotify {
		return nil, nil
	}
	evidence := in.Observation.TLS
	if !domain.ValidTLSObservation(evidence) {
		return nil, fmt.Errorf("certificate paging observation evidence: %w", domain.ErrValidation)
	}
	if evidence == nil {
		// Absent or malformed evidence is never a recovery. Retain the cursor and
		// any open incident so a later check re-evaluates the same threshold.
		return nil, nil
	}
	if in.Maintenance {
		// Suppression covers the administrative lifecycle too: a window must not
		// consume a threshold the operator was never told about, nor quietly close
		// an incident that is still factually open. The effective status during a
		// window is MAINTENANCE, so this check must precede status validation.
		return nil, nil
	}
	if in.Observation.Status != domain.StatusUp && in.Observation.Status != domain.StatusDown {
		return nil, fmt.Errorf("certificate paging observation status %d: %w", in.Observation.Status, domain.ErrValidation)
	}
	if in.Prior != nil && (in.Prior.MonitorID != in.Monitor.ID || in.Prior.AssignmentGeneration != in.Assignment.Generation) {
		return nil, fmt.Errorf("certificate cursor does not match the evaluated assignment: %w", domain.ErrValidation)
	}
	notAfter := evidence.NotAfter.UTC()
	now := in.Now.UTC()

	// The cursor describes only the certificate it was recorded against. Any
	// other expiry is a renewal, so the delivered-threshold history no longer
	// applies and a crossed threshold may alert again under a new identity.
	sameCertificate := in.Prior != nil && in.Prior.AlertNotAfter != nil && in.Prior.AlertNotAfter.UTC().Equal(notAfter)
	delivered := 0
	if sameCertificate {
		delivered = in.Prior.AlertThreshold
	}
	threshold, crossed := domain.MostUrgentCertificateThreshold(evidence.DaysRemaining)

	// An open incident belongs to one certificate and one threshold. It is
	// retired when the certificate renews, when the expiry moves back outside
	// every threshold, or when a more urgent threshold takes over. A strictly
	// less urgent reading of the same certificate (a clock adjustment) leaves it
	// open: the tighter alert was already delivered.
	openIncident := in.Prior != nil && in.Prior.SourceAlertID != ""
	retire := false
	if openIncident {
		retire = !sameCertificate || !crossed || threshold < in.Prior.AlertThreshold || in.Prior.AlertThreshold == 0
	}
	fire := crossed && (delivered == 0 || threshold < delivered)
	if !retire && !fire {
		return nil, nil
	}
	if openIncident && (in.PriorCursor == nil || in.PriorCursor.SubjectKind != domain.IncidentSubjectCertificate) {
		return nil, fmt.Errorf("certificate cursor references an unreadable open incident: %w", domain.ErrValidation)
	}

	work := &domain.EdgeCertAlertWork{Cursor: domain.EdgeCertAlertState{
		MonitorID: in.Monitor.ID, AssignmentGeneration: in.Assignment.Generation, ConfigRevision: in.Observation.ConfigRevision, UpdatedAt: now,
	}}
	// The cursor version is the write fence. It advances exactly once per committed
	// lifecycle, so a store can reject a record built against superseded state
	// instead of replaying or dropping a threshold alert.
	if in.Prior != nil {
		work.Cursor.Version = in.Prior.Version + 1
	} else {
		work.Cursor.Version = 1
	}
	if retire {
		prior := *in.PriorCursor
		if prior.Status == domain.AlertStatusResolved || prior.TransitionVersion <= 0 {
			return nil, fmt.Errorf("open certificate incident has no promotable lifecycle: %w", domain.ErrValidation)
		}
		if prior.TransitionVersion == math.MaxInt64 {
			return nil, fmt.Errorf("open certificate incident cannot advance: %w", domain.ErrValidation)
		}
		resolved := prior
		resolved.Status, resolved.ResolvedAt = domain.AlertStatusResolved, &now
		resolved.TransitionVersion = prior.TransitionVersion + 1
		resolved.ConfigRevision = in.Observation.ConfigRevision
		resolved.Reason = certificateRetirementReason(prior.CertificateThreshold, prior.CertificateNotAfter, notAfter, evidence.DaysRemaining)
		work.Transitions = append(work.Transitions, resolved)
	}
	if !fire {
		// Retirement with no replacement: the certificate recovered past every
		// threshold, or was renewed while a stale incident stayed open.
		work.Cursor.AlertThreshold, work.Cursor.AlertNotAfter, work.Cursor.SourceAlertID = 0, nil, ""
		return work, nil
	}

	alertID, err := in.NewID()
	if err != nil {
		return nil, err
	}
	days := int(evidence.DaysRemaining)
	if days < 0 {
		days = 0
	}
	message := formatCertExpiryMessage(in.Monitor.Name, threshold, days, evidence.Issuer, notAfter)
	expiry := notAfter
	firing := domain.RegionalIncident{
		SourceAlertID: alertID, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectCertificate,
		MonitorID: in.Monitor.ID, ProbeID: in.Observation.ProbeID, AssignmentGeneration: in.Assignment.Generation,
		Status: domain.AlertStatusFiring, TransitionVersion: 1, StartedAt: now, Reason: message,
		ConfigRevision: in.Observation.ConfigRevision, CertificateThreshold: int64(threshold), CertificateNotAfter: &expiry,
	}
	if !domain.ValidCertificateIncident(&firing) {
		return nil, fmt.Errorf("evaluated certificate incident lacks exact subject identity: %w", domain.ErrValidation)
	}
	work.Transitions = append(work.Transitions, firing)
	work.Cursor.AlertThreshold, work.Cursor.AlertNotAfter, work.Cursor.SourceAlertID = threshold, &expiry, alertID
	work.Certificate = &domain.EdgeCertAlertContent{Threshold: threshold, DaysRemaining: days, Issuer: evidence.Issuer, NotAfter: notAfter, Message: message}

	// One alert per crossed threshold, per attached active channel. A missing
	// channel in the accepted graph is a defect, not a reason to skip silently.
	for _, link := range in.Assignment.NotificationLinks {
		channel, exists := in.Config.Channels[link.NotificationID]
		if !exists || channel.Notification == nil {
			return nil, fmt.Errorf("certificate alert channel %d is absent from the accepted graph: %w", link.NotificationID, domain.ErrValidation)
		}
		if !channel.Notification.Active {
			continue
		}
		deliveryID, err := in.NewID()
		if err != nil {
			return nil, err
		}
		work.Intents = append(work.Intents, domain.DeliveryIntent{
			DeliveryID: deliveryID, SourceAlertID: alertID, SourceTransitionVersion: 1,
			ProbeID: in.Observation.ProbeID, NotificationID: link.NotificationID, NotificationVersion: channel.Version,
			EventKind: domain.DeliveryEventCertificateExpiry, AvailableAt: now,
		})
	}
	return work, nil
}

// certificateRetirementReason records, in bounded redacted text, why an open
// certificate incident closed without its own provider notification.
func certificateRetirementReason(threshold int64, incidentNotAfter *time.Time, observed time.Time, daysRemaining int64) string {
	switch {
	case incidentNotAfter != nil && !incidentNotAfter.UTC().Equal(observed):
		return fmt.Sprintf("TLS certificate was renewed; the %d day alert applies to the superseded expiry", threshold)
	case daysRemaining > 30:
		return fmt.Sprintf("TLS certificate expiry advanced beyond the %d day threshold", threshold)
	case daysRemaining < 0:
		return "TLS certificate evidence is no longer evaluable"
	default:
		return fmt.Sprintf("TLS certificate alert advanced past the %d day threshold", threshold)
	}
}
