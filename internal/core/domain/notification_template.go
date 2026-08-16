package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NotificationTemplate is a reusable message layout for one notification
// provider. Templates are install-wide resources; UserID records their creator.
type NotificationTemplate struct {
	ID            int64
	UserID        int64
	Name          string
	Provider      string
	TitleTemplate string
	BodyTemplate  string
	Config        map[string]any
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

var notificationTemplatePlaceholder = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_.]*)\s*\}\}`)

var notificationTemplateVariables = []string{
	"alert.scope",
	"alert.id",
	"alert.name",
	"alert.type",
	"alert.target",
	"monitor.id",
	"monitor.name",
	"monitor.type",
	"monitor.target",
	"monitor.description",
	"monitor.owner",
	"group.id",
	"group.name",
	"group.description",
	"group.owner",
	"group.condition",
	"group.threshold",
	"group.threshold_is_percent",
	"group.threshold_display",
	"status",
	"status.emoji",
	"previous_status",
	"message",
	"check_output",
	"duration",
	"started_at",
	"started_at.unix",
	"timestamp",
	"timestamp.unix",
	"event_kind",
	"ack_url",
	"tags",
	"certificate.threshold",
	"certificate.days_remaining",
	"certificate.issuer",
	"certificate.not_after",
	"condition.kind",
	"condition.state",
	"condition.previous_state",
	"condition.used",
	"condition.limit",
	"condition.percent",
	"condition.threshold",
	"condition.unit",
	"condition.resource",
	"condition.scope",
	"condition.source",
	"condition.observed_at",
}

// NotificationTemplateVariables returns every placeholder supported by the
// notification template renderer. Prefix a variable with "json." to emit a
// JSON-encoded value suitable for a custom webhook body.
func NotificationTemplateVariables() []string {
	return append([]string(nil), notificationTemplateVariables...)
}

// NotificationTemplateProviderSupported reports whether provider messages can
// be customized through reusable notification templates.
func NotificationTemplateProviderSupported(provider string) bool {
	switch provider {
	case "discord", "smtp", "webhook", "line":
		return true
	default:
		return false
	}
}

// ValidateNotificationTemplateText verifies that every placeholder in text is
// known and that no malformed template delimiter remains.
func ValidateNotificationTemplateText(text string) error {
	_, err := RenderNotificationTemplate(text, AlertContext{}, time.Unix(0, 0).UTC())
	return err
}

// RenderNotificationTemplate expands Phoenix placeholders in text using alert.
// The caller supplies now so previews and tests can be deterministic.
func RenderNotificationTemplate(text string, alert AlertContext, now time.Time) (string, error) {
	values := notificationTemplateValues(alert, now)
	var renderErr error
	rendered := notificationTemplatePlaceholder.ReplaceAllStringFunc(text, func(match string) string {
		parts := notificationTemplatePlaceholder.FindStringSubmatch(match)
		if len(parts) != 2 {
			renderErr = fmt.Errorf("invalid placeholder %q", match)
			return match
		}

		name := parts[1]
		jsonEncoded := strings.HasPrefix(name, "json.")
		if jsonEncoded {
			name = strings.TrimPrefix(name, "json.")
		}
		value, ok := values[name]
		if !ok {
			renderErr = fmt.Errorf("unknown placeholder %q", parts[1])
			return match
		}
		if !jsonEncoded {
			return value.text
		}
		encoded, err := json.Marshal(value.jsonValue)
		if err != nil {
			renderErr = fmt.Errorf("encode placeholder %q: %w", parts[1], err)
			return match
		}
		return string(encoded)
	})
	if renderErr != nil {
		return "", renderErr
	}
	// A remaining opening delimiter means the source contained a malformed
	// placeholder. Do not reject a bare "}}": adjacent JSON object closers are
	// valid and common in webhook bodies.
	if strings.Contains(rendered, "{{") {
		return "", fmt.Errorf("malformed template placeholder")
	}
	return rendered, nil
}

// RenderNotificationHTMLTemplate expands Phoenix placeholders in an
// operator-authored HTML email. Placeholder values are passed through the
// standard library's context-aware HTML template escaper, so alert data cannot
// break out of text, attributes, CSS, or URL contexts. Literal HTML authored by
// a notification manager remains unchanged.
func RenderNotificationHTMLTemplate(text string, alert AlertContext, now time.Time) (string, error) {
	values := notificationTemplateValues(alert, now)
	withoutPlaceholders := notificationTemplatePlaceholder.ReplaceAllString(text, "")
	if strings.Contains(withoutPlaceholders, "{{") {
		return "", fmt.Errorf("malformed template placeholder")
	}

	var transformErr error
	transformed := notificationTemplatePlaceholder.ReplaceAllStringFunc(text, func(match string) string {
		parts := notificationTemplatePlaceholder.FindStringSubmatch(match)
		if len(parts) != 2 {
			transformErr = fmt.Errorf("invalid placeholder %q", match)
			return match
		}

		name := parts[1]
		lookupName := strings.TrimPrefix(name, "json.")
		if _, ok := values[lookupName]; !ok {
			transformErr = fmt.Errorf("unknown placeholder %q", name)
			return match
		}
		return fmt.Sprintf(`{{ phoenixValue %q }}`, name)
	})
	if transformErr != nil {
		return "", transformErr
	}

	valueForTemplate := func(name string) (string, error) {
		jsonEncoded := strings.HasPrefix(name, "json.")
		lookupName := strings.TrimPrefix(name, "json.")
		value, ok := values[lookupName]
		if !ok {
			return "", fmt.Errorf("unknown placeholder %q", name)
		}
		if !jsonEncoded {
			return value.text, nil
		}
		encoded, err := json.Marshal(value.jsonValue)
		if err != nil {
			return "", fmt.Errorf("encode placeholder %q: %w", name, err)
		}
		return string(encoded), nil
	}

	template, err := htmltemplate.New("notification-email").Funcs(htmltemplate.FuncMap{
		"phoenixValue": valueForTemplate,
	}).Parse(transformed)
	if err != nil {
		return "", fmt.Errorf("parse HTML template: %w", err)
	}
	var rendered bytes.Buffer
	if err := template.Execute(&rendered, nil); err != nil {
		return "", fmt.Errorf("render HTML template: %w", err)
	}
	if strings.Contains(rendered.String(), "ZgotmplZ") {
		return "", fmt.Errorf("unsafe value in HTML template context")
	}
	return rendered.String(), nil
}

type notificationTemplateValue struct {
	text      string
	jsonValue any
}

func notificationTemplateValues(alert AlertContext, now time.Time) map[string]notificationTemplateValue {
	eventKind := alert.EventKind
	if eventKind == "" {
		eventKind = AlertEventStatusChange
	}

	tags := make([]string, 0, len(alert.Tags))
	for key, value := range alert.Tags {
		tags = append(tags, key+"="+value)
	}
	sort.Strings(tags)
	tagsText := strings.Join(tags, ", ")

	startedAt := ""
	startedAtUnix := notificationTemplateValue{text: "", jsonValue: nil}
	if !alert.StartedAt.IsZero() {
		startedAt = alert.StartedAt.UTC().Format(time.RFC3339)
		startedAtUnix = notificationTemplateValue{
			text:      strconv.FormatInt(alert.StartedAt.Unix(), 10),
			jsonValue: alert.StartedAt.Unix(),
		}
	}
	duration := ""
	if alert.Duration > 0 {
		duration = alert.Duration.String()
	}
	certNotAfter := ""
	if alert.CertNotAfter != nil && !alert.CertNotAfter.IsZero() {
		certNotAfter = alert.CertNotAfter.UTC().Format(time.RFC3339)
	}
	conditionObservedAt := ""
	if alert.ConditionObservedAt != nil && !alert.ConditionObservedAt.IsZero() {
		conditionObservedAt = alert.ConditionObservedAt.UTC().Format(time.RFC3339)
	}

	stringValue := func(value string) notificationTemplateValue {
		return notificationTemplateValue{text: value, jsonValue: value}
	}
	intValue := func(value int64) notificationTemplateValue {
		return notificationTemplateValue{text: strconv.FormatInt(value, 10), jsonValue: value}
	}
	boolValue := func(value bool) notificationTemplateValue {
		return notificationTemplateValue{text: strconv.FormatBool(value), jsonValue: value}
	}
	floatPointerValue := func(value *float64) notificationTemplateValue {
		if value == nil {
			return notificationTemplateValue{text: "", jsonValue: nil}
		}
		return notificationTemplateValue{
			text:      strconv.FormatFloat(*value, 'f', -1, 64),
			jsonValue: *value,
		}
	}

	scope := alert.AlertScope
	if scope == "" {
		if alert.GroupID != 0 || alert.MonitorType == AlertScopeGroup {
			scope = AlertScopeGroup
		} else {
			scope = AlertScopeMonitor
		}
	}
	entityID, entityName, entityType, entityTarget := alert.MonitorID, alert.MonitorName, alert.MonitorType, alert.MonitorTarget
	if scope == AlertScopeGroup {
		entityID = alert.GroupID
		entityName = alert.GroupName
		if entityName == "" {
			entityName = alert.MonitorName
		}
		entityType = AlertScopeGroup
		entityTarget = ""
	}
	thresholdDisplay := ""
	if alert.GroupCondition == GroupConditionThreshold && alert.GroupThreshold > 0 {
		thresholdDisplay = strconv.Itoa(alert.GroupThreshold)
		if alert.GroupThresholdIsPercent {
			thresholdDisplay += "%"
		}
	}
	statusEmoji := "⚪"
	if eventKind == AlertEventCertificateExpiry {
		statusEmoji = "⚠️"
	} else if eventKind == AlertEventCapacityCondition {
		switch alert.ConditionState {
		case ConditionStateOK:
			statusEmoji = "✅"
		case ConditionStateError:
			statusEmoji = "❌"
		default:
			statusEmoji = "⚠️"
		}
	} else {
		switch alert.Status {
		case StatusUp:
			statusEmoji = "✅"
		case StatusDown:
			statusEmoji = "❌"
		case StatusPending:
			statusEmoji = "⚠️"
		case StatusMaintenance:
			statusEmoji = "🛠️"
		}
	}

	return map[string]notificationTemplateValue{
		"alert.scope":                stringValue(scope),
		"alert.id":                   intValue(entityID),
		"alert.name":                 stringValue(entityName),
		"alert.type":                 stringValue(entityType),
		"alert.target":               stringValue(entityTarget),
		"monitor.id":                 intValue(alert.MonitorID),
		"monitor.name":               stringValue(alert.MonitorName),
		"monitor.type":               stringValue(alert.MonitorType),
		"monitor.target":             stringValue(alert.MonitorTarget),
		"monitor.description":        stringValue(alert.MonitorDescription),
		"monitor.owner":              stringValue(alert.MonitorOwner),
		"group.id":                   intValue(alert.GroupID),
		"group.name":                 stringValue(alert.GroupName),
		"group.description":          stringValue(alert.GroupDescription),
		"group.owner":                stringValue(alert.GroupOwner),
		"group.condition":            stringValue(string(alert.GroupCondition)),
		"group.threshold":            intValue(int64(alert.GroupThreshold)),
		"group.threshold_is_percent": boolValue(alert.GroupThresholdIsPercent),
		"group.threshold_display":    stringValue(thresholdDisplay),
		"status":                     stringValue(alert.Status.String()),
		"status.emoji":               stringValue(statusEmoji),
		"previous_status":            stringValue(alert.PreviousStatus.String()),
		"message":                    stringValue(alert.Message),
		"check_output":               stringValue(alert.CheckOutput),
		"duration":                   stringValue(duration),
		"started_at":                 stringValue(startedAt),
		"started_at.unix":            startedAtUnix,
		"timestamp":                  stringValue(now.UTC().Format(time.RFC3339)),
		"timestamp.unix":             intValue(now.Unix()),
		"event_kind":                 stringValue(eventKind),
		"ack_url":                    stringValue(alert.AckURL),
		"tags":                       {text: tagsText, jsonValue: alert.Tags},
		"certificate.threshold":      intValue(int64(alert.CertThreshold)),
		"certificate.days_remaining": intValue(int64(alert.CertDaysRemaining)),
		"certificate.issuer":         stringValue(alert.CertIssuer),
		"certificate.not_after":      stringValue(certNotAfter),
		"condition.kind":             stringValue(alert.ConditionKind),
		"condition.state":            stringValue(string(alert.ConditionState)),
		"condition.previous_state":   stringValue(string(alert.ConditionPreviousState)),
		"condition.used":             floatPointerValue(alert.ConditionUsed),
		"condition.limit":            floatPointerValue(alert.ConditionLimit),
		"condition.percent":          floatPointerValue(alert.ConditionPercent),
		"condition.threshold":        floatPointerValue(alert.ConditionThreshold),
		"condition.unit":             stringValue(alert.ConditionUnit),
		"condition.resource":         stringValue(alert.ConditionResource),
		"condition.scope":            stringValue(alert.ConditionScope),
		"condition.source":           stringValue(alert.ConditionSource),
		"condition.observed_at":      stringValue(conditionObservedAt),
	}
}
