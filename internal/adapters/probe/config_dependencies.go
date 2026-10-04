package probe

import (
	"errors"
	"strings"
	"time"
)

func decodeConfigChannelForTarget(data []byte, local bool) (ConfigChannel, error) {
	var channel ConfigChannel
	if _, err := decodeConfigFields(data, &channel, "id version type name active config include_ack_url", "template_id"); err != nil {
		return channel, err
	}
	if channel.ID <= 0 || channel.Version <= 0 || !notificationProvider(channel.Type) || !validConfigName(channel.Name) || !local && channel.IncludeAckURL || channel.TemplateID != nil && *channel.TemplateID <= 0 || !validConfigExtension(channel.Config, MaxConfigObjectBytes) {
		return channel, errors.New("invalid channel identity, provider, metadata, config, or remote acknowledgement setting")
	}
	return channel, nil
}

func decodeConfigTemplate(data []byte) (ConfigTemplate, error) {
	var template ConfigTemplate
	if _, err := decodeConfigFields(data, &template, "id provider name version title_template body_template config", ""); err != nil {
		return template, err
	}
	switch template.Provider {
	case "discord", "smtp", "webhook", "line":
	default:
		return template, errors.New("unsupported template provider")
	}
	if template.ID <= 0 || template.Version <= 0 || !validConfigName(template.Name) || len(template.TitleTemplate) > 1000 || len(template.BodyTemplate) > 64<<10 || !validConfigExtension(template.Config, MaxTemplateConfigBytes) {
		return template, errors.New("invalid template identity, metadata, or object bounds")
	}
	return template, nil
}

func decodeConfigMaintenance(data []byte) (ConfigMaintenance, error) {
	var window ConfigMaintenance
	if _, err := decodeConfigFields(data, &window, "id active strategy cron_expr duration timezone monitor_ids", "start_date end_date"); err != nil {
		return window, err
	}
	if window.ID <= 0 || !validConfigIDs(window.MonitorIDs, MaxConfigAssignments) || !validConfigMinutes(window.Duration) || strings.TrimSpace(window.Timezone) == "" || len(window.Timezone) > 256 || len(window.CronExpr) > 4096 {
		return window, errors.New("invalid maintenance identity, references, or bounds")
	}
	switch window.Strategy {
	case "single":
		if window.StartDate == nil || window.EndDate == nil || !time.Time(*window.StartDate).Before(time.Time(*window.EndDate)) {
			return window, errors.New("single maintenance requires ordered start/end dates")
		}
	case "cron":
		if window.StartDate != nil || window.EndDate != nil || strings.TrimSpace(window.CronExpr) == "" || window.Duration <= 0 {
			return window, errors.New("cron maintenance requires null dates, an expression, and positive duration")
		}
	default:
		return window, errors.New("unsupported maintenance strategy")
	}
	return window, nil
}

func decodeConfigProxy(data []byte) (ConfigProxy, error) {
	var proxy ConfigProxy
	if _, err := decodeConfigFields(data, &proxy, "binding_key version protocol host port auth username password active", ""); err != nil {
		return proxy, err
	}
	if !validBindingKey(proxy.BindingKey) || proxy.Version <= 0 || strings.TrimSpace(proxy.Host) == "" || len(proxy.Host) > 4096 || proxy.Port <= 0 || proxy.Port > 65535 || len(proxy.Username) > 4096 || len(proxy.Password) > 16<<10 {
		return proxy, errors.New("invalid proxy identity, endpoint, or credential bounds")
	}
	switch proxy.Protocol {
	case "http", "https", "socks5":
	default:
		return proxy, errors.New("unsupported proxy protocol")
	}
	return proxy, nil
}

func decodeConfigEscalation(data []byte) (ConfigEscalation, error) {
	var policy ConfigEscalation
	fields, err := decodeConfigFields(data, &policy, "id version enabled steps", "")
	if err != nil {
		return policy, err
	}
	if policy.ID <= 0 || policy.Version <= 0 {
		return policy, errors.New("invalid escalation identity/version")
	}
	policy.Steps, err = decodeConfigList(fields["steps"], 20, decodeConfigEscalationStep)
	if err != nil {
		return policy, err
	}
	for index, step := range policy.Steps {
		if int64(step.Step) != int64(index)+1 {
			return policy, errors.New("escalation steps must be dense and ordered from one")
		}
	}
	return policy, nil
}

func decodeConfigEscalationStep(data []byte) (ConfigEscalationStep, error) {
	var step ConfigEscalationStep
	if _, err := decodeConfigFields(data, &step, "step delay_seconds notification_ids", ""); err != nil {
		return step, err
	}
	if step.Step <= 0 || step.DelaySeconds < 0 || step.DelaySeconds > 7*24*60*60 || step.DelaySeconds%60 != 0 || len(step.NotificationIDs) == 0 || !validConfigIDs(step.NotificationIDs, MaxConfigDependencies) {
		return step, errors.New("invalid escalation order, minute-based delay, or channels")
	}
	return step, nil
}

func decodeConfigWatchdog(data []byte) (ConfigWatchdog, error) {
	var watchdog ConfigWatchdog
	if len(data) > MaxConfigEntryBytes {
		return watchdog, errors.New("watchdog exceeds byte limit")
	}
	if _, err := decodeConfigFields(data, &watchdog, "enabled lost_after_seconds recover_after_seconds notification_ids resend_interval", ""); err != nil {
		return watchdog, err
	}
	if watchdog.LostAfterSeconds <= 0 || watchdog.RecoverAfterSeconds <= 0 || !validConfigMinutes(watchdog.ResendInterval) || !validConfigIDs(watchdog.NotificationIDs, MaxConfigDependencies) {
		return watchdog, errors.New("invalid watchdog timing or channels")
	}
	return watchdog, nil
}

func notificationProvider(provider string) bool {
	switch provider {
	case "telegram", "discord", "slack", "smtp", "webhook", "teams", "mattermost", "gotify", "bark", "feishu", "line":
		return true
	default:
		return false
	}
}
