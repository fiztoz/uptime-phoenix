package notifier

import (
	"fmt"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// isCertificateExpiry reports whether the alert is a certificate-expiry event.
// Empty EventKind is treated as status_change for backward compatibility.
func isCertificateExpiry(alert domain.AlertContext) bool {
	return alert.EventKind == domain.AlertEventCertificateExpiry
}

func isCapacityCondition(alert domain.AlertContext) bool {
	return alert.EventKind == domain.AlertEventCapacityCondition
}

func isAuxiliaryAlert(alert domain.AlertContext) bool {
	return isCertificateExpiry(alert) || isCapacityCondition(alert) || isProbeConnection(alert)
}

func isProbeConnection(alert domain.AlertContext) bool {
	return alert.EventKind == domain.DeliveryEventProbeConnection && alert.AlertScope == domain.AlertScopeProbe
}

func probeAlertName(alert domain.AlertContext) string {
	if alert.ProbeName != "" {
		return alert.ProbeName
	}
	return alert.ProbeID
}

// alertTitle returns a short title appropriate for the event kind.
func alertTitle(alert domain.AlertContext) string {
	if isProbeConnection(alert) {
		if alert.Status == domain.StatusUp {
			return "Probe connection restored: " + probeAlertName(alert)
		}
		return "Probe connection lost: " + probeAlertName(alert)
	}
	if isCertificateExpiry(alert) {
		return fmt.Sprintf("Certificate expiring: %s (%d days)", alert.MonitorName, alert.CertDaysRemaining)
	}
	if isCapacityCondition(alert) {
		label := alert.ConditionResource
		if label == "" {
			label = alert.ConditionKind
		}
		switch alert.ConditionState {
		case domain.ConditionStateOK:
			return fmt.Sprintf("Capacity recovered: %s — %s", alert.MonitorName, label)
		case domain.ConditionStateError:
			return fmt.Sprintf("Capacity check error: %s — %s", alert.MonitorName, label)
		default:
			return fmt.Sprintf("Capacity warning: %s — %s", alert.MonitorName, label)
		}
	}
	return fmt.Sprintf("%s is %s%s", alert.MonitorName, alert.Status, aggregateTitleSuffix(alert))
}

// aggregateScopePrefix is the plain-text marker operators are told to trust
// when separating a hub aggregate page from per-region pages.
const aggregateScopePrefix = "AGGREGATE "

// aggregateTitleSuffix marks a hub aggregate page so a receiver that keys on
// the title can tell it apart from the per-region pages in `both` mode.
func aggregateTitleSuffix(alert domain.AlertContext) string {
	if alert.DeliveryScope == domain.IncidentScopeAggregate {
		return " (aggregate)"
	}
	return ""
}

// disarmAggregatePrefix keeps aggregateScopePrefix exclusive to hub aggregate
// pages. Check output and push messages are caller-controlled, so a
// non-aggregate alert must not present that prefix as the start of a line.
func disarmAggregatePrefix(alert domain.AlertContext, text string) string {
	if text == "" || alert.DeliveryScope == domain.IncidentScopeAggregate {
		return text
	}
	lines := strings.Split(text, "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t\r")
		if strings.HasPrefix(trimmed, aggregateScopePrefix) {
			lead := line[:len(line)-len(trimmed)]
			lines[i] = lead + "check: " + trimmed
			changed = true
		}
	}
	if !changed {
		return text
	}
	return strings.Join(lines, "\n")
}

// alertTitleWithPrefix is like alertTitle but with a leading product prefix
// (e.g. "Phoenix Alert: …").
func alertTitleWithPrefix(prefix string, alert domain.AlertContext) string {
	if isAuxiliaryAlert(alert) {
		return strings.TrimSpace(prefix) + " " + alertTitle(alert)
	}
	return fmt.Sprintf("%s %s is %s%s", prefix, alert.MonitorName, alert.Status, aggregateTitleSuffix(alert))
}

// alertBody expands the human-readable body. Certificate events include
// threshold, days remaining, issuer, and NotAfter when present.
func alertBody(alert domain.AlertContext) (bodyText string) {
	defer func() {
		bodyText = disarmAggregatePrefix(alert, bodyText)
		if alert.DeliveryScope == domain.IncidentScopeRegional && alert.ProbeID != "" {
			bodyText += "\nRegion: " + probeAlertName(alert)
			if alert.ProbeLocation != "" {
				bodyText += " (" + alert.ProbeLocation + ")"
			}
		}
	}()
	if isProbeConnection(alert) {
		body := alert.Message
		if body == "" {
			body = alertTitle(alert)
		}
		body += "\nProbe: " + probeAlertName(alert)
		if alert.ProbeLocation != "" {
			body += "\nLocation: " + alert.ProbeLocation
		}
		return body
	}
	if isCapacityCondition(alert) {
		if alert.Message != "" {
			return alert.Message
		}
		return fmt.Sprintf("%s condition is %s", alert.ConditionResource, alert.ConditionState)
	}
	if !isCertificateExpiry(alert) {
		if alert.Message != "" {
			return alert.Message
		}
		return fmt.Sprintf("%s is %s", alert.MonitorName, alert.Status)
	}
	body := alert.Message
	if body == "" {
		body = fmt.Sprintf(
			"TLS certificate for %s expires in %d day(s) (threshold: %d days)",
			alert.MonitorName, alert.CertDaysRemaining, alert.CertThreshold,
		)
	}
	if alert.CertIssuer != "" && !strings.Contains(body, alert.CertIssuer) {
		body += fmt.Sprintf("\nIssuer: %s", alert.CertIssuer)
	}
	if alert.CertNotAfter != nil && !alert.CertNotAfter.IsZero() {
		na := alert.CertNotAfter.UTC().Format(time.RFC3339)
		if !strings.Contains(body, na) {
			body += fmt.Sprintf("\nNot after: %s", na)
		}
	}
	return body
}

// webhookEventPayload returns the default JSON object fields for a webhook
// body, including event kind and certificate fields when applicable.
func webhookEventPayload(alert domain.AlertContext) map[string]any {
	kind := alert.EventKind
	if kind == "" {
		kind = domain.AlertEventStatusChange
	}
	body := map[string]any{
		"title":      alertTitle(alert),
		"message":    alertBody(alert),
		"event_kind": kind,
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
		"monitor": map[string]any{
			"name":   alert.MonitorName,
			"type":   alert.MonitorType,
			"target": alert.MonitorTarget,
			"id":     alert.MonitorID,
		},
		"check_output": alert.CheckOutput,
	}
	if isProbeConnection(alert) {
		delete(body, "monitor")
	}
	if isProbeConnection(alert) || alert.DeliveryScope == domain.IncidentScopeRegional && alert.ProbeID != "" {
		body["probe"] = map[string]any{"id": alert.ProbeID, "name": alert.ProbeName, "location": alert.ProbeLocation}
		body["delivery_scope"], body["source_alert_id"] = alert.DeliveryScope, alert.SourceAlertID
	} else if alert.DeliveryScope == domain.IncidentScopeAggregate {
		body["delivery_scope"], body["source_alert_id"] = alert.DeliveryScope, alert.SourceAlertID
	}
	if isCertificateExpiry(alert) {
		body["cert_threshold"] = alert.CertThreshold
		body["cert_days_remaining"] = alert.CertDaysRemaining
		if alert.CertIssuer != "" {
			body["cert_issuer"] = alert.CertIssuer
		}
		if alert.CertNotAfter != nil && !alert.CertNotAfter.IsZero() {
			body["cert_not_after"] = alert.CertNotAfter.UTC().Format(time.RFC3339)
		}
	} else if isCapacityCondition(alert) {
		body["condition"] = map[string]any{
			"kind":           alert.ConditionKind,
			"state":          alert.ConditionState,
			"previous_state": alert.ConditionPreviousState,
			"used":           alert.ConditionUsed,
			"limit":          alert.ConditionLimit,
			"percent":        alert.ConditionPercent,
			"threshold":      alert.ConditionThreshold,
			"unit":           alert.ConditionUnit,
			"resource":       alert.ConditionResource,
			"scope":          alert.ConditionScope,
			"source":         alert.ConditionSource,
			"observed_at":    alert.ConditionObservedAt,
		}
		body["severity"] = alert.ConditionState
	} else {
		body["severity"] = alert.Status.String()
		body["status"] = alert.Status
	}
	return body
}

func alertEmoji(alert domain.AlertContext) string {
	if isCertificateExpiry(alert) {
		return "📜"
	}
	if isCapacityCondition(alert) {
		switch alert.ConditionState {
		case domain.ConditionStateOK:
			return "✅"
		case domain.ConditionStateError:
			return "❌"
		default:
			return "⚠️"
		}
	}
	switch alert.Status {
	case domain.StatusDown:
		return "🔴"
	case domain.StatusMaintenance:
		return "🔧"
	case domain.StatusPending:
		return "⏳"
	default:
		return "✅"
	}
}

// renderCustomLayout expands the reusable template selected on a notification.
// Empty fields are left empty so each provider can retain its built-in fallback.
func renderCustomLayout(alert domain.AlertContext) (title, body string, custom bool, err error) {
	return renderCustomLayoutAt(alert, time.Now().UTC())
}

func renderCustomLayoutAt(alert domain.AlertContext, now time.Time) (title, body string, custom bool, err error) {
	if alert.TemplateTitle == "" && alert.TemplateBody == "" {
		return "", "", false, nil
	}
	if alert.TemplateTitle != "" {
		title, err = domain.RenderNotificationTemplate(alert.TemplateTitle, alert, now)
		if err != nil {
			return "", "", true, fmt.Errorf("render title: %w", err)
		}
	}
	if alert.TemplateBody != "" {
		body, err = domain.RenderNotificationTemplate(alert.TemplateBody, alert, now)
		if err != nil {
			return "", "", true, fmt.Errorf("render body: %w", err)
		}
	}
	return title, body, true, nil
}
