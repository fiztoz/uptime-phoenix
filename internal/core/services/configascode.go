package services

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// secretConfigKeys are notification/proxy config map keys treated as secrets.
var secretConfigKeys = map[string]struct{}{
	"token": {}, "password": {}, "webhook_url": {}, "webhookurl": {},
	"bot_token": {}, "bottoken": {}, "api_key": {}, "apikey": {},
	"secret": {}, "auth_password": {}, "authpassword": {},
	"client_secret": {}, "clientsecret": {}, "access_token": {},
	"accesstoken": {}, "private_key": {}, "privatekey": {},
	"smtp_password": {}, "smtppassword": {}, "app_secret": {},
	"appsecret": {}, "channel_secret": {}, "channelsecret": {},
	"secret_key": {}, "secretkey": {}, "session_token": {}, "sessiontoken": {},
}

var configKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// probeConfigKeyPattern mirrors the registration store's stable probe key
// contract (`probes.probe_key`): a lowercase slug, never the reserved `local`.
// Services cannot import the adapter, so the rule is restated here and tested.
var probeConfigKeyPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ConfigService implements declarative config-as-code (F5 Sprint 14).
type ConfigService struct {
	keys          ports.ConfigKeyRepository
	tags          ports.TagRepository
	proxies       ports.ProxyRepository
	notifications ports.NotificationRepository
	groups        ports.MonitorGroupRepository
	monitors      ports.MonitorRepository
	monitorTags   ports.MonitorTagRepository
	monitorNotifs ports.MonitorNotificationRepository
	groupNotifs   ports.GroupNotificationRepository
	statusPages   ports.StatusPageRepository
	spMonitors    ports.StatusPageMonitorRepository
	maintenance   ports.MaintenanceRepository
	maintMonitors ports.MaintenanceWindowMonitorRepository
	// Optional (SetProbeRegistry/SetProbeAssignments): probe identities and
	// monitor vantage-point sets. Probes are keyed natively by probe key and
	// never live in config_keys. When either is missing, a document that
	// declares probes or assignment sets is refused instead of half applying.
	probeRegistry    ports.ProbeRegistryRepository
	probeAssignments ports.MonitorProbeAssignmentRepository
	password         ports.PasswordHasher // for status-page access codes
}

// NewConfigService wires repositories for config-as-code.
func NewConfigService(
	keys ports.ConfigKeyRepository,
	tags ports.TagRepository,
	proxies ports.ProxyRepository,
	notifications ports.NotificationRepository,
	groups ports.MonitorGroupRepository,
	monitors ports.MonitorRepository,
	monitorTags ports.MonitorTagRepository,
	monitorNotifs ports.MonitorNotificationRepository,
	groupNotifs ports.GroupNotificationRepository,
	statusPages ports.StatusPageRepository,
	spMonitors ports.StatusPageMonitorRepository,
	maintenance ports.MaintenanceRepository,
	maintMonitors ports.MaintenanceWindowMonitorRepository,
	password ports.PasswordHasher,
) *ConfigService {
	return &ConfigService{
		keys: keys, tags: tags, proxies: proxies, notifications: notifications,
		groups: groups, monitors: monitors, monitorTags: monitorTags,
		monitorNotifs: monitorNotifs, groupNotifs: groupNotifs,
		statusPages: statusPages, spMonitors: spMonitors,
		maintenance: maintenance, maintMonitors: maintMonitors,
		password: password,
	}
}

// Validate checks the document without writing.
func (s *ConfigService) Validate(_ context.Context, doc *ConfigDocument) []string {
	return validateConfigDocument(doc)
}

// SetProbeRegistry attaches the probe registration store so `probes`
// declarations can upsert registrations by their stable keys.
func (s *ConfigService) SetProbeRegistry(repo ports.ProbeRegistryRepository) {
	s.probeRegistry = repo
}

// SetProbeAssignments attaches the desired-assignment store so monitor
// `probe_assignments` declarations are applied as complete sets.
func (s *ConfigService) SetProbeAssignments(repo ports.MonitorProbeAssignmentRepository) {
	s.probeAssignments = repo
}

// Plan computes the diff without writing.
func (s *ConfigService) Plan(ctx context.Context, userID int64, doc *ConfigDocument, opts ConfigApplyOptions) (*ConfigPlan, error) {
	errs := validateConfigDocument(doc)
	plan := &ConfigPlan{Valid: len(errs) == 0, Errors: errs, Changes: []ConfigChange{}}
	if !plan.Valid {
		return plan, nil
	}
	if err := s.buildPlan(ctx, userID, doc, opts, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// Apply upserts the document. When prune is true, keyed resources missing
// from the document are deleted. A second apply of the same document is a no-op.
func (s *ConfigService) Apply(ctx context.Context, userID int64, doc *ConfigDocument, opts ConfigApplyOptions) (*ConfigApplyResult, error) {
	plan, err := s.Plan(ctx, userID, doc, opts)
	if err != nil {
		return nil, err
	}
	if !plan.Valid {
		return &ConfigApplyResult{Plan: plan}, fmt.Errorf("config apply: %w: %s", domain.ErrValidation, strings.Join(plan.Errors, "; "))
	}
	applied := make([]ConfigChange, 0)
	if err := s.applyPlan(ctx, userID, doc, plan, &applied); err != nil {
		return nil, err
	}
	// Recompute plan against live state so the result reflects residual zero-diff.
	after, err := s.Plan(ctx, userID, doc, opts)
	if err != nil {
		return nil, err
	}
	res := &ConfigApplyResult{Plan: after, Applied: applied}
	for _, c := range applied {
		switch c.Action {
		case ConfigActionCreate:
			res.Creates++
		case ConfigActionUpdate:
			res.Updates++
		case ConfigActionDelete:
			res.Deletes++
		}
	}
	if after != nil {
		res.Unchanged = after.Unchanged
	}
	return res, nil
}

// Export builds a redacted ConfigDocument of every keyed resource for the install.
// userID scopes user-owned resources (monitors, groups, notifications, proxies, maintenance).
func (s *ConfigService) Export(ctx context.Context, userID int64) (*ConfigDocument, error) {
	doc := &ConfigDocument{
		APIVersion: ConfigAPIVersion,
		Kind:       ConfigKind,
		Spec:       ConfigSpec{},
	}

	// Tags
	tagKeys, err := s.keys.ListByType(ctx, domain.ConfigResourceTag)
	if err != nil {
		return nil, fmt.Errorf("config export: list tag keys: %w", err)
	}
	for _, k := range tagKeys {
		t, err := s.tags.GetByID(ctx, k.ResourceID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		doc.Spec.Tags = append(doc.Spec.Tags, ConfigTag{Key: k.KeyName, Name: t.Name, Color: t.Color})
	}

	// Proxies
	proxyKeys, err := s.keys.ListByType(ctx, domain.ConfigResourceProxy)
	if err != nil {
		return nil, err
	}
	for _, k := range proxyKeys {
		p, err := s.proxies.GetByID(ctx, k.ResourceID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		if p.UserID != userID {
			continue
		}
		active := p.Active
		cp := ConfigProxy{
			Key: k.KeyName, Protocol: p.Protocol, Host: p.Host, Port: p.Port,
			Auth: p.Auth, Username: p.Username, Active: &active, IsDefault: p.IsDefault,
		}
		if p.Password != "" {
			cp.Password = ConfigSecretRedacted
		}
		doc.Spec.Proxies = append(doc.Spec.Proxies, cp)
	}

	// Notifications
	notifKeys, err := s.keys.ListByType(ctx, domain.ConfigResourceNotification)
	if err != nil {
		return nil, err
	}
	for _, k := range notifKeys {
		n, err := s.notifications.GetByID(ctx, k.ResourceID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		if n.UserID != userID {
			continue
		}
		active := n.Active
		includeAckURL := n.IncludeAckURL
		doc.Spec.Notifications = append(doc.Spec.Notifications, ConfigNotification{
			Key: k.KeyName, Name: n.Name, Type: n.Type, Active: &active,
			IsDefault: n.IsDefault, IncludeAckURL: &includeAckURL, Config: redactConfigMap(n.Config),
		})
	}

	// Groups
	groupKeys, err := s.keys.ListByType(ctx, domain.ConfigResourceMonitorGroup)
	if err != nil {
		return nil, err
	}
	idToGroupKey := map[int64]string{}
	for _, k := range groupKeys {
		idToGroupKey[k.ResourceID] = k.KeyName
	}
	for _, k := range groupKeys {
		g, err := s.groups.GetByID(ctx, k.ResourceID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		if g.UserID != userID {
			continue
		}
		cg := ConfigMonitorGroup{
			Key: k.KeyName, Name: g.Name, Description: g.Description, Owner: g.Owner,
			Condition: string(g.Condition), Threshold: g.Threshold,
			ThresholdIsPercent: g.ThresholdIsPercent, Weight: g.Weight, Collapsed: g.Collapsed,
		}
		if g.ParentID != nil {
			cg.Parent = idToGroupKey[*g.ParentID]
		}
		doc.Spec.MonitorGroups = append(doc.Spec.MonitorGroups, cg)
	}

	// Monitors
	monKeys, err := s.keys.ListByType(ctx, domain.ConfigResourceMonitor)
	if err != nil {
		return nil, err
	}
	idToMonKey := map[int64]string{}
	idToProxyKey := map[int64]string{}
	referencedProbes := map[string]domain.Probe{}
	for _, k := range monKeys {
		idToMonKey[k.ResourceID] = k.KeyName
	}
	for _, k := range proxyKeys {
		idToProxyKey[k.ResourceID] = k.KeyName
	}
	for _, k := range monKeys {
		m, err := s.monitors.GetByID(ctx, k.ResourceID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		if m.UserID != userID {
			continue
		}
		active := m.Active
		cm := ConfigMonitor{
			Key: k.KeyName, Name: m.Name, Description: m.Description, Owner: m.Owner,
			InheritGroupOwner: m.InheritGroupOwner, Type: m.Type,
			Active: &active, Interval: m.Interval, RetryInterval: m.RetryInterval,
			MaxRetries: m.MaxRetries, Timeout: m.Timeout, Config: redactConfigMap(m.Config),
			AcceptedStatusCodes: m.AcceptedStatusCodes, UpsideDown: m.UpsideDown,
			ResendInterval: m.ResendInterval, Weight: m.Weight, TLSIgnore: m.TLSIgnore,
			CertExpiryNotify: m.CertExpiryNotify,
		}
		if m.ProxyID != nil {
			cm.Proxy = idToProxyKey[*m.ProxyID]
		}
		if m.GroupID != nil {
			cm.Group = idToGroupKey[*m.GroupID]
		}
		// Desired vantage-point set by stable probe key. The set travels only
		// together with every member's probe declaration, so a document never
		// contains a dangling assignment reference. Connection material stays
		// out of the document entirely.
		if s.probeAssignments != nil && s.probeRegistry != nil {
			if set, err := s.probeAssignments.GetByMonitorID(ctx, m.ID); err == nil && len(set.Assignments) > 0 {
				members := make([]ConfigMonitorProbeAssignment, 0, len(set.Assignments))
				resolvable := true
				for _, member := range set.Assignments {
					key := domain.LocalProbeID
					if member.ProbeID != domain.LocalProbeID {
						p, err := s.probeRegistry.GetByID(ctx, member.ProbeID)
						if err != nil {
							resolvable = false
							break
						}
						key = p.Key
						referencedProbes[p.Key] = *p
					}
					a := ConfigMonitorProbeAssignment{Probe: key}
					if member.ResourceBinding != nil {
						a.BindingKey = member.ResourceBinding.BindingKey
						a.BindingKind = member.ResourceBinding.Kind
					}
					members = append(members, a)
				}
				if resolvable {
					sort.Slice(members, func(i, j int) bool { return members[i].Probe < members[j].Probe })
					cm.ProbeAssignments = members
					policy := set.HealthPolicy
					if policy == "" {
						policy = domain.HealthPolicyAnyDown
					}
					cm.HealthPolicy = string(policy)
				}
			}
		}
		doc.Spec.Monitors = append(doc.Spec.Monitors, cm)

		// Links for this monitor when both ends are keyed.
		if s.monitorTags != nil {
			mts, _ := s.monitorTags.ListByMonitor(ctx, m.ID)
			for _, mt := range mts {
				if tk, err := s.keys.GetByResource(ctx, domain.ConfigResourceTag, mt.TagID); err == nil {
					doc.Spec.MonitorTags = append(doc.Spec.MonitorTags, ConfigMonitorTag{
						Monitor: k.KeyName, Tag: tk.KeyName, Value: mt.Value,
					})
				}
			}
		}
		if s.monitorNotifs != nil {
			mns, _ := s.monitorNotifs.ListByMonitor(ctx, m.ID)
			for _, mn := range mns {
				if nk, err := s.keys.GetByResource(ctx, domain.ConfigResourceNotification, mn.NotificationID); err == nil {
					includeTarget := mn.IncludeTarget
					doc.Spec.MonitorNotifications = append(doc.Spec.MonitorNotifications, ConfigMonitorNotification{
						Monitor: k.KeyName, Notification: nk.KeyName, IncludeTarget: &includeTarget,
					})
				}
			}
		}
	}

	// Probe declarations for every referenced remote probe. `enabled` is always
	// explicit so a round trip never guesses an operator's intent.
	probeKeys := make([]string, 0, len(referencedProbes))
	for key := range referencedProbes {
		probeKeys = append(probeKeys, key)
	}
	sort.Strings(probeKeys)
	for _, key := range probeKeys {
		p := referencedProbes[key]
		enabled := p.Enabled
		doc.Spec.Probes = append(doc.Spec.Probes, ConfigProbe{
			Key: p.Key, Name: p.Name, Location: p.Location, Enabled: &enabled,
		})
	}

	// Group notifications
	if s.groupNotifs != nil {
		for _, k := range groupKeys {
			links, _ := s.groupNotifs.ListByGroup(ctx, k.ResourceID)
			for _, l := range links {
				if nk, err := s.keys.GetByResource(ctx, domain.ConfigResourceNotification, l.NotificationID); err == nil {
					doc.Spec.GroupNotifications = append(doc.Spec.GroupNotifications, ConfigGroupNotification{
						Group: k.KeyName, Notification: nk.KeyName,
					})
				}
			}
		}
	}

	// Status pages
	spKeys, err := s.keys.ListByType(ctx, domain.ConfigResourceStatusPage)
	if err != nil {
		return nil, err
	}
	idToSPKey := map[int64]string{}
	for _, k := range spKeys {
		idToSPKey[k.ResourceID] = k.KeyName
	}
	for _, k := range spKeys {
		sp, err := s.statusPages.GetByID(ctx, k.ResourceID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		pub := sp.Published
		csp := ConfigStatusPage{
			Key: k.KeyName, Slug: sp.Slug, Title: sp.Title, Description: sp.Description,
			Icon: sp.Icon, Theme: sp.Theme, Published: &pub, CustomDomain: sp.CustomDomain,
			FooterText: sp.FooterText, CustomCSS: sp.CustomCSS, DashboardStyle: sp.DashboardStyle,
			ShowTags: sp.ShowTags, AutoResolveIncidents: sp.AutoResolveIncidents, SLATarget: sp.SLATarget,
		}
		if sp.PasswordHash != "" {
			csp.AccessCode = ConfigSecretRedacted
		}
		doc.Spec.StatusPages = append(doc.Spec.StatusPages, csp)
		if s.spMonitors != nil {
			links, _ := s.spMonitors.ListByStatusPage(ctx, sp.ID)
			for _, l := range links {
				if mk, ok := idToMonKey[l.MonitorID]; ok {
					doc.Spec.StatusPageMonitors = append(doc.Spec.StatusPageMonitors, ConfigStatusPageMonitor{
						StatusPage: k.KeyName, Monitor: mk, DisplayOrder: l.DisplayOrder,
					})
				}
			}
		}
	}

	// Maintenance
	mwKeys, err := s.keys.ListByType(ctx, domain.ConfigResourceMaintenanceWindow)
	if err != nil {
		return nil, err
	}
	for _, k := range mwKeys {
		mw, err := s.maintenance.GetByID(ctx, k.ResourceID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		if mw.UserID != userID {
			continue
		}
		active := mw.Active
		cm := ConfigMaintenance{
			Key: k.KeyName, Title: mw.Title, Description: mw.Description, Active: &active,
			Strategy: mw.Strategy, CronExpr: mw.CronExpr, Duration: mw.Duration, Timezone: mw.Timezone,
		}
		if !mw.StartDate.IsZero() {
			cm.StartDate = mw.StartDate.UTC().Format(time.RFC3339)
		}
		if !mw.EndDate.IsZero() {
			cm.EndDate = mw.EndDate.UTC().Format(time.RFC3339)
		}
		doc.Spec.MaintenanceWindows = append(doc.Spec.MaintenanceWindows, cm)
		if s.maintMonitors != nil {
			ids, _ := s.maintMonitors.ListByMaintenance(ctx, mw.ID)
			for _, mid := range ids {
				if mk, ok := idToMonKey[mid]; ok {
					doc.Spec.MaintenanceMonitors = append(doc.Spec.MaintenanceMonitors, ConfigMaintenanceMonitor{
						Maintenance: k.KeyName, Monitor: mk,
					})
				}
			}
		}
	}

	return doc, nil
}

// --- validation ------------------------------------------------------------

func validateConfigDocument(doc *ConfigDocument) []string {
	var errs []string
	if doc == nil {
		return []string{"document is required"}
	}
	if doc.APIVersion != ConfigAPIVersion {
		errs = append(errs, fmt.Sprintf("unsupported apiVersion %q (want %q)", doc.APIVersion, ConfigAPIVersion))
	}
	if doc.Kind != "" && doc.Kind != ConfigKind {
		errs = append(errs, fmt.Sprintf("unsupported kind %q (want %q)", doc.Kind, ConfigKind))
	}
	seen := map[string]map[string]struct{}{}
	checkKey := func(kind, key string) {
		if key == "" {
			errs = append(errs, kind+": key is required")
			return
		}
		if !configKeyPattern.MatchString(key) {
			errs = append(errs, kind+": invalid key "+key)
			return
		}
		if seen[kind] == nil {
			seen[kind] = map[string]struct{}{}
		}
		if _, ok := seen[kind][key]; ok {
			errs = append(errs, kind+": duplicate key "+key)
		}
		seen[kind][key] = struct{}{}
	}
	for _, t := range doc.Spec.Tags {
		checkKey("tag", t.Key)
		if strings.TrimSpace(t.Name) == "" {
			errs = append(errs, "tag "+t.Key+": name is required")
		}
	}
	for _, p := range doc.Spec.Proxies {
		checkKey("proxy", p.Key)
		if p.Host == "" || p.Port <= 0 {
			errs = append(errs, "proxy "+p.Key+": host and port are required")
		}
	}
	for _, n := range doc.Spec.Notifications {
		checkKey("notification", n.Key)
		if n.Name == "" || n.Type == "" {
			errs = append(errs, "notification "+n.Key+": name and type are required")
		}
	}
	for _, p := range doc.Spec.Probes {
		// Probe identity is keyed natively: a lowercase slug, never reserved
		// `local` (that row exists on every install and is referenced directly).
		if p.Key == "" {
			errs = append(errs, "probe: key is required")
			continue
		}
		if p.Key == domain.LocalProbeID || !probeConfigKeyPattern.MatchString(p.Key) {
			errs = append(errs, "probe: invalid key "+p.Key)
			continue
		}
		if seen["probe"] == nil {
			seen["probe"] = map[string]struct{}{}
		}
		if _, ok := seen["probe"][p.Key]; ok {
			errs = append(errs, "probe: duplicate key "+p.Key)
		}
		seen["probe"][p.Key] = struct{}{}
		if strings.TrimSpace(p.Name) == "" {
			errs = append(errs, "probe "+p.Key+": name is required")
		}
	}
	for _, g := range doc.Spec.MonitorGroups {
		checkKey("monitor_group", g.Key)
		if g.Name == "" {
			errs = append(errs, "monitor_group "+g.Key+": name is required")
		}
	}
	for _, m := range doc.Spec.Monitors {
		checkKey("monitor", m.Key)
		if m.Name == "" || m.Type == "" {
			errs = append(errs, "monitor "+m.Key+": name and type are required")
		}
	}
	for _, sp := range doc.Spec.StatusPages {
		checkKey("status_page", sp.Key)
		if sp.Slug == "" || sp.Title == "" {
			errs = append(errs, "status_page "+sp.Key+": slug and title are required")
		}
	}
	for _, mw := range doc.Spec.MaintenanceWindows {
		checkKey("maintenance_window", mw.Key)
		if mw.Title == "" || mw.Strategy == "" {
			errs = append(errs, "maintenance_window "+mw.Key+": title and strategy are required")
		}
	}
	// Ref integrity (within document).
	has := func(kind, key string) bool {
		if key == "" {
			return true
		}
		_, ok := seen[kind][key]
		return ok
	}
	for _, m := range doc.Spec.Monitors {
		if m.Group != "" && !has("monitor_group", m.Group) {
			errs = append(errs, "monitor "+m.Key+": unknown group key "+m.Group)
		}
		if m.Proxy != "" && !has("proxy", m.Proxy) {
			errs = append(errs, "monitor "+m.Key+": unknown proxy key "+m.Proxy)
		}
		if m.HealthPolicy != "" && m.HealthPolicy != string(domain.HealthPolicyAnyDown) && m.HealthPolicy != string(domain.HealthPolicyAllDown) {
			errs = append(errs, "monitor "+m.Key+": unsupported health_policy "+m.HealthPolicy)
		}
		if len(m.ProbeAssignments) == 0 {
			continue
		}
		seenMembers := map[string]struct{}{}
		for _, a := range m.ProbeAssignments {
			if a.Probe == "" || (a.Probe != domain.LocalProbeID && !has("probe", a.Probe)) {
				errs = append(errs, "monitor "+m.Key+": unknown probe key "+a.Probe)
				continue
			}
			if _, ok := seenMembers[a.Probe]; ok {
				errs = append(errs, "monitor "+m.Key+": duplicate probe key "+a.Probe)
			}
			seenMembers[a.Probe] = struct{}{}
			bound := a.BindingKey != "" || a.BindingKind != ""
			// Mirror the assignment store's binding contract so a document fails
			// validation instead of half applying.
			if bound && (a.Probe == domain.LocalProbeID || !domain.ValidProbeResourceBinding(domain.ProbeResourceBinding{BindingKey: a.BindingKey, Kind: a.BindingKind})) {
				errs = append(errs, "monitor "+m.Key+": invalid resource binding for probe "+a.Probe)
			}
			if m.Type == "docker" && a.Probe != domain.LocalProbeID && !bound {
				errs = append(errs, "monitor "+m.Key+": docker assignment for probe "+a.Probe+" requires a resource binding")
			}
			if m.Type != "docker" && bound {
				errs = append(errs, "monitor "+m.Key+": resource bindings are only valid for docker monitors")
			}
			if m.Type == "push" && a.Probe != domain.LocalProbeID {
				errs = append(errs, "monitor "+m.Key+": push monitors cannot run remotely")
			}
		}
	}
	for _, g := range doc.Spec.MonitorGroups {
		if g.Parent != "" && !has("monitor_group", g.Parent) {
			errs = append(errs, "monitor_group "+g.Key+": unknown parent key "+g.Parent)
		}
	}
	for _, l := range doc.Spec.MonitorTags {
		if !has("monitor", l.Monitor) || !has("tag", l.Tag) {
			errs = append(errs, "monitor_tags: unknown monitor/tag ref")
		}
	}
	for _, l := range doc.Spec.MonitorNotifications {
		if !has("monitor", l.Monitor) || !has("notification", l.Notification) {
			errs = append(errs, "monitor_notifications: unknown monitor/notification ref")
		}
	}
	for _, l := range doc.Spec.GroupNotifications {
		if !has("monitor_group", l.Group) || !has("notification", l.Notification) {
			errs = append(errs, "group_notifications: unknown group/notification ref")
		}
	}
	for _, l := range doc.Spec.StatusPageMonitors {
		if !has("status_page", l.StatusPage) || !has("monitor", l.Monitor) {
			errs = append(errs, "status_page_monitors: unknown status_page/monitor ref")
		}
	}
	for _, l := range doc.Spec.MaintenanceMonitors {
		if !has("maintenance_window", l.Maintenance) || !has("monitor", l.Monitor) {
			errs = append(errs, "maintenance_monitors: unknown maintenance/monitor ref")
		}
	}
	return errs
}

// --- plan ------------------------------------------------------------------

func (s *ConfigService) buildPlan(ctx context.Context, userID int64, doc *ConfigDocument, opts ConfigApplyOptions, plan *ConfigPlan) error {
	add := func(kind, key string, action ConfigChangeAction) {
		plan.Changes = append(plan.Changes, ConfigChange{Kind: kind, Key: key, Action: action})
		switch action {
		case ConfigActionCreate:
			plan.Creates++
		case ConfigActionUpdate:
			plan.Updates++
		case ConfigActionDelete:
			plan.Deletes++
		case ConfigActionUnchanged:
			plan.Unchanged++
		}
	}

	// Probe declarations and assignment sets need the probe stores. Without
	// them the document cannot be honored, so refuse instead of half applying.
	declaresAssignments := false
	for _, m := range doc.Spec.Monitors {
		declaresAssignments = declaresAssignments || len(m.ProbeAssignments) > 0
	}
	if len(doc.Spec.Probes) > 0 && s.probeRegistry == nil {
		return fmt.Errorf("config plan: %w: probes are not available on this install", domain.ErrValidation)
	}
	if declaresAssignments && (s.probeRegistry == nil || s.probeAssignments == nil) {
		return fmt.Errorf("config plan: %w: monitor probe assignments are not available on this install", domain.ErrValidation)
	}

	// Tags
	docTagKeys := map[string]struct{}{}
	for _, t := range doc.Spec.Tags {
		docTagKeys[t.Key] = struct{}{}
		existing, err := s.keys.GetByKey(ctx, domain.ConfigResourceTag, t.Key)
		if isNotFound(err) {
			add("tag", t.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		cur, err := s.tags.GetByID(ctx, existing.ResourceID)
		if err != nil {
			return err
		}
		color := t.Color
		if color == "" {
			color = "#666666"
		}
		if cur.Name == t.Name && cur.Color == color {
			add("tag", t.Key, ConfigActionUnchanged)
		} else {
			add("tag", t.Key, ConfigActionUpdate)
		}
	}
	if opts.Prune {
		live, err := s.keys.ListByType(ctx, domain.ConfigResourceTag)
		if err != nil {
			return err
		}
		for _, k := range live {
			if _, ok := docTagKeys[k.KeyName]; !ok {
				add("tag", k.KeyName, ConfigActionDelete)
			}
		}
	}

	// Notifications
	docNotifKeys := map[string]struct{}{}
	for _, n := range doc.Spec.Notifications {
		docNotifKeys[n.Key] = struct{}{}
		existing, err := s.keys.GetByKey(ctx, domain.ConfigResourceNotification, n.Key)
		if isNotFound(err) {
			add("notification", n.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		cur, err := s.notifications.GetByID(ctx, existing.ResourceID)
		if err != nil {
			return err
		}
		if notificationEqual(cur, n) {
			add("notification", n.Key, ConfigActionUnchanged)
		} else {
			add("notification", n.Key, ConfigActionUpdate)
		}
	}
	if opts.Prune {
		live, err := s.keys.ListByType(ctx, domain.ConfigResourceNotification)
		if err != nil {
			return err
		}
		for _, k := range live {
			if _, ok := docNotifKeys[k.KeyName]; !ok {
				// Only prune user-owned
				if n, err := s.notifications.GetByID(ctx, k.ResourceID); err == nil && n.UserID == userID {
					add("notification", k.KeyName, ConfigActionDelete)
				}
			}
		}
	}

	// Proxies
	docProxyKeys := map[string]struct{}{}
	for _, p := range doc.Spec.Proxies {
		docProxyKeys[p.Key] = struct{}{}
		existing, err := s.keys.GetByKey(ctx, domain.ConfigResourceProxy, p.Key)
		if isNotFound(err) {
			add("proxy", p.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		cur, err := s.proxies.GetByID(ctx, existing.ResourceID)
		if err != nil {
			return err
		}
		if proxyEqual(cur, p) {
			add("proxy", p.Key, ConfigActionUnchanged)
		} else {
			add("proxy", p.Key, ConfigActionUpdate)
		}
	}
	if opts.Prune {
		live, err := s.keys.ListByType(ctx, domain.ConfigResourceProxy)
		if err != nil {
			return err
		}
		for _, k := range live {
			if _, ok := docProxyKeys[k.KeyName]; !ok {
				if p, err := s.proxies.GetByID(ctx, k.ResourceID); err == nil && p.UserID == userID {
					add("proxy", k.KeyName, ConfigActionDelete)
				}
			}
		}
	}

	// Probes. Identity is keyed natively by probe key (never in config_keys).
	// There is no prune pass on purpose: registrations are retained identities
	// with no delete path, and drift correction must never retire one.
	effectiveProbeEnabled := map[string]bool{}
	for _, p := range doc.Spec.Probes {
		existing, err := s.lookupProbe(ctx, p.Key)
		if err != nil && !isNotFound(err) {
			return err
		}
		enabled := true
		if existing != nil {
			enabled = existing.Enabled
		}
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		effectiveProbeEnabled[p.Key] = enabled
		if isNotFound(err) || existing == nil {
			add("probe", p.Key, ConfigActionCreate)
			continue
		}
		if existing.Name == p.Name && existing.Location == p.Location && existing.Enabled == enabled {
			add("probe", p.Key, ConfigActionUnchanged)
		} else {
			add("probe", p.Key, ConfigActionUpdate)
		}
	}

	// Groups
	docGroupKeys := map[string]struct{}{}
	for _, g := range doc.Spec.MonitorGroups {
		docGroupKeys[g.Key] = struct{}{}
		existing, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitorGroup, g.Key)
		if isNotFound(err) {
			add("monitor_group", g.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		cur, err := s.groups.GetByID(ctx, existing.ResourceID)
		if err != nil {
			return err
		}
		if groupEqual(cur, g, s, ctx) {
			add("monitor_group", g.Key, ConfigActionUnchanged)
		} else {
			add("monitor_group", g.Key, ConfigActionUpdate)
		}
	}
	if opts.Prune {
		live, err := s.keys.ListByType(ctx, domain.ConfigResourceMonitorGroup)
		if err != nil {
			return err
		}
		for _, k := range live {
			if _, ok := docGroupKeys[k.KeyName]; !ok {
				if g, err := s.groups.GetByID(ctx, k.ResourceID); err == nil && g.UserID == userID {
					add("monitor_group", k.KeyName, ConfigActionDelete)
				}
			}
		}
	}

	// Monitors
	docMonKeys := map[string]struct{}{}
	for _, m := range doc.Spec.Monitors {
		docMonKeys[m.Key] = struct{}{}
		existing, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitor, m.Key)
		if isNotFound(err) {
			add("monitor", m.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		cur, err := s.monitors.GetByID(ctx, existing.ResourceID)
		if err != nil {
			return err
		}
		if monitorEqual(cur, m, s, ctx) {
			add("monitor", m.Key, ConfigActionUnchanged)
		} else {
			add("monitor", m.Key, ConfigActionUpdate)
		}
	}
	if opts.Prune {
		live, err := s.keys.ListByType(ctx, domain.ConfigResourceMonitor)
		if err != nil {
			return err
		}
		for _, k := range live {
			if _, ok := docMonKeys[k.KeyName]; !ok {
				if m, err := s.monitors.GetByID(ctx, k.ResourceID); err == nil && m.UserID == userID {
					add("monitor", k.KeyName, ConfigActionDelete)
				}
			}
		}
	}

	// Monitor probe assignment sets. A set is managed only when the monitor
	// declares one: an omitted `probe_assignments` list is never rewritten, so a
	// document can never silently reroute a regional monitor to local execution.
	for _, m := range doc.Spec.Monitors {
		if len(m.ProbeAssignments) == 0 {
			continue
		}
		// An assignment set is only honored for probes that will be enabled
		// after this apply. Disabled members fail loudly here rather than
		// stranding a monitor on an inert set.
		for _, a := range m.ProbeAssignments {
			if a.Probe != domain.LocalProbeID && !effectiveProbeEnabled[a.Probe] {
				err := fmt.Sprintf("monitor %s: probe %s is disabled; enable and enroll it before assigning", m.Key, a.Probe)
				plan.Errors = append(plan.Errors, err)
				plan.Valid = false
			}
		}
		ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitor, m.Key)
		if isNotFound(err) {
			add("probe_assignment", m.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		current, err := s.probeAssignments.GetByMonitorID(ctx, ck.ResourceID)
		if isNotFound(err) {
			add("probe_assignment", m.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		ids, bindings, policy, err := s.resolveDocumentSet(ctx, m)
		if err != nil {
			return err
		}
		if probeAssignmentSetEqual(current, ids, policy, bindings) {
			add("probe_assignment", m.Key, ConfigActionUnchanged)
		} else {
			add("probe_assignment", m.Key, ConfigActionUpdate)
		}
	}

	// Status pages
	docSPKeys := map[string]struct{}{}
	for _, sp := range doc.Spec.StatusPages {
		docSPKeys[sp.Key] = struct{}{}
		existing, err := s.keys.GetByKey(ctx, domain.ConfigResourceStatusPage, sp.Key)
		if isNotFound(err) {
			add("status_page", sp.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		cur, err := s.statusPages.GetByID(ctx, existing.ResourceID)
		if err != nil {
			return err
		}
		if statusPageEqual(cur, sp) {
			add("status_page", sp.Key, ConfigActionUnchanged)
		} else {
			add("status_page", sp.Key, ConfigActionUpdate)
		}
	}
	if opts.Prune {
		live, err := s.keys.ListByType(ctx, domain.ConfigResourceStatusPage)
		if err != nil {
			return err
		}
		for _, k := range live {
			if _, ok := docSPKeys[k.KeyName]; !ok {
				add("status_page", k.KeyName, ConfigActionDelete)
			}
		}
	}

	// Maintenance
	docMWKeys := map[string]struct{}{}
	for _, mw := range doc.Spec.MaintenanceWindows {
		docMWKeys[mw.Key] = struct{}{}
		existing, err := s.keys.GetByKey(ctx, domain.ConfigResourceMaintenanceWindow, mw.Key)
		if isNotFound(err) {
			add("maintenance_window", mw.Key, ConfigActionCreate)
			continue
		}
		if err != nil {
			return err
		}
		cur, err := s.maintenance.GetByID(ctx, existing.ResourceID)
		if err != nil {
			return err
		}
		if maintenanceEqual(cur, mw) {
			add("maintenance_window", mw.Key, ConfigActionUnchanged)
		} else {
			add("maintenance_window", mw.Key, ConfigActionUpdate)
		}
	}
	if opts.Prune {
		live, err := s.keys.ListByType(ctx, domain.ConfigResourceMaintenanceWindow)
		if err != nil {
			return err
		}
		for _, k := range live {
			if _, ok := docMWKeys[k.KeyName]; !ok {
				if mw, err := s.maintenance.GetByID(ctx, k.ResourceID); err == nil && mw.UserID == userID {
					add("maintenance_window", k.KeyName, ConfigActionDelete)
				}
			}
		}
	}

	sort.Slice(plan.Changes, func(i, j int) bool {
		if plan.Changes[i].Kind != plan.Changes[j].Kind {
			return plan.Changes[i].Kind < plan.Changes[j].Kind
		}
		return plan.Changes[i].Key < plan.Changes[j].Key
	})
	return nil
}

// --- apply -----------------------------------------------------------------

func (s *ConfigService) applyPlan(ctx context.Context, userID int64, doc *ConfigDocument, plan *ConfigPlan, applied *[]ConfigChange) error {
	// Index desired by kind+key
	tagsByKey := map[string]ConfigTag{}
	for _, t := range doc.Spec.Tags {
		tagsByKey[t.Key] = t
	}
	notifsByKey := map[string]ConfigNotification{}
	for _, n := range doc.Spec.Notifications {
		notifsByKey[n.Key] = n
	}
	proxiesByKey := map[string]ConfigProxy{}
	for _, p := range doc.Spec.Proxies {
		proxiesByKey[p.Key] = p
	}
	groupsByKey := map[string]ConfigMonitorGroup{}
	for _, g := range doc.Spec.MonitorGroups {
		groupsByKey[g.Key] = g
	}
	monsByKey := map[string]ConfigMonitor{}
	for _, m := range doc.Spec.Monitors {
		monsByKey[m.Key] = m
	}
	spsByKey := map[string]ConfigStatusPage{}
	for _, sp := range doc.Spec.StatusPages {
		spsByKey[sp.Key] = sp
	}
	mwsByKey := map[string]ConfigMaintenance{}
	for _, mw := range doc.Spec.MaintenanceWindows {
		mwsByKey[mw.Key] = mw
	}
	probesByKey := map[string]ConfigProbe{}
	for _, p := range doc.Spec.Probes {
		probesByKey[p.Key] = p
	}

	// Deletes first (reverse dependency order: links handled implicitly by cascades / re-sync)
	for _, c := range plan.Changes {
		if c.Action != ConfigActionDelete {
			continue
		}
		if err := s.applyDelete(ctx, c); err != nil {
			return err
		}
		*applied = append(*applied, c)
	}

	// Creates/updates: tags, proxies, notifications, groups (parents first), monitors, status pages, maintenance
	for _, c := range plan.Changes {
		if c.Action != ConfigActionCreate && c.Action != ConfigActionUpdate {
			continue
		}
		switch c.Kind {
		case "tag":
			if err := s.applyTag(ctx, tagsByKey[c.Key], c.Action); err != nil {
				return err
			}
		case "probe":
			if err := s.applyProbe(ctx, probesByKey[c.Key], c.Action); err != nil {
				return err
			}
		case "proxy":
			if err := s.applyProxy(ctx, userID, proxiesByKey[c.Key], c.Action); err != nil {
				return err
			}
		case "notification":
			if err := s.applyNotification(ctx, userID, notifsByKey[c.Key], c.Action); err != nil {
				return err
			}
		case "status_page":
			if err := s.applyStatusPage(ctx, spsByKey[c.Key], c.Action); err != nil {
				return err
			}
		case "maintenance_window":
			if err := s.applyMaintenance(ctx, userID, mwsByKey[c.Key], c.Action); err != nil {
				return err
			}
		}
		if c.Kind != "monitor_group" && c.Kind != "monitor" {
			*applied = append(*applied, c)
		}
	}

	// Groups in dependency order
	for _, g := range orderConfigGroups(doc.Spec.MonitorGroups) {
		for _, c := range plan.Changes {
			if c.Kind == "monitor_group" && c.Key == g.Key && (c.Action == ConfigActionCreate || c.Action == ConfigActionUpdate) {
				if err := s.applyGroup(ctx, userID, g, c.Action); err != nil {
					return err
				}
				*applied = append(*applied, c)
			}
		}
	}

	// Monitors
	for _, c := range plan.Changes {
		if c.Kind != "monitor" || (c.Action != ConfigActionCreate && c.Action != ConfigActionUpdate) {
			continue
		}
		if err := s.applyMonitor(ctx, userID, monsByKey[c.Key], c.Action); err != nil {
			return err
		}
		*applied = append(*applied, c)
	}

	// Reconcile relationship links for resources in the document.
	if err := s.syncLinks(ctx, doc); err != nil {
		return err
	}
	// Reconcile declared monitor vantage-point sets last: probes referenced by
	// them are already created or updated above.
	if err := s.syncProbeAssignments(ctx, doc); err != nil {
		return err
	}
	return nil
}

func (s *ConfigService) applyDelete(ctx context.Context, c ConfigChange) error {
	k, err := s.keys.GetByKey(ctx, resourceTypeForKind(c.Kind), c.Key)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	switch c.Kind {
	case "tag":
		_ = s.tags.Delete(ctx, k.ResourceID)
	case "proxy":
		_ = s.proxies.Delete(ctx, k.ResourceID)
	case "notification":
		_ = s.notifications.Delete(ctx, k.ResourceID)
	case "monitor_group":
		_ = s.groups.Delete(ctx, k.ResourceID)
	case "monitor":
		_ = s.monitors.Delete(ctx, k.ResourceID)
	case "status_page":
		_ = s.statusPages.Delete(ctx, k.ResourceID)
	case "maintenance_window":
		_ = s.maintenance.Delete(ctx, k.ResourceID)
	}
	return s.keys.DeleteByKey(ctx, resourceTypeForKind(c.Kind), c.Key)
}

func (s *ConfigService) applyTag(ctx context.Context, t ConfigTag, action ConfigChangeAction) error {
	color := t.Color
	if color == "" {
		color = "#666666"
	}
	if action == ConfigActionCreate {
		tag := &domain.Tag{Name: t.Name, Color: color}
		if err := s.tags.Create(ctx, tag); err != nil {
			return fmt.Errorf("create tag %s: %w", t.Key, err)
		}
		return s.keys.Upsert(ctx, &domain.ConfigKey{ResourceType: domain.ConfigResourceTag, KeyName: t.Key, ResourceID: tag.ID})
	}
	ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceTag, t.Key)
	if err != nil {
		return err
	}
	tag, err := s.tags.GetByID(ctx, ck.ResourceID)
	if err != nil {
		return err
	}
	tag.Name = t.Name
	tag.Color = color
	if err := s.tags.Update(ctx, tag); err != nil {
		return err
	}
	return nil
}

func (s *ConfigService) applyProxy(ctx context.Context, userID int64, p ConfigProxy, action ConfigChangeAction) error {
	active := true
	if p.Active != nil {
		active = *p.Active
	}
	if action == ConfigActionCreate {
		proxy := &domain.Proxy{
			UserID: userID, Protocol: p.Protocol, Host: p.Host, Port: p.Port,
			Auth: p.Auth, Username: p.Username, Password: p.Password,
			Active: active, IsDefault: p.IsDefault,
		}
		if err := s.proxies.Create(ctx, proxy); err != nil {
			return fmt.Errorf("create proxy %s: %w", p.Key, err)
		}
		return s.keys.Upsert(ctx, &domain.ConfigKey{ResourceType: domain.ConfigResourceProxy, KeyName: p.Key, ResourceID: proxy.ID})
	}
	ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceProxy, p.Key)
	if err != nil {
		return err
	}
	cur, err := s.proxies.GetByID(ctx, ck.ResourceID)
	if err != nil {
		return err
	}
	cur.Protocol = p.Protocol
	cur.Host = p.Host
	cur.Port = p.Port
	cur.Auth = p.Auth
	cur.Username = p.Username
	cur.Active = active
	cur.IsDefault = p.IsDefault
	if p.Password != "" && p.Password != ConfigSecretRedacted {
		cur.Password = p.Password
	}
	return s.proxies.Update(ctx, cur)
}

func (s *ConfigService) applyNotification(ctx context.Context, userID int64, n ConfigNotification, action ConfigChangeAction) error {
	active := true
	if n.Active != nil {
		active = *n.Active
	}
	includeAckURL := configIncludeAckURL(n)
	if action == ConfigActionCreate {
		cfg := n.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		notif := &domain.Notification{
			UserID: userID, Name: n.Name, Type: n.Type, Active: active,
			IsDefault: n.IsDefault, IncludeAckURL: includeAckURL, Config: stripRedacted(cfg),
		}
		if err := s.notifications.Create(ctx, notif); err != nil {
			return fmt.Errorf("create notification %s: %w", n.Key, err)
		}
		return s.keys.Upsert(ctx, &domain.ConfigKey{ResourceType: domain.ConfigResourceNotification, KeyName: n.Key, ResourceID: notif.ID})
	}
	ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceNotification, n.Key)
	if err != nil {
		return err
	}
	cur, err := s.notifications.GetByID(ctx, ck.ResourceID)
	if err != nil {
		return err
	}
	cur.Name = n.Name
	cur.Type = n.Type
	cur.Active = active
	cur.IsDefault = n.IsDefault
	cur.IncludeAckURL = includeAckURL
	cur.Config = mergeConfigMaps(cur.Config, n.Config)
	return s.notifications.Update(ctx, cur)
}

func (s *ConfigService) applyGroup(ctx context.Context, userID int64, g ConfigMonitorGroup, action ConfigChangeAction) error {
	var parentID *int64
	if g.Parent != "" {
		pk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitorGroup, g.Parent)
		if err != nil {
			return fmt.Errorf("group %s parent %s: %w", g.Key, g.Parent, err)
		}
		parentID = &pk.ResourceID
	}
	cond := domain.GroupCondition(g.Condition)
	if cond == "" {
		cond = domain.GroupConditionWorstOfChildren
	}
	if action == ConfigActionCreate {
		grp := &domain.MonitorGroup{
			UserID: userID, Name: g.Name, Description: g.Description, Owner: g.Owner, ParentID: parentID,
			Condition: cond, Threshold: g.Threshold, ThresholdIsPercent: g.ThresholdIsPercent,
			Weight: g.Weight, Collapsed: g.Collapsed,
		}
		if err := s.groups.Create(ctx, grp); err != nil {
			return fmt.Errorf("create group %s: %w", g.Key, err)
		}
		return s.keys.Upsert(ctx, &domain.ConfigKey{ResourceType: domain.ConfigResourceMonitorGroup, KeyName: g.Key, ResourceID: grp.ID})
	}
	ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitorGroup, g.Key)
	if err != nil {
		return err
	}
	cur, err := s.groups.GetByID(ctx, ck.ResourceID)
	if err != nil {
		return err
	}
	cur.Name = g.Name
	cur.Description = g.Description
	cur.Owner = g.Owner
	cur.ParentID = parentID
	cur.Condition = cond
	cur.Threshold = g.Threshold
	cur.ThresholdIsPercent = g.ThresholdIsPercent
	cur.Weight = g.Weight
	cur.Collapsed = g.Collapsed
	return s.groups.Update(ctx, cur)
}

func (s *ConfigService) applyMonitor(ctx context.Context, userID int64, m ConfigMonitor, action ConfigChangeAction) error {
	active := true
	if m.Active != nil {
		active = *m.Active
	}
	interval := m.Interval
	if interval <= 0 {
		interval = 60
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	cfg := m.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	var proxyID, groupID *int64
	if m.Proxy != "" {
		pk, err := s.keys.GetByKey(ctx, domain.ConfigResourceProxy, m.Proxy)
		if err != nil {
			return fmt.Errorf("monitor %s proxy %s: %w", m.Key, m.Proxy, err)
		}
		proxyID = &pk.ResourceID
	}
	if m.Group != "" {
		gk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitorGroup, m.Group)
		if err != nil {
			return fmt.Errorf("monitor %s group %s: %w", m.Key, m.Group, err)
		}
		groupID = &gk.ResourceID
	}
	if action == ConfigActionCreate {
		mon := &domain.Monitor{
			UserID: userID, Name: m.Name, Description: m.Description, Owner: m.Owner,
			InheritGroupOwner: m.InheritGroupOwner, Type: m.Type,
			Active: active, Interval: interval, RetryInterval: m.RetryInterval,
			MaxRetries: m.MaxRetries, Timeout: timeout, Config: stripRedacted(cfg),
			AcceptedStatusCodes: m.AcceptedStatusCodes, ProxyID: proxyID, GroupID: groupID,
			UpsideDown: m.UpsideDown, ResendInterval: m.ResendInterval, Weight: m.Weight,
			TLSIgnore: m.TLSIgnore, CertExpiryNotify: m.CertExpiryNotify,
		}
		if err := s.monitors.Create(ctx, mon); err != nil {
			return fmt.Errorf("create monitor %s: %w", m.Key, err)
		}
		return s.keys.Upsert(ctx, &domain.ConfigKey{ResourceType: domain.ConfigResourceMonitor, KeyName: m.Key, ResourceID: mon.ID})
	}
	ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitor, m.Key)
	if err != nil {
		return err
	}
	cur, err := s.monitors.GetByID(ctx, ck.ResourceID)
	if err != nil {
		return err
	}
	cur.Name = m.Name
	cur.Description = m.Description
	cur.Owner = m.Owner
	cur.InheritGroupOwner = m.InheritGroupOwner
	cur.Type = m.Type
	cur.Active = active
	cur.Interval = interval
	cur.RetryInterval = m.RetryInterval
	cur.MaxRetries = m.MaxRetries
	cur.Timeout = timeout
	cur.Config = mergeConfigMaps(cur.Config, cfg)
	cur.AcceptedStatusCodes = m.AcceptedStatusCodes
	cur.ProxyID = proxyID
	cur.GroupID = groupID
	cur.UpsideDown = m.UpsideDown
	cur.ResendInterval = m.ResendInterval
	cur.Weight = m.Weight
	cur.TLSIgnore = m.TLSIgnore
	cur.CertExpiryNotify = m.CertExpiryNotify
	return s.monitors.Update(ctx, cur)
}

func (s *ConfigService) applyStatusPage(ctx context.Context, sp ConfigStatusPage, action ConfigChangeAction) error {
	published := true
	if sp.Published != nil {
		published = *sp.Published
	}
	if action == ConfigActionCreate {
		page := &domain.StatusPage{
			Slug: sp.Slug, Title: sp.Title, Description: sp.Description, Icon: sp.Icon,
			Theme: sp.Theme, Published: published, CustomDomain: sp.CustomDomain,
			FooterText: sp.FooterText, CustomCSS: sp.CustomCSS, DashboardStyle: sp.DashboardStyle,
			ShowTags: sp.ShowTags, AutoResolveIncidents: sp.AutoResolveIncidents, SLATarget: sp.SLATarget,
		}
		if sp.AccessCode != "" && sp.AccessCode != ConfigSecretRedacted && s.password != nil {
			hash, err := s.password.Hash(sp.AccessCode)
			if err != nil {
				return err
			}
			page.PasswordHash = hash
		}
		if err := s.statusPages.Create(ctx, page); err != nil {
			return fmt.Errorf("create status_page %s: %w", sp.Key, err)
		}
		return s.keys.Upsert(ctx, &domain.ConfigKey{ResourceType: domain.ConfigResourceStatusPage, KeyName: sp.Key, ResourceID: page.ID})
	}
	ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceStatusPage, sp.Key)
	if err != nil {
		return err
	}
	cur, err := s.statusPages.GetByID(ctx, ck.ResourceID)
	if err != nil {
		return err
	}
	cur.Slug = sp.Slug
	cur.Title = sp.Title
	cur.Description = sp.Description
	cur.Icon = sp.Icon
	cur.Theme = sp.Theme
	cur.Published = published
	cur.CustomDomain = sp.CustomDomain
	cur.FooterText = sp.FooterText
	cur.CustomCSS = sp.CustomCSS
	cur.DashboardStyle = sp.DashboardStyle
	cur.ShowTags = sp.ShowTags
	cur.AutoResolveIncidents = sp.AutoResolveIncidents
	cur.SLATarget = sp.SLATarget
	if sp.AccessCode != "" && sp.AccessCode != ConfigSecretRedacted && s.password != nil {
		hash, err := s.password.Hash(sp.AccessCode)
		if err != nil {
			return err
		}
		cur.PasswordHash = hash
	}
	return s.statusPages.Update(ctx, cur)
}

func (s *ConfigService) applyMaintenance(ctx context.Context, userID int64, mw ConfigMaintenance, action ConfigChangeAction) error {
	active := true
	if mw.Active != nil {
		active = *mw.Active
	}
	tz := mw.Timezone
	if tz == "" {
		tz = "UTC"
	}
	start, end := time.Time{}, time.Time{}
	if mw.StartDate != "" {
		t, err := time.Parse(time.RFC3339, mw.StartDate)
		if err != nil {
			return fmt.Errorf("maintenance %s start_date: %w", mw.Key, err)
		}
		start = t.UTC()
	}
	if mw.EndDate != "" {
		t, err := time.Parse(time.RFC3339, mw.EndDate)
		if err != nil {
			return fmt.Errorf("maintenance %s end_date: %w", mw.Key, err)
		}
		end = t.UTC()
	}
	if action == ConfigActionCreate {
		win := &domain.MaintenanceWindow{
			UserID: userID, Title: mw.Title, Description: mw.Description, Active: active,
			Strategy: mw.Strategy, StartDate: start, EndDate: end, CronExpr: mw.CronExpr,
			Duration: mw.Duration, Timezone: tz,
		}
		if err := s.maintenance.Create(ctx, win); err != nil {
			return fmt.Errorf("create maintenance %s: %w", mw.Key, err)
		}
		return s.keys.Upsert(ctx, &domain.ConfigKey{ResourceType: domain.ConfigResourceMaintenanceWindow, KeyName: mw.Key, ResourceID: win.ID})
	}
	ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceMaintenanceWindow, mw.Key)
	if err != nil {
		return err
	}
	cur, err := s.maintenance.GetByID(ctx, ck.ResourceID)
	if err != nil {
		return err
	}
	cur.Title = mw.Title
	cur.Description = mw.Description
	cur.Active = active
	cur.Strategy = mw.Strategy
	cur.StartDate = start
	cur.EndDate = end
	cur.CronExpr = mw.CronExpr
	cur.Duration = mw.Duration
	cur.Timezone = tz
	return s.maintenance.Update(ctx, cur)
}

func (s *ConfigService) syncLinks(ctx context.Context, doc *ConfigDocument) error {
	// Monitor tags: for each monitor in doc, set the tags listed for it.
	monTagDesired := map[string][]ConfigMonitorTag{}
	for _, l := range doc.Spec.MonitorTags {
		monTagDesired[l.Monitor] = append(monTagDesired[l.Monitor], l)
	}
	for _, m := range doc.Spec.Monitors {
		mk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitor, m.Key)
		if err != nil {
			continue
		}
		desired := monTagDesired[m.Key]
		// Current
		cur, _ := s.monitorTags.ListByMonitor(ctx, mk.ResourceID)
		curSet := map[int64]string{}
		for _, mt := range cur {
			curSet[mt.TagID] = mt.Value
		}
		wantSet := map[int64]string{}
		for _, d := range desired {
			tk, err := s.keys.GetByKey(ctx, domain.ConfigResourceTag, d.Tag)
			if err != nil {
				return err
			}
			wantSet[tk.ResourceID] = d.Value
			if v, ok := curSet[tk.ResourceID]; !ok || v != d.Value {
				_ = s.monitorTags.Remove(ctx, mk.ResourceID, tk.ResourceID)
				if err := s.monitorTags.Assign(ctx, mk.ResourceID, tk.ResourceID, d.Value); err != nil {
					return err
				}
			}
		}
		for tagID := range curSet {
			if _, ok := wantSet[tagID]; !ok {
				_ = s.monitorTags.Remove(ctx, mk.ResourceID, tagID)
			}
		}
	}

	// Monitor notifications
	monNotifDesired := map[string][]ConfigMonitorNotification{}
	for _, l := range doc.Spec.MonitorNotifications {
		monNotifDesired[l.Monitor] = append(monNotifDesired[l.Monitor], l)
	}
	for _, m := range doc.Spec.Monitors {
		mk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitor, m.Key)
		if err != nil {
			continue
		}
		want := map[int64]bool{}
		for _, l := range monNotifDesired[m.Key] {
			nck, err := s.keys.GetByKey(ctx, domain.ConfigResourceNotification, l.Notification)
			if err != nil {
				return err
			}
			want[nck.ResourceID] = true
			includeTarget := configMonitorNotifIncludeTarget(l)
			_ = s.monitorNotifs.Attach(ctx, mk.ResourceID, nck.ResourceID, includeTarget)
			// Reconcile the flag on links that already existed.
			_ = s.monitorNotifs.SetIncludeTarget(ctx, mk.ResourceID, nck.ResourceID, includeTarget)
		}
		if cur, err := s.monitorNotifs.ListByMonitor(ctx, mk.ResourceID); err == nil {
			for _, link := range cur {
				if !want[link.NotificationID] {
					_ = s.monitorNotifs.Detach(ctx, mk.ResourceID, link.NotificationID)
				}
			}
		}
	}

	// Group notifications
	if s.groupNotifs != nil {
		grpNotifDesired := map[string][]string{}
		for _, l := range doc.Spec.GroupNotifications {
			grpNotifDesired[l.Group] = append(grpNotifDesired[l.Group], l.Notification)
		}
		for _, g := range doc.Spec.MonitorGroups {
			gk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitorGroup, g.Key)
			if err != nil {
				continue
			}
			want := map[int64]struct{}{}
			for _, nk := range grpNotifDesired[g.Key] {
				nck, err := s.keys.GetByKey(ctx, domain.ConfigResourceNotification, nk)
				if err != nil {
					return err
				}
				want[nck.ResourceID] = struct{}{}
				_ = s.groupNotifs.Attach(ctx, gk.ResourceID, nck.ResourceID)
			}
			if cur, err := s.groupNotifs.ListByGroup(ctx, gk.ResourceID); err == nil {
				for _, link := range cur {
					if _, ok := want[link.NotificationID]; !ok {
						_ = s.groupNotifs.Detach(ctx, gk.ResourceID, link.NotificationID)
					}
				}
			}
		}
	}

	// Status page monitors — replace set per page in doc
	if s.spMonitors != nil {
		spMonDesired := map[string][]ConfigStatusPageMonitor{}
		for _, l := range doc.Spec.StatusPageMonitors {
			spMonDesired[l.StatusPage] = append(spMonDesired[l.StatusPage], l)
		}
		for _, sp := range doc.Spec.StatusPages {
			spk, err := s.keys.GetByKey(ctx, domain.ConfigResourceStatusPage, sp.Key)
			if err != nil {
				continue
			}
			// Build ordered list of monitor IDs
			desired := spMonDesired[sp.Key]
			sort.Slice(desired, func(i, j int) bool {
				return desired[i].DisplayOrder < desired[j].DisplayOrder
			})
			// Clear and re-add (ReorderMonitors-style): remove all then add
			if cur, err := s.spMonitors.ListByStatusPage(ctx, spk.ResourceID); err == nil {
				for _, l := range cur {
					_ = s.spMonitors.RemoveMonitor(ctx, spk.ResourceID, l.MonitorID)
				}
			}
			for i, d := range desired {
				mk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitor, d.Monitor)
				if err != nil {
					return err
				}
				order := d.DisplayOrder
				if order == 0 {
					order = i
				}
				if err := s.spMonitors.AddMonitor(ctx, spk.ResourceID, mk.ResourceID, order); err != nil {
					return err
				}
			}
		}
	}

	// Maintenance monitors
	if s.maintMonitors != nil {
		mmDesired := map[string][]string{}
		for _, l := range doc.Spec.MaintenanceMonitors {
			mmDesired[l.Maintenance] = append(mmDesired[l.Maintenance], l.Monitor)
		}
		for _, mw := range doc.Spec.MaintenanceWindows {
			mwk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMaintenanceWindow, mw.Key)
			if err != nil {
				continue
			}
			want := map[int64]struct{}{}
			for _, monKey := range mmDesired[mw.Key] {
				mk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitor, monKey)
				if err != nil {
					return err
				}
				want[mk.ResourceID] = struct{}{}
				_ = s.maintMonitors.Assign(ctx, mwk.ResourceID, mk.ResourceID)
			}
			if cur, err := s.maintMonitors.ListByMaintenance(ctx, mwk.ResourceID); err == nil {
				for _, mid := range cur {
					if _, ok := want[mid]; !ok {
						_ = s.maintMonitors.Remove(ctx, mwk.ResourceID, mid)
					}
				}
			}
		}
	}
	return nil
}

// --- probe declarations and assignment sets -------------------------------

// lookupProbe resolves one stable probe key. Missing is (nil, ErrNotFound);
// an unwired registry is a typed validation error, never a silent no-op.
func (s *ConfigService) lookupProbe(ctx context.Context, key string) (*domain.Probe, error) {
	if s.probeRegistry == nil {
		return nil, fmt.Errorf("probes are not available on this install: %w", domain.ErrValidation)
	}
	return s.probeRegistry.GetByKey(ctx, key)
}

// applyProbe upserts one registration by stable key. Identity (id, key, kind)
// is immutable and connection material is never part of the document: a newly
// created registration is inert until an operator registers and enrolls it.
func (s *ConfigService) applyProbe(ctx context.Context, p ConfigProbe, action ConfigChangeAction) error {
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	if action == ConfigActionCreate {
		probe := &domain.Probe{
			Key: p.Key, Name: p.Name, Location: p.Location,
			Kind: domain.ProbeKindRemote, Enabled: enabled,
		}
		if err := s.probeRegistry.Create(ctx, probe); err != nil {
			return fmt.Errorf("create probe %s: %w", p.Key, err)
		}
		return nil
	}
	cur, err := s.probeRegistry.GetByKey(ctx, p.Key)
	if err != nil {
		return fmt.Errorf("update probe %s: %w", p.Key, err)
	}
	cur.Name = p.Name
	cur.Location = p.Location
	cur.Enabled = enabled
	if err := s.probeRegistry.Update(ctx, cur, cur.Revision); err != nil {
		return fmt.Errorf("update probe %s: %w", p.Key, err)
	}
	return nil
}

// resolveDocumentSet maps one declared assignment set onto live probe IDs,
// bindings and policy. Members are already validated against the document.
func (s *ConfigService) resolveDocumentSet(ctx context.Context, m ConfigMonitor) ([]string, []domain.ProbeAssignmentBinding, domain.HealthPolicy, error) {
	policy := domain.HealthPolicyAnyDown
	if m.HealthPolicy != "" {
		policy = domain.HealthPolicy(m.HealthPolicy)
	}
	ids := make([]string, 0, len(m.ProbeAssignments))
	bindings := []domain.ProbeAssignmentBinding{}
	for _, a := range m.ProbeAssignments {
		id := domain.LocalProbeID
		if a.Probe != domain.LocalProbeID {
			p, err := s.lookupProbe(ctx, a.Probe)
			if err != nil {
				return nil, nil, "", fmt.Errorf("monitor %s probe %s: %w", m.Key, a.Probe, err)
			}
			id = p.ID
		}
		ids = append(ids, id)
		if a.BindingKey != "" || a.BindingKind != "" {
			bindings = append(bindings, domain.ProbeAssignmentBinding{
				ProbeID:              id,
				ProbeResourceBinding: domain.ProbeResourceBinding{BindingKey: a.BindingKey, Kind: a.BindingKind},
			})
		}
	}
	return ids, bindings, policy, nil
}

// syncProbeAssignments commits every declared monitor set as one complete
// desired set. Sets are managed only for monitors that declare one; members
// must be enabled at commit time (the plan refuses disabled members) and no
// assignment is ever invented for a monitor the document leaves unmanaged.
func (s *ConfigService) syncProbeAssignments(ctx context.Context, doc *ConfigDocument) error {
	declares := false
	for _, m := range doc.Spec.Monitors {
		declares = declares || len(m.ProbeAssignments) > 0
	}
	if !declares {
		return nil
	}
	if s.probeRegistry == nil || s.probeAssignments == nil {
		return fmt.Errorf("config apply: %w: monitor probe assignments are not available on this install", domain.ErrValidation)
	}
	for _, m := range doc.Spec.Monitors {
		if len(m.ProbeAssignments) == 0 {
			continue
		}
		ck, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitor, m.Key)
		if err != nil {
			return fmt.Errorf("monitor %s: %w", m.Key, err)
		}
		ids, bindings, policy, err := s.resolveDocumentSet(ctx, m)
		if err != nil {
			return err
		}
		// Defense in depth: re-check the live registration before writing the
		// set. A disabled member must never be handed new work, even if the
		// plan was computed against stale state.
		for _, a := range m.ProbeAssignments {
			if a.Probe == domain.LocalProbeID {
				continue
			}
			p, err := s.lookupProbe(ctx, a.Probe)
			if err != nil {
				return fmt.Errorf("monitor %s probe %s: %w", m.Key, a.Probe, err)
			}
			if !p.Enabled {
				return fmt.Errorf("monitor %s: probe %s is disabled: %w", m.Key, a.Probe, domain.ErrValidation)
			}
		}
		revision := int64(1)
		if current, err := s.probeAssignments.GetByMonitorID(ctx, ck.ResourceID); err == nil {
			revision = current.Revision
			if probeAssignmentSetEqual(current, ids, policy, bindings) {
				continue
			}
		} else if !isNotFound(err) {
			return err
		}
		if _, err := s.probeAssignments.Restore(ctx, ck.ResourceID, revision, ids, policy, bindings); err != nil {
			return fmt.Errorf("monitor %s probe assignments: %w", m.Key, err)
		}
	}
	return nil
}

// probeAssignmentSetEqual compares a live set against one declared set,
// ignoring order and generation (generations are runtime state).
func probeAssignmentSetEqual(current *domain.MonitorProbeAssignments, ids []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) bool {
	if current == nil || current.HealthPolicy != policy || len(current.Assignments) != len(ids) {
		return false
	}
	want := map[string]domain.ProbeResourceBinding{}
	for _, id := range ids {
		want[id] = domain.ProbeResourceBinding{}
	}
	for _, b := range bindings {
		want[b.ProbeID] = b.ProbeResourceBinding
	}
	for _, a := range current.Assignments {
		wantBinding, ok := want[a.ProbeID]
		if !ok {
			return false
		}
		var have domain.ProbeResourceBinding
		if a.ResourceBinding != nil {
			have = *a.ResourceBinding
		}
		if have != wantBinding {
			return false
		}
	}
	return true
}

// --- equality / secrets helpers --------------------------------------------

func resourceTypeForKind(kind string) string {
	switch kind {
	case "tag":
		return domain.ConfigResourceTag
	case "proxy":
		return domain.ConfigResourceProxy
	case "notification":
		return domain.ConfigResourceNotification
	case "monitor_group":
		return domain.ConfigResourceMonitorGroup
	case "monitor":
		return domain.ConfigResourceMonitor
	case "status_page":
		return domain.ConfigResourceStatusPage
	case "maintenance_window":
		return domain.ConfigResourceMaintenanceWindow
	default:
		return kind
	}
}

func redactConfigMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isSecretKey(k) {
			if s, ok := v.(string); ok && s != "" {
				out[k] = ConfigSecretRedacted
				continue
			}
		}
		out[k] = v
	}
	return out
}

func isSecretKey(k string) bool {
	_, ok := secretConfigKeys[strings.ToLower(strings.ReplaceAll(k, "-", "_"))]
	return ok
}

func stripRedacted(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok && s == ConfigSecretRedacted {
			continue
		}
		out[k] = v
	}
	return out
}

func mergeConfigMaps(existing, incoming map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range existing {
		out[k] = v
	}
	if incoming == nil {
		return out
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok && (s == ConfigSecretRedacted || s == "") && isSecretKey(k) {
			// preserve existing
			continue
		}
		out[k] = v
	}
	return out
}

func notificationEqual(cur *domain.Notification, want ConfigNotification) bool {
	active := true
	if want.Active != nil {
		active = *want.Active
	}
	if cur.Name != want.Name || cur.Type != want.Type || cur.Active != active || cur.IsDefault != want.IsDefault ||
		cur.IncludeAckURL != configIncludeAckURL(want) {
		return false
	}
	// Compare config ignoring redacted secrets.
	return configMapsEqualIgnoringSecrets(cur.Config, want.Config)
}

func configIncludeAckURL(n ConfigNotification) bool {
	if n.IncludeAckURL == nil {
		return domain.DefaultIncludeAckURL
	}
	return *n.IncludeAckURL
}

func configMonitorNotifIncludeTarget(l ConfigMonitorNotification) bool {
	if l.IncludeTarget == nil {
		return domain.DefaultIncludeTarget
	}
	return *l.IncludeTarget
}

func proxyEqual(cur *domain.Proxy, want ConfigProxy) bool {
	active := true
	if want.Active != nil {
		active = *want.Active
	}
	if cur.Protocol != want.Protocol || cur.Host != want.Host || cur.Port != want.Port ||
		cur.Auth != want.Auth || cur.Username != want.Username || cur.Active != active ||
		cur.IsDefault != want.IsDefault {
		return false
	}
	if want.Password != "" && want.Password != ConfigSecretRedacted && want.Password != cur.Password {
		return false
	}
	return true
}

func groupEqual(cur *domain.MonitorGroup, want ConfigMonitorGroup, s *ConfigService, ctx context.Context) bool {
	cond := want.Condition
	if cond == "" {
		cond = string(domain.GroupConditionWorstOfChildren)
	}
	if cur.Name != want.Name || cur.Description != want.Description || cur.Owner != want.Owner ||
		string(cur.Condition) != cond || cur.Threshold != want.Threshold ||
		cur.ThresholdIsPercent != want.ThresholdIsPercent || cur.Weight != want.Weight ||
		cur.Collapsed != want.Collapsed {
		return false
	}
	var wantParent *int64
	if want.Parent != "" {
		if pk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitorGroup, want.Parent); err == nil {
			wantParent = &pk.ResourceID
		}
	}
	return ptrInt64Equal(cur.ParentID, wantParent)
}

func monitorEqual(cur *domain.Monitor, want ConfigMonitor, s *ConfigService, ctx context.Context) bool {
	active := true
	if want.Active != nil {
		active = *want.Active
	}
	interval := want.Interval
	if interval <= 0 {
		interval = 60
	}
	timeout := want.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	if cur.Name != want.Name || cur.Description != want.Description || cur.Owner != want.Owner ||
		cur.InheritGroupOwner != want.InheritGroupOwner || cur.Type != want.Type ||
		cur.Active != active || cur.Interval != interval || cur.RetryInterval != want.RetryInterval ||
		cur.MaxRetries != want.MaxRetries || cur.Timeout != timeout ||
		cur.UpsideDown != want.UpsideDown || cur.ResendInterval != want.ResendInterval ||
		cur.Weight != want.Weight || cur.TLSIgnore != want.TLSIgnore ||
		cur.CertExpiryNotify != want.CertExpiryNotify {
		return false
	}
	if !stringSliceEqual(cur.AcceptedStatusCodes, want.AcceptedStatusCodes) {
		return false
	}
	if !configMapsEqualIgnoringSecrets(cur.Config, want.Config) {
		return false
	}
	var wantProxy, wantGroup *int64
	if want.Proxy != "" {
		if pk, err := s.keys.GetByKey(ctx, domain.ConfigResourceProxy, want.Proxy); err == nil {
			wantProxy = &pk.ResourceID
		}
	}
	if want.Group != "" {
		if gk, err := s.keys.GetByKey(ctx, domain.ConfigResourceMonitorGroup, want.Group); err == nil {
			wantGroup = &gk.ResourceID
		}
	}
	return ptrInt64Equal(cur.ProxyID, wantProxy) && ptrInt64Equal(cur.GroupID, wantGroup)
}

func statusPageEqual(cur *domain.StatusPage, want ConfigStatusPage) bool {
	published := true
	if want.Published != nil {
		published = *want.Published
	}
	if cur.Slug != want.Slug || cur.Title != want.Title || cur.Description != want.Description ||
		cur.Icon != want.Icon || cur.Theme != want.Theme || cur.Published != published ||
		cur.CustomDomain != want.CustomDomain || cur.FooterText != want.FooterText ||
		cur.CustomCSS != want.CustomCSS || cur.DashboardStyle != want.DashboardStyle ||
		cur.ShowTags != want.ShowTags || cur.AutoResolveIncidents != want.AutoResolveIncidents {
		return false
	}
	if want.AccessCode != "" && want.AccessCode != ConfigSecretRedacted {
		return false // explicit new code ⇒ update
	}
	if (cur.SLATarget == nil) != (want.SLATarget == nil) {
		return false
	}
	if cur.SLATarget != nil && want.SLATarget != nil && *cur.SLATarget != *want.SLATarget {
		return false
	}
	return true
}

func maintenanceEqual(cur *domain.MaintenanceWindow, want ConfigMaintenance) bool {
	active := true
	if want.Active != nil {
		active = *want.Active
	}
	tz := want.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if cur.Title != want.Title || cur.Description != want.Description || cur.Active != active ||
		cur.Strategy != want.Strategy || cur.CronExpr != want.CronExpr || cur.Duration != want.Duration ||
		cur.Timezone != tz {
		return false
	}
	if want.StartDate != "" {
		t, err := time.Parse(time.RFC3339, want.StartDate)
		if err != nil || !cur.StartDate.UTC().Equal(t.UTC()) {
			return false
		}
	}
	if want.EndDate != "" {
		t, err := time.Parse(time.RFC3339, want.EndDate)
		if err != nil || !cur.EndDate.UTC().Equal(t.UTC()) {
			return false
		}
	}
	return true
}

func configMapsEqualIgnoringSecrets(a, b map[string]any) bool {
	// Build comparable copies with secrets normalized.
	na := normalizeConfigForCompare(a)
	nb := normalizeConfigForCompare(b)
	aj, _ := json.Marshal(na)
	bj, _ := json.Marshal(nb)
	return string(aj) == string(bj)
}

func normalizeConfigForCompare(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		if isSecretKey(k) {
			if s, ok := v.(string); ok && (s == "" || s == ConfigSecretRedacted) {
				continue
			}
			// Non-redacted secret value ⇒ treat as different from redacted/absent
			// by encoding presence only when it's a real secret change intent.
			if s, ok := v.(string); ok && s != ConfigSecretRedacted {
				out[k] = s
			}
			continue
		}
		out[k] = v
	}
	return out
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ptrInt64Equal(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func orderConfigGroups(groups []ConfigMonitorGroup) []ConfigMonitorGroup {
	byKey := map[string]ConfigMonitorGroup{}
	for _, g := range groups {
		byKey[g.Key] = g
	}
	var out []ConfigMonitorGroup
	seen := map[string]bool{}
	var visit func(string)
	visit = func(key string) {
		if seen[key] {
			return
		}
		g, ok := byKey[key]
		if !ok {
			return
		}
		if g.Parent != "" {
			visit(g.Parent)
		}
		seen[key] = true
		out = append(out, g)
	}
	for _, g := range groups {
		visit(g.Key)
	}
	return out
}
