package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// LocalConfigValidator checks local snapshot semantics without executing checks
// or sending notifications. Registry lookups are supplied by the composition root.
// It does not probe ICMP privileges, Docker sockets, DNS or network reachability.
type LocalConfigValidator struct {
	checker func(string) (ports.Checker, bool)
	sender  func(string) (ports.NotificationSender, bool)
}

var _ ports.LocalProbeConfigValidator = (*LocalConfigValidator)(nil)

// NewLocalConfigValidator uses the installed checkers and notification senders.
// Missing lookups fail validation even for an empty replacement snapshot.
func NewLocalConfigValidator(checker func(string) (ports.Checker, bool), sender func(string) (ports.NotificationSender, bool)) *LocalConfigValidator {
	return &LocalConfigValidator{checker: checker, sender: sender}
}

// ValidateLocal rechecks the complete graph before inspecting extension objects.
// Errors expose only fixed section names and numeric indexes, never validator
// diagnostics, endpoints, names, credentials, template text or binding keys.
func (v *LocalConfigValidator) ValidateLocal(ctx context.Context, document []byte, target domain.ProbeConfigTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v == nil || v.checker == nil || v.sender == nil || !domain.ValidProbeConfigTarget(target) || target.ProbeID != domain.LocalProbeID {
		return domain.ErrValidation
	}
	s, err := DecodeLocalConfigSnapshot(document)
	if err != nil || s.HubID != target.HubID || s.ProbeID != target.ProbeID || s.Watchdog.Enabled {
		return localConfigInvalid(ctx, "snapshot", 0)
	}
	for i, a := range s.Assignments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := v.validateAssignment(a); err != nil {
			return localConfigInvalid(ctx, "assignments", i)
		}
	}
	for i, c := range s.NotificationChannels {
		if err := ctx.Err(); err != nil {
			return err
		}
		sender, ok := v.sender(c.Type)
		config, err := configExtensionObject(c.Config)
		if !ok || sender == nil || sender.Type() != c.Type || err != nil || sender.Validate(config) != nil {
			return localConfigInvalid(ctx, "notification_channels", i)
		}
	}
	for i, t := range s.NotificationTemplates {
		if err := ctx.Err(); err != nil {
			return err
		}
		config, err := configExtensionObject(t.Config)
		if err != nil || services.ValidateNotificationTemplate(&domain.NotificationTemplate{
			ID: t.ID, Name: t.Name, Provider: t.Provider, TitleTemplate: t.TitleTemplate, BodyTemplate: t.BodyTemplate, Config: config,
		}) != nil {
			return localConfigInvalid(ctx, "notification_templates", i)
		}
	}
	for i, w := range s.MaintenanceWindows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validLocalConfigSchedule(w, time.Time(s.EffectiveAt)) {
			return localConfigInvalid(ctx, "maintenance_windows", i)
		}
	}
	for i, p := range s.ProxyBindings {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validLocalConfigProxy(p) {
			return localConfigInvalid(ctx, "proxy_bindings", i)
		}
	}
	return ctx.Err()
}

func (v *LocalConfigValidator) validateAssignment(a ConfigAssignment) error {
	c, ok := v.checker(a.Monitor.Type)
	if !ok || c == nil || c.Type() != a.Monitor.Type {
		return domain.ErrValidation
	}
	for _, capability := range a.RequiredCapabilities {
		if !v.supports(capability) {
			return domain.ErrValidation
		}
	}
	config, err := configExtensionObject(a.Monitor.Config)
	if err != nil {
		return err
	}
	if a.Monitor.Type == "push" {
		// PushChecker.Validate expects an inbound credential deliberately absent
		// from this dialect. Push is local-only and has no outbound execution
		// settings; activation must check hub-owned push identity separately.
		if _, exists := config["push_token"]; exists {
			return domain.ErrValidation
		}
		return nil
	}
	// Match scheduler overrides so validation sees the effective check settings.
	config["timeout"] = a.Monitor.Timeout
	if a.Monitor.TLSIgnore {
		config["tls_ignore"] = true
	}
	if len(a.Monitor.AcceptedStatusCodes) != 0 {
		codes := make([]any, len(a.Monitor.AcceptedStatusCodes))
		for i, code := range a.Monitor.AcceptedStatusCodes {
			codes[i] = code
		}
		config["accepted_statuscodes"] = codes
	}
	return c.Validate(config)
}

func (v *LocalConfigValidator) supports(capability string) bool {
	if capability == "snapshot.v1" {
		return true
	}
	kind, rest, ok := strings.Cut(capability, ".")
	name, version, versioned := strings.Cut(rest, ".")
	if !ok || !versioned || version != "v1" {
		return false
	}
	switch kind {
	case "checker":
		if !pullMonitorType(name) && name != "push" {
			return false
		}
		c, ok := v.checker(name)
		return ok && c != nil && c.Type() == name
	case "notifier":
		if !notificationProvider(name) {
			return false
		}
		s, ok := v.sender(name)
		return ok && s != nil && s.Type() == name
	default:
		return false
	}
}

func configExtensionObject(data []byte) (map[string]any, error) {
	var config map[string]any
	// Keep JSON numbers as float64, as the existing extension validators expect.
	if err := json.Unmarshal(data, &config); err != nil || config == nil {
		return nil, domain.ErrValidation
	}
	return config, nil
}

func validLocalConfigSchedule(w ConfigMaintenance, effectiveAt time.Time) bool {
	loc, err := time.LoadLocation(w.Timezone)
	if err != nil || w.Timezone == "Local" {
		return false
	}
	if w.Strategy != "cron" {
		return true // Single-window bounds were checked by the graph decoder.
	}
	fields := strings.Fields(w.CronExpr)
	if len(fields) == 0 || fields[0] == "TZ=Local" || fields[0] == "CRON_TZ=Local" {
		return false
	}
	// Use the same five-field/descriptor grammar as scheduler.CronEvaluator.
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	schedule, err := parser.Parse(w.CronExpr)
	if err != nil {
		return false
	}
	// Impossible calendar dates must not become a silently ineffective window.
	return !schedule.Next(effectiveAt.In(loc)).IsZero()
}

func validLocalConfigProxy(p ConfigProxy) bool {
	// A host is not a URL, path or host:port. net.JoinHostPort also supports
	// unbracketed IPv6 literals, matching the HTTP checker's proxy construction.
	if strings.Contains(p.Host, ":") {
		if _, err := netip.ParseAddr(p.Host); err != nil {
			return false
		}
	}
	u := url.URL{Scheme: p.Protocol, Host: net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))}
	parsed, err := url.Parse(u.String())
	return err == nil && parsed.Hostname() == p.Host && parsed.Port() == strconv.Itoa(int(p.Port)) &&
		parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" &&
		!strings.ContainsAny(p.Host, " \t\r\n/@?#[]")
}

func localConfigInvalid(ctx context.Context, section string, index int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("local configuration %s[%d]: %w", section, index, domain.ErrValidation)
}
