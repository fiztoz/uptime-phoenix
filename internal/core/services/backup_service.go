// Package services contains the use-case implementations.
// Services depend ONLY on ports and domain — never on adapters.
package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// BackupDocumentVersion is the schema version of the export JSON.
// Bump when the wire shape changes incompatibly so importers can migrate.
const BackupDocumentVersion = 1

// BackupDocument is the versioned JSON shape written by Export and accepted by Import.
//
// SECRETS POLICY (deliberate exception to "never return secrets"):
// A restorable backup must include notification provider configs (bot tokens,
// webhook URLs) and proxy passwords. Uptime Kuma does the same. Export is
// auth-gated, responses are Cache-Control: no-store, and the UI warns that the
// file contains secrets. Status-page PasswordHash (bcrypt) is also included so
// a protected page stays protected after restore.
type BackupDocument struct {
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exported_at"`

	Proxies               []BackupProxy                `json:"proxies"`
	NotificationTemplates []BackupNotificationTemplate `json:"notification_templates,omitempty"`
	Notifications         []BackupNotification         `json:"notifications"`
	Tags                  []BackupTag                  `json:"tags"`
	MonitorGroups         []BackupMonitorGroup         `json:"monitor_groups"`
	Monitors              []BackupMonitor              `json:"monitors"`
	MonitorTags           []BackupMonitorTag           `json:"monitor_tags"`
	MonitorNotifications  []BackupMonitorNotification  `json:"monitor_notifications"`
	// GroupNotifications is absent from documents exported before folder alerting
	// existed. It unmarshals to nil there, and the import loop over it is then a
	// no-op — that is the whole backward-compatibility story.
	GroupNotifications  []BackupGroupNotification  `json:"group_notifications"`
	StatusPages         []BackupStatusPage         `json:"status_pages"`
	StatusPageMonitors  []BackupStatusPageMonitor  `json:"status_page_monitors"`
	StatusPageCNAMEs    []BackupStatusPageCNAME    `json:"status_page_cnames"`
	Incidents           []BackupIncident           `json:"incidents"`
	MaintenanceWindows  []BackupMaintenance        `json:"maintenance_windows"`
	MaintenanceMonitors []BackupMaintenanceMonitor `json:"maintenance_monitors"`
	// StatusPageSubscriptionChannels is the per-page SMTP channel binding only.
	// Never export subscriber emails, tokens, or confirmation state (Sprint C F3.1).
	StatusPageSubscriptionChannels []BackupStatusPageSubscriptionChannel `json:"status_page_subscription_channels,omitempty"`
}

// BackupStatusPageSubscriptionChannel links a status page to one SMTP notification.
type BackupStatusPageSubscriptionChannel struct {
	StatusPageID   int64 `json:"status_page_id"`
	NotificationID int64 `json:"notification_id"`
}

// BackupProxy is the export shape of a proxy, including Password for restore.
type BackupProxy struct {
	ID        int64  `json:"id"`
	Protocol  string `json:"protocol"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Auth      bool   `json:"auth"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Active    bool   `json:"active"`
	IsDefault bool   `json:"is_default"`
}

// BackupNotification is the export shape of a notification (config includes secrets).
type BackupNotification struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Active    bool   `json:"active"`
	IsDefault bool   `json:"is_default"`
	// IncludeAckURL is a pointer so backups taken before this field existed
	// unmarshal as nil and restore to the create default (off).
	IncludeAckURL *bool          `json:"include_ack_url,omitempty"`
	TemplateID    *int64         `json:"template_id,omitempty"`
	Config        map[string]any `json:"config"`
}

// BackupNotificationTemplate is the export shape of a reusable provider layout.
type BackupNotificationTemplate struct {
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	Provider      string         `json:"provider"`
	TitleTemplate string         `json:"title_template"`
	BodyTemplate  string         `json:"body_template"`
	Config        map[string]any `json:"config,omitempty"`
}

// BackupTag is the export shape of a tag.
type BackupTag struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// BackupMonitor is the export shape of a monitor (IDs are source-side and remapped on import).
type BackupMonitor struct {
	ID                  int64          `json:"id"`
	Name                string         `json:"name"`
	Description         string         `json:"description"`
	Owner               string         `json:"owner"`
	InheritGroupOwner   bool           `json:"inherit_group_owner"`
	Type                string         `json:"type"`
	Active              bool           `json:"active"`
	Interval            int            `json:"interval"`
	RetryInterval       int            `json:"retry_interval"`
	MaxRetries          int            `json:"max_retries"`
	Timeout             float64        `json:"timeout"`
	Config              map[string]any `json:"config"`
	AcceptedStatusCodes []string       `json:"accepted_statuscodes"`
	ProxyID             *int64         `json:"proxy_id"`
	UpsideDown          bool           `json:"upside_down"`
	ResendInterval      int            `json:"resend_interval"`
	PushToken           string         `json:"push_token,omitempty"`
	GroupID             *int64         `json:"group_id"`
	Weight              int            `json:"weight"`
	TLSIgnore           bool           `json:"tls_ignore"`
	CertExpiryNotify    bool           `json:"cert_expiry_notify"`
}

// BackupMonitorGroup is the export shape of a monitor group / folder (IDs are
// source-side and remapped on import). ParentID, like Monitor.GroupID, is the
// source-side ID of another BackupMonitorGroup in the same document.
type BackupMonitorGroup struct {
	ID                 int64                 `json:"id"`
	Name               string                `json:"name"`
	Description        string                `json:"description"`
	Owner              string                `json:"owner"`
	ParentID           *int64                `json:"parent_id"`
	Condition          domain.GroupCondition `json:"condition"`
	Threshold          int                   `json:"threshold"`
	ThresholdIsPercent bool                  `json:"threshold_is_percent"`
	Weight             int                   `json:"weight"`
	Collapsed          bool                  `json:"collapsed"`
}

// BackupMonitorTag links a monitor to a tag in the export.
type BackupMonitorTag struct {
	MonitorID int64  `json:"monitor_id"`
	TagID     int64  `json:"tag_id"`
	Value     string `json:"value"`
}

// BackupMonitorNotification links a monitor to a notification in the export.
type BackupMonitorNotification struct {
	MonitorID      int64 `json:"monitor_id"`
	NotificationID int64 `json:"notification_id"`
}

// BackupGroupNotification links a monitor GROUP to a notification in the export.
//
// Distinct from BackupMonitorNotification and not derivable from it: a folder
// alerts on its own derived status, so dropping these links on restore would
// leave every folder silently un-alerted while every monitor kept working.
type BackupGroupNotification struct {
	GroupID        int64 `json:"group_id"`
	NotificationID int64 `json:"notification_id"`
}

// BackupStatusPage is the export shape of a status page.
type BackupStatusPage struct {
	ID                   int64  `json:"id"`
	Slug                 string `json:"slug"`
	Title                string `json:"title"`
	Description          string `json:"description"`
	Icon                 string `json:"icon"`
	Favicon              string `json:"favicon,omitempty"`
	Theme                string `json:"theme"`
	Published            bool   `json:"published"`
	CustomDomain         string `json:"custom_domain"`
	PasswordHash         string `json:"password_hash,omitempty"`
	FooterText           string `json:"footer_text"`
	CustomCSS            string `json:"custom_css"`
	DashboardStyle       string `json:"dashboard_style"`
	ShowTags             bool   `json:"show_tags"`
	AutoResolveIncidents bool   `json:"auto_resolve_incidents"`
	// ShowPoweredBy is a pointer so pre-F3.5 backups (field absent) default to
	// branded (true) while explicit false survives round-trip.
	ShowPoweredBy *bool    `json:"show_powered_by,omitempty"`
	SLATarget     *float64 `json:"sla_target,omitempty"`
}

// BackupStatusPageMonitor links a status page to a monitor.
type BackupStatusPageMonitor struct {
	StatusPageID int64 `json:"status_page_id"`
	MonitorID    int64 `json:"monitor_id"`
	DisplayOrder int   `json:"display_order"`
}

// BackupStatusPageCNAME is a custom domain alias on a status page.
type BackupStatusPageCNAME struct {
	StatusPageID int64  `json:"status_page_id"`
	Domain       string `json:"domain"`
}

// BackupIncident is an incident attached to a status page.
type BackupIncident struct {
	StatusPageID int64  `json:"status_page_id"`
	Title        string `json:"title"`
	Content      string `json:"content"`
	Style        string `json:"style"`
	Pinned       bool   `json:"pinned"`
	Active       bool   `json:"active"`
}

// BackupMaintenance is a maintenance window.
type BackupMaintenance struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Active      bool      `json:"active"`
	Strategy    string    `json:"strategy"`
	StartDate   time.Time `json:"start_date"`
	EndDate     time.Time `json:"end_date"`
	CronExpr    string    `json:"cron_expr"`
	Duration    int       `json:"duration"`
	Timezone    string    `json:"timezone"`
}

// BackupMaintenanceMonitor links a maintenance window to a monitor.
type BackupMaintenanceMonitor struct {
	MaintenanceID int64 `json:"maintenance_id"`
	MonitorID     int64 `json:"monitor_id"`
}

// ImportSkipped describes one item that could not be imported.
type ImportSkipped struct {
	Kind   string `json:"kind"`
	ID     int64  `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

// ImportSummary is returned by Import with creation counts and skips.
type ImportSummary struct {
	ProxiesCreated               int             `json:"proxies_created"`
	NotificationTemplatesCreated int             `json:"notification_templates_created"`
	NotificationsCreated         int             `json:"notifications_created"`
	TagsCreated                  int             `json:"tags_created"`
	TagsReused                   int             `json:"tags_reused"`
	MonitorGroupsCreated         int             `json:"monitor_groups_created"`
	MonitorsCreated              int             `json:"monitors_created"`
	MonitorTagsCreated           int             `json:"monitor_tags_created"`
	MonitorNotificationsCreated  int             `json:"monitor_notifications_created"`
	GroupNotificationsCreated    int             `json:"group_notifications_created"`
	StatusPagesCreated           int             `json:"status_pages_created"`
	StatusPageMonitorsCreated    int             `json:"status_page_monitors_created"`
	StatusPageCNAMEsCreated      int             `json:"status_page_cnames_created"`
	IncidentsCreated             int             `json:"incidents_created"`
	MaintenanceWindowsCreated    int             `json:"maintenance_windows_created"`
	MaintenanceMonitorsCreated   int             `json:"maintenance_monitors_created"`
	Skipped                      []ImportSkipped `json:"skipped"`
}

// BackupService exports and imports a user's configuration as a versioned JSON document.
// Import is merge-only (creates new entities; never overwrites or deletes existing data).
type BackupService struct {
	monitors              ports.MonitorRepository
	groups                ports.MonitorGroupRepository
	notifications         ports.NotificationRepository
	notificationTemplates ports.NotificationTemplateRepository
	monitorNotifs         ports.MonitorNotificationRepository
	// Optional (SetGroupNotificationRepo). When nil, folder→notification links are
	// simply absent from an export and skipped on import — the pre-folder-alerting
	// behavior — rather than silently dropped from a document that HAS them.
	groupNotifs   ports.GroupNotificationRepository
	tags          ports.TagRepository
	monitorTags   ports.MonitorTagRepository
	statusPages   ports.StatusPageRepository
	spMonitors    ports.StatusPageMonitorRepository
	spCNAMEs      ports.StatusPageCNAMERepository
	incidents     ports.IncidentRepository
	maintenance   ports.MaintenanceRepository
	maintMonitors ports.MaintenanceWindowMonitorRepository
	proxies       ports.ProxyRepository
	// Optional: channel Get/Set only — never used to export subscriber PII.
	spSubscribers ports.StatusPageSubscriberRepository
	// Optional: when set, monitor/group create goes through the service so
	// validation (group cycle/threshold rules, proxy/group ownership) and
	// EventBus events stay consistent with normal CRUD.
	monitorSvc *MonitorService
	groupSvc   *MonitorGroupService
	proxySvc   *ProxyService
}

// NewBackupService creates a BackupService. monitorSvc, groupSvc and proxySvc
// may be nil; when non-nil they are preferred over raw repo Create for
// monitors/groups/proxies.
func NewBackupService(
	monitors ports.MonitorRepository,
	groups ports.MonitorGroupRepository,
	notifications ports.NotificationRepository,
	monitorNotifs ports.MonitorNotificationRepository,
	tags ports.TagRepository,
	monitorTags ports.MonitorTagRepository,
	statusPages ports.StatusPageRepository,
	spMonitors ports.StatusPageMonitorRepository,
	spCNAMEs ports.StatusPageCNAMERepository,
	incidents ports.IncidentRepository,
	maintenance ports.MaintenanceRepository,
	maintMonitors ports.MaintenanceWindowMonitorRepository,
	proxies ports.ProxyRepository,
) *BackupService {
	return &BackupService{
		monitors:      monitors,
		groups:        groups,
		notifications: notifications,
		monitorNotifs: monitorNotifs,
		tags:          tags,
		monitorTags:   monitorTags,
		statusPages:   statusPages,
		spMonitors:    spMonitors,
		spCNAMEs:      spCNAMEs,
		incidents:     incidents,
		maintenance:   maintenance,
		maintMonitors: maintMonitors,
		proxies:       proxies,
	}
}

// SetGroupNotificationRepo attaches the folder→notification link store, so those
// links survive an export/import round trip. Without it a restore would bring back
// every folder and every notification, and quietly lose the wiring between them.
func (s *BackupService) SetGroupNotificationRepo(repo ports.GroupNotificationRepository) {
	s.groupNotifs = repo
}

// SetNotificationTemplateRepo attaches reusable message layouts so template
// selections survive backup export/import. It remains optional for older tests
// and minimal compositions that do not expose templates.
func (s *BackupService) SetNotificationTemplateRepo(repo ports.NotificationTemplateRepository) {
	s.notificationTemplates = repo
}

// SetMonitorService attaches MonitorService so imports publish monitor.update events
// and reuse group/proxy validation.
func (s *BackupService) SetMonitorService(ms *MonitorService) {
	s.monitorSvc = ms
}

// SetMonitorGroupService attaches MonitorGroupService so group imports reuse
// the service's validation (name/condition/threshold rules, cycle detection)
// instead of writing straight through the repository.
func (s *BackupService) SetMonitorGroupService(gs *MonitorGroupService) {
	s.groupSvc = gs
}

// SetProxyService attaches ProxyService so default-proxy invariants are enforced on import.
func (s *BackupService) SetProxyService(ps *ProxyService) {
	s.proxySvc = ps
}

// SetSubscriberRepo attaches the status-page subscription repository so SMTP
// channel bindings (notification_id only) round-trip through backup. Subscriber
// emails and tokens are never exported.
func (s *BackupService) SetSubscriberRepo(repo ports.StatusPageSubscriberRepository) {
	s.spSubscribers = repo
}

// Export builds a BackupDocument for everything owned by (or linked to) userID.
func (s *BackupService) Export(ctx context.Context, userID int64) (*BackupDocument, error) {
	doc := &BackupDocument{
		Version:                        BackupDocumentVersion,
		ExportedAt:                     time.Now().UTC(),
		Proxies:                        []BackupProxy{},
		NotificationTemplates:          []BackupNotificationTemplate{},
		Notifications:                  []BackupNotification{},
		Tags:                           []BackupTag{},
		MonitorGroups:                  []BackupMonitorGroup{},
		Monitors:                       []BackupMonitor{},
		MonitorTags:                    []BackupMonitorTag{},
		MonitorNotifications:           []BackupMonitorNotification{},
		GroupNotifications:             []BackupGroupNotification{},
		StatusPages:                    []BackupStatusPage{},
		StatusPageMonitors:             []BackupStatusPageMonitor{},
		StatusPageCNAMEs:               []BackupStatusPageCNAME{},
		Incidents:                      []BackupIncident{},
		MaintenanceWindows:             []BackupMaintenance{},
		MaintenanceMonitors:            []BackupMaintenanceMonitor{},
		StatusPageSubscriptionChannels: []BackupStatusPageSubscriptionChannel{},
	}

	// Proxies (user-owned).
	proxies, err := s.proxies.List(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("backup export: list proxies: %w", err)
	}
	for _, p := range proxies {
		doc.Proxies = append(doc.Proxies, BackupProxy{
			ID:        p.ID,
			Protocol:  p.Protocol,
			Host:      p.Host,
			Port:      p.Port,
			Auth:      p.Auth,
			Username:  p.Username,
			Password:  p.Password,
			Active:    p.Active,
			IsDefault: p.IsDefault,
		})
	}

	// Notifications (user-owned; config intentionally includes secrets).
	notifs, err := s.notifications.List(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("backup export: list notifications: %w", err)
	}
	for _, n := range notifs {
		includeAckURL := n.IncludeAckURL
		doc.Notifications = append(doc.Notifications, BackupNotification{
			ID:            n.ID,
			Name:          n.Name,
			Type:          n.Type,
			Active:        n.Active,
			IsDefault:     n.IsDefault,
			IncludeAckURL: &includeAckURL,
			TemplateID:    n.TemplateID,
			Config:        n.Config,
		})
	}
	if s.notificationTemplates != nil {
		referenced := make(map[int64]struct{})
		for _, notification := range notifs {
			if notification.TemplateID != nil {
				referenced[*notification.TemplateID] = struct{}{}
			}
		}
		templates, listErr := s.notificationTemplates.List(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("backup export: list notification templates: %w", listErr)
		}
		for _, template := range templates {
			_, used := referenced[template.ID]
			if template.UserID != userID && !used {
				continue
			}
			doc.NotificationTemplates = append(doc.NotificationTemplates, BackupNotificationTemplate{
				ID:            template.ID,
				Name:          template.Name,
				Provider:      template.Provider,
				TitleTemplate: template.TitleTemplate,
				BodyTemplate:  template.BodyTemplate,
				Config:        template.Config,
			})
		}
	}

	// Monitor groups / folders (user-owned).
	groups, err := s.groups.List(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("backup export: list monitor groups: %w", err)
	}
	for _, g := range groups {
		doc.MonitorGroups = append(doc.MonitorGroups, BackupMonitorGroup{
			ID:                 g.ID,
			Name:               g.Name,
			Description:        g.Description,
			Owner:              g.Owner,
			ParentID:           g.ParentID,
			Condition:          g.Condition,
			Threshold:          g.Threshold,
			ThresholdIsPercent: g.ThresholdIsPercent,
			Weight:             g.Weight,
			Collapsed:          g.Collapsed,
		})
	}

	// Monitors (user-owned).
	monitors, err := s.monitors.List(ctx, ports.MonitorFilter{UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("backup export: list monitors: %w", err)
	}
	userMonitorIDs := make(map[int64]struct{}, len(monitors))
	for _, m := range monitors {
		userMonitorIDs[m.ID] = struct{}{}
		doc.Monitors = append(doc.Monitors, BackupMonitor{
			ID:                  m.ID,
			Name:                m.Name,
			Description:         m.Description,
			Owner:               m.Owner,
			InheritGroupOwner:   m.InheritGroupOwner,
			Type:                m.Type,
			Active:              m.Active,
			Interval:            m.Interval,
			RetryInterval:       m.RetryInterval,
			MaxRetries:          m.MaxRetries,
			Timeout:             m.Timeout,
			Config:              m.Config,
			AcceptedStatusCodes: m.AcceptedStatusCodes,
			ProxyID:             m.ProxyID,
			UpsideDown:          m.UpsideDown,
			ResendInterval:      m.ResendInterval,
			PushToken:           m.PushToken,
			GroupID:             m.GroupID,
			Weight:              m.Weight,
			TLSIgnore:           m.TLSIgnore,
			CertExpiryNotify:    m.CertExpiryNotify,
		})
	}

	// Tags + monitor-tag links for the user's monitors only.
	usedTagIDs := make(map[int64]struct{})
	for mid := range userMonitorIDs {
		links, err := s.monitorTags.ListByMonitor(ctx, mid)
		if err != nil {
			return nil, fmt.Errorf("backup export: list monitor tags for %d: %w", mid, err)
		}
		for _, link := range links {
			usedTagIDs[link.TagID] = struct{}{}
			doc.MonitorTags = append(doc.MonitorTags, BackupMonitorTag{
				MonitorID: link.MonitorID,
				TagID:     link.TagID,
				Value:     link.Value,
			})
		}
	}
	if len(usedTagIDs) > 0 {
		allTags, err := s.tags.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("backup export: list tags: %w", err)
		}
		for _, t := range allTags {
			if _, ok := usedTagIDs[t.ID]; ok {
				doc.Tags = append(doc.Tags, BackupTag{ID: t.ID, Name: t.Name, Color: t.Color})
			}
		}
	}

	// Monitor ↔ notification links (only for user's monitors).
	for _, n := range notifs {
		links, err := s.monitorNotifs.ListByNotification(ctx, n.ID)
		if err != nil {
			return nil, fmt.Errorf("backup export: list monitor notifications for %d: %w", n.ID, err)
		}
		for _, link := range links {
			if _, ok := userMonitorIDs[link.MonitorID]; !ok {
				continue
			}
			doc.MonitorNotifications = append(doc.MonitorNotifications, BackupMonitorNotification{
				MonitorID:      link.MonitorID,
				NotificationID: link.NotificationID,
			})
		}
	}

	// Group ↔ notification links (only for the user's own folders). Skipped
	// entirely when the repo is not wired, rather than exporting an empty list that
	// a later import would read as "this install had no folder alerting".
	if s.groupNotifs != nil {
		userGroupIDs := make(map[int64]struct{}, len(groups))
		for _, g := range groups {
			userGroupIDs[g.ID] = struct{}{}
		}
		for _, n := range notifs {
			links, err := s.groupNotifs.ListByNotification(ctx, n.ID)
			if err != nil {
				return nil, fmt.Errorf("backup export: list group notifications for %d: %w", n.ID, err)
			}
			for _, link := range links {
				if _, ok := userGroupIDs[link.GroupID]; !ok {
					continue
				}
				doc.GroupNotifications = append(doc.GroupNotifications, BackupGroupNotification{
					GroupID:        link.GroupID,
					NotificationID: link.NotificationID,
				})
			}
		}
	}

	// Status pages that reference at least one of the user's monitors.
	// Status pages have no user_id in the schema — scoping by linked monitors
	// is the ownership model for multi-user installs.
	allSPs, err := s.statusPages.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup export: list status pages: %w", err)
	}
	for _, sp := range allSPs {
		spMons, err := s.spMonitors.ListByStatusPage(ctx, sp.ID)
		if err != nil {
			return nil, fmt.Errorf("backup export: list status page monitors for %d: %w", sp.ID, err)
		}
		linked := false
		for _, sm := range spMons {
			if _, ok := userMonitorIDs[sm.MonitorID]; ok {
				linked = true
				doc.StatusPageMonitors = append(doc.StatusPageMonitors, BackupStatusPageMonitor{
					StatusPageID: sm.StatusPageID,
					MonitorID:    sm.MonitorID,
					DisplayOrder: sm.DisplayOrder,
				})
			}
		}
		if !linked {
			// Only pages tied to this user's monitors (status pages have no user_id).
			continue
		}
		// Channel binding only — never subscriber emails/tokens.
		if s.spSubscribers != nil {
			if ch, chErr := s.spSubscribers.GetChannel(ctx, sp.ID); chErr == nil && ch != nil {
				doc.StatusPageSubscriptionChannels = append(doc.StatusPageSubscriptionChannels, BackupStatusPageSubscriptionChannel{
					StatusPageID:   sp.ID,
					NotificationID: ch.NotificationID,
				})
			}
		}
		poweredBy := sp.ShowPoweredBy
		doc.StatusPages = append(doc.StatusPages, BackupStatusPage{
			ID:                   sp.ID,
			Slug:                 sp.Slug,
			Title:                sp.Title,
			Description:          sp.Description,
			Icon:                 sp.Icon,
			Favicon:              sp.Favicon,
			Theme:                sp.Theme,
			Published:            sp.Published,
			CustomDomain:         sp.CustomDomain,
			PasswordHash:         sp.PasswordHash,
			FooterText:           sp.FooterText,
			CustomCSS:            sp.CustomCSS,
			DashboardStyle:       sp.DashboardStyle,
			ShowTags:             sp.ShowTags,
			AutoResolveIncidents: sp.AutoResolveIncidents,
			ShowPoweredBy:        &poweredBy,
			SLATarget:            sp.SLATarget,
		})

		cnames, err := s.spCNAMEs.ListByStatusPage(ctx, sp.ID)
		if err != nil {
			return nil, fmt.Errorf("backup export: list cnames for %d: %w", sp.ID, err)
		}
		for _, c := range cnames {
			doc.StatusPageCNAMEs = append(doc.StatusPageCNAMEs, BackupStatusPageCNAME{
				StatusPageID: c.StatusPageID,
				Domain:       c.Domain,
			})
		}

		incs, err := s.incidents.ListByStatusPage(ctx, sp.ID)
		if err != nil {
			return nil, fmt.Errorf("backup export: list incidents for %d: %w", sp.ID, err)
		}
		for _, inc := range incs {
			doc.Incidents = append(doc.Incidents, BackupIncident{
				StatusPageID: inc.StatusPageID,
				Title:        inc.Title,
				Content:      inc.Content,
				Style:        inc.Style,
				Pinned:       inc.Pinned,
				Active:       inc.Active,
			})
		}
	}

	// Maintenance windows (user-owned) + monitor assignments.
	windows, err := s.maintenance.List(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("backup export: list maintenance: %w", err)
	}
	for _, mw := range windows {
		tz := mw.Timezone
		if tz == "" {
			tz = "UTC"
		}
		doc.MaintenanceWindows = append(doc.MaintenanceWindows, BackupMaintenance{
			ID:          mw.ID,
			Title:       mw.Title,
			Description: mw.Description,
			Active:      mw.Active,
			Strategy:    mw.Strategy,
			StartDate:   mw.StartDate,
			EndDate:     mw.EndDate,
			CronExpr:    mw.CronExpr,
			Duration:    mw.Duration,
			Timezone:    tz,
		})
		mids, err := s.maintMonitors.ListByMaintenance(ctx, mw.ID)
		if err != nil {
			return nil, fmt.Errorf("backup export: list maintenance monitors for %d: %w", mw.ID, err)
		}
		for _, mid := range mids {
			if _, ok := userMonitorIDs[mid]; !ok {
				continue
			}
			doc.MaintenanceMonitors = append(doc.MaintenanceMonitors, BackupMaintenanceMonitor{
				MaintenanceID: mw.ID,
				MonitorID:     mid,
			})
		}
	}

	return doc, nil
}

// Import creates new entities for userID from a BackupDocument (merge-only).
// Source IDs are remapped; parents are imported before children.
func (s *BackupService) Import(ctx context.Context, userID int64, doc *BackupDocument) (*ImportSummary, error) {
	if doc == nil {
		return nil, fmt.Errorf("backup import: %w: document is required", domain.ErrValidation)
	}
	if doc.Version == 0 {
		// Treat missing version as v1 for slightly older hand-written docs.
		doc.Version = BackupDocumentVersion
	}
	if doc.Version != BackupDocumentVersion {
		return nil, fmt.Errorf("backup import: %w: unsupported backup version %d (want %d)",
			domain.ErrValidation, doc.Version, BackupDocumentVersion)
	}

	summary := &ImportSummary{Skipped: []ImportSkipped{}}
	proxyMap := map[int64]int64{}
	templateMap := map[int64]int64{}
	notifMap := map[int64]int64{}
	tagMap := map[int64]int64{}
	groupMap := map[int64]int64{}
	monitorMap := map[int64]int64{}
	spMap := map[int64]int64{}
	maintMap := map[int64]int64{}

	// 1. Proxies
	for _, bp := range doc.Proxies {
		p := &domain.Proxy{
			UserID:    userID,
			Protocol:  bp.Protocol,
			Host:      bp.Host,
			Port:      bp.Port,
			Auth:      bp.Auth,
			Username:  bp.Username,
			Password:  bp.Password,
			Active:    bp.Active,
			IsDefault: bp.IsDefault,
		}
		var err error
		if s.proxySvc != nil {
			err = s.proxySvc.Create(ctx, p)
		} else {
			err = s.proxies.Create(ctx, p)
		}
		if err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "proxy", ID: bp.ID, Name: bp.Host, Reason: err.Error(),
			})
			continue
		}
		proxyMap[bp.ID] = p.ID
		summary.ProxiesCreated++
	}

	// 2. Notification templates. Older v1 documents omit this additive field.
	if s.notificationTemplates != nil {
		for _, backupTemplate := range doc.NotificationTemplates {
			template := &domain.NotificationTemplate{
				UserID:        userID,
				Name:          backupTemplate.Name,
				Provider:      backupTemplate.Provider,
				TitleTemplate: backupTemplate.TitleTemplate,
				BodyTemplate:  backupTemplate.BodyTemplate,
				Config:        backupTemplate.Config,
			}
			if err := validateNotificationTemplate(template); err != nil {
				summary.Skipped = append(summary.Skipped, ImportSkipped{
					Kind: "notification_template", ID: backupTemplate.ID, Name: backupTemplate.Name, Reason: err.Error(),
				})
				continue
			}
			if err := s.notificationTemplates.Create(ctx, template); err != nil {
				summary.Skipped = append(summary.Skipped, ImportSkipped{
					Kind: "notification_template", ID: backupTemplate.ID, Name: backupTemplate.Name, Reason: err.Error(),
				})
				continue
			}
			templateMap[backupTemplate.ID] = template.ID
			summary.NotificationTemplatesCreated++
		}
	}

	// 3. Notifications
	for _, bn := range doc.Notifications {
		var templateID *int64
		if bn.TemplateID != nil {
			if mapped, ok := templateMap[*bn.TemplateID]; ok {
				templateID = &mapped
			}
		}
		includeAckURL := domain.DefaultIncludeAckURL
		if bn.IncludeAckURL != nil {
			includeAckURL = *bn.IncludeAckURL
		}
		n := &domain.Notification{
			UserID:        userID,
			Name:          bn.Name,
			Type:          bn.Type,
			Active:        bn.Active,
			IsDefault:     bn.IsDefault,
			IncludeAckURL: includeAckURL,
			TemplateID:    templateID,
			Config:        bn.Config,
		}
		if n.Config == nil {
			n.Config = map[string]any{}
		}
		if err := s.notifications.Create(ctx, n); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "notification", ID: bn.ID, Name: bn.Name, Reason: err.Error(),
			})
			continue
		}
		notifMap[bn.ID] = n.ID
		summary.NotificationsCreated++
	}

	// 4. Tags — reuse existing by unique name, otherwise create.
	existingTags, err := s.tags.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup import: list tags: %w", err)
	}
	tagByName := make(map[string]*domain.Tag, len(existingTags))
	for _, t := range existingTags {
		tagByName[strings.ToLower(t.Name)] = t
	}
	for _, bt := range doc.Tags {
		if existing, ok := tagByName[strings.ToLower(bt.Name)]; ok {
			tagMap[bt.ID] = existing.ID
			summary.TagsReused++
			continue
		}
		t := &domain.Tag{Name: bt.Name, Color: bt.Color}
		if t.Color == "" {
			t.Color = "#666666"
		}
		if err := s.tags.Create(ctx, t); err != nil {
			// Race or unique constraint — try reuse by re-listing.
			if reused := findTagByName(ctx, s.tags, bt.Name); reused != nil {
				tagMap[bt.ID] = reused.ID
				summary.TagsReused++
				continue
			}
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "tag", ID: bt.ID, Name: bt.Name, Reason: err.Error(),
			})
			continue
		}
		tagMap[bt.ID] = t.ID
		tagByName[strings.ToLower(t.Name)] = t
		summary.TagsCreated++
	}

	// 4. Monitor groups — parents before children, at arbitrary nesting depth.
	for _, bg := range orderGroupsByDepth(doc.MonitorGroups) {
		var parentID *int64
		if bg.ParentID != nil {
			if newID, ok := groupMap[*bg.ParentID]; ok {
				parentID = &newID
			} else {
				summary.Skipped = append(summary.Skipped, ImportSkipped{
					Kind: "monitor_group", ID: bg.ID, Name: bg.Name,
					Reason: "parent group was not imported",
				})
				continue
			}
		}
		g := &domain.MonitorGroup{
			UserID:             userID,
			Name:               bg.Name,
			Description:        bg.Description,
			Owner:              bg.Owner,
			ParentID:           parentID,
			Condition:          bg.Condition,
			Threshold:          bg.Threshold,
			ThresholdIsPercent: bg.ThresholdIsPercent,
			Weight:             bg.Weight,
			Collapsed:          bg.Collapsed,
		}
		var groupErr error
		if s.groupSvc != nil {
			groupErr = s.groupSvc.Create(ctx, g)
		} else {
			groupErr = s.groups.Create(ctx, g)
		}
		if groupErr != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "monitor_group", ID: bg.ID, Name: bg.Name, Reason: groupErr.Error(),
			})
			continue
		}
		groupMap[bg.ID] = g.ID
		summary.MonitorGroupsCreated++
	}

	// 5. Monitors — groups already exist from step 4, so a stable ID order is
	// enough; monitors no longer nest under one another (only under groups).
	sortedMonitors := make([]BackupMonitor, len(doc.Monitors))
	copy(sortedMonitors, doc.Monitors)
	sort.Slice(sortedMonitors, func(i, j int) bool { return sortedMonitors[i].ID < sortedMonitors[j].ID })
	for _, bm := range sortedMonitors {
		var groupID *int64
		if bm.GroupID != nil {
			if newID, ok := groupMap[*bm.GroupID]; ok {
				groupID = &newID
			} else {
				summary.Skipped = append(summary.Skipped, ImportSkipped{
					Kind: "monitor", ID: bm.ID, Name: bm.Name,
					Reason: "monitor group was not imported",
				})
				continue
			}
		}
		var proxyID *int64
		if bm.ProxyID != nil {
			if newID, ok := proxyMap[*bm.ProxyID]; ok {
				proxyID = &newID
			}
			// Missing proxy: create monitor without proxy rather than skip entirely.
		}

		m := &domain.Monitor{
			UserID:              userID,
			Name:                bm.Name,
			Description:         bm.Description,
			Owner:               bm.Owner,
			InheritGroupOwner:   bm.InheritGroupOwner,
			Type:                bm.Type,
			Active:              bm.Active,
			Interval:            bm.Interval,
			RetryInterval:       bm.RetryInterval,
			MaxRetries:          bm.MaxRetries,
			Timeout:             bm.Timeout,
			Config:              bm.Config,
			AcceptedStatusCodes: bm.AcceptedStatusCodes,
			ProxyID:             proxyID,
			UpsideDown:          bm.UpsideDown,
			ResendInterval:      bm.ResendInterval,
			PushToken:           bm.PushToken,
			GroupID:             groupID,
			Weight:              bm.Weight,
			TLSIgnore:           bm.TLSIgnore,
			CertExpiryNotify:    bm.CertExpiryNotify,
		}
		if m.Config == nil {
			m.Config = map[string]any{}
		}
		if m.Interval <= 0 {
			m.Interval = 60
		}
		if m.Timeout <= 0 {
			m.Timeout = 30
		}

		var createErr error
		if s.monitorSvc != nil {
			createErr = s.monitorSvc.Create(ctx, m)
		} else {
			createErr = s.monitors.Create(ctx, m)
		}
		if createErr != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "monitor", ID: bm.ID, Name: bm.Name, Reason: createErr.Error(),
			})
			continue
		}
		monitorMap[bm.ID] = m.ID
		summary.MonitorsCreated++
	}

	// 6. Monitor-tag assignments
	for _, mt := range doc.MonitorTags {
		newMon, okM := monitorMap[mt.MonitorID]
		newTag, okT := tagMap[mt.TagID]
		if !okM || !okT {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "monitor_tag", Reason: "monitor or tag was not imported",
			})
			continue
		}
		if err := s.monitorTags.Assign(ctx, newMon, newTag, mt.Value); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "monitor_tag", Reason: err.Error(),
			})
			continue
		}
		summary.MonitorTagsCreated++
	}

	// 7. Monitor-notification assignments
	for _, mn := range doc.MonitorNotifications {
		newMon, okM := monitorMap[mn.MonitorID]
		newNotif, okN := notifMap[mn.NotificationID]
		if !okM || !okN {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "monitor_notification", Reason: "monitor or notification was not imported",
			})
			continue
		}
		if err := s.monitorNotifs.Attach(ctx, newMon, newNotif); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "monitor_notification", Reason: err.Error(),
			})
			continue
		}
		summary.MonitorNotificationsCreated++
	}

	// 7b. Group-notification assignments — the folder's own alerting.
	//
	// Absent from documents exported before folder alerting existed: doc.GroupNotifications
	// is nil there and this loop does nothing. A document that HAS them but lands on an
	// install with no group-notification repo wired reports every link as skipped rather
	// than pretending it restored them.
	for _, gn := range doc.GroupNotifications {
		if s.groupNotifs == nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "group_notification", Reason: "group notifications are not enabled on this install",
			})
			continue
		}
		newGroup, okG := groupMap[gn.GroupID]
		newNotif, okN := notifMap[gn.NotificationID]
		if !okG || !okN {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "group_notification", Reason: "group or notification was not imported",
			})
			continue
		}
		if err := s.groupNotifs.Attach(ctx, newGroup, newNotif); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "group_notification", Reason: err.Error(),
			})
			continue
		}
		summary.GroupNotificationsCreated++
	}

	// 8. Status pages (slug uniqueness — suffix on conflict)
	for _, bsp := range doc.StatusPages {
		slug, err := s.uniqueStatusPageSlug(ctx, bsp.Slug)
		if err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page", ID: bsp.ID, Name: bsp.Slug, Reason: err.Error(),
			})
			continue
		}
		showPoweredBy := true
		if bsp.ShowPoweredBy != nil {
			showPoweredBy = *bsp.ShowPoweredBy
		}
		sp := &domain.StatusPage{
			Slug:                 slug,
			Title:                bsp.Title,
			Description:          bsp.Description,
			Icon:                 bsp.Icon,
			Favicon:              bsp.Favicon,
			Theme:                bsp.Theme,
			Published:            bsp.Published,
			CustomDomain:         "", // avoid unique/custom-domain collisions; CNAMEs handle aliases
			PasswordHash:         bsp.PasswordHash,
			FooterText:           bsp.FooterText,
			CustomCSS:            bsp.CustomCSS,
			DashboardStyle:       bsp.DashboardStyle,
			ShowTags:             bsp.ShowTags,
			AutoResolveIncidents: bsp.AutoResolveIncidents,
			ShowPoweredBy:        showPoweredBy,
			SLATarget:            bsp.SLATarget,
		}
		if sp.Theme == "" {
			sp.Theme = "light"
		}
		sp.DashboardStyle = domain.NormalizeDashboardStyle(sp.DashboardStyle)
		if err := normalizeStatusPageSLATarget(sp); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page", ID: bsp.ID, Name: bsp.Slug, Reason: err.Error(),
			})
			continue
		}
		if err := s.statusPages.Create(ctx, sp); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page", ID: bsp.ID, Name: bsp.Slug, Reason: err.Error(),
			})
			continue
		}
		spMap[bsp.ID] = sp.ID
		summary.StatusPagesCreated++
	}

	// 8b. Status-page SMTP subscription channels (notification ID remapped; no subscriber PII).
	for _, ch := range doc.StatusPageSubscriptionChannels {
		if s.spSubscribers == nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page_subscription_channel", Reason: "subscription channels are not enabled on this install",
			})
			continue
		}
		newSP, okSP := spMap[ch.StatusPageID]
		newNotif, okN := notifMap[ch.NotificationID]
		if !okSP || !okN {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page_subscription_channel", Reason: "status page or notification was not imported",
			})
			continue
		}
		if err := s.spSubscribers.SetChannel(ctx, &domain.StatusPageSubscriptionChannel{
			StatusPageID:   newSP,
			NotificationID: newNotif,
		}); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page_subscription_channel", Reason: err.Error(),
			})
			continue
		}
	}

	// 9. Status page monitors
	for _, sm := range doc.StatusPageMonitors {
		newSP, okSP := spMap[sm.StatusPageID]
		newMon, okM := monitorMap[sm.MonitorID]
		if !okSP || !okM {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page_monitor", Reason: "status page or monitor was not imported",
			})
			continue
		}
		order := sm.DisplayOrder
		if order <= 0 {
			order = 1000
		}
		if err := s.spMonitors.AddMonitor(ctx, newSP, newMon, order); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page_monitor", Reason: err.Error(),
			})
			continue
		}
		summary.StatusPageMonitorsCreated++
	}

	// 10. Status page CNAMEs
	for _, bc := range doc.StatusPageCNAMEs {
		newSP, ok := spMap[bc.StatusPageID]
		if !ok {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page_cname", Name: bc.Domain, Reason: "status page was not imported",
			})
			continue
		}
		cname := &domain.StatusPageCNAME{StatusPageID: newSP, Domain: bc.Domain}
		if err := s.spCNAMEs.Create(ctx, cname); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "status_page_cname", Name: bc.Domain, Reason: err.Error(),
			})
			continue
		}
		summary.StatusPageCNAMEsCreated++
	}

	// 11. Incidents
	for _, bi := range doc.Incidents {
		newSP, ok := spMap[bi.StatusPageID]
		if !ok {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "incident", Name: bi.Title, Reason: "status page was not imported",
			})
			continue
		}
		inc := &domain.Incident{
			StatusPageID: newSP,
			Title:        bi.Title,
			Content:      bi.Content,
			Style:        bi.Style,
			Pinned:       bi.Pinned,
			Active:       bi.Active,
		}
		if inc.Style == "" {
			inc.Style = "warning"
		}
		if err := s.incidents.Create(ctx, inc); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "incident", Name: bi.Title, Reason: err.Error(),
			})
			continue
		}
		summary.IncidentsCreated++
	}

	// 12. Maintenance windows
	for _, bmw := range doc.MaintenanceWindows {
		mw := &domain.MaintenanceWindow{
			UserID:      userID,
			Title:       bmw.Title,
			Description: bmw.Description,
			Active:      bmw.Active,
			Strategy:    bmw.Strategy,
			StartDate:   bmw.StartDate,
			EndDate:     bmw.EndDate,
			CronExpr:    bmw.CronExpr,
			Duration:    bmw.Duration,
			Timezone:    bmw.Timezone,
		}
		if mw.Strategy == "" {
			mw.Strategy = "single"
		}
		if mw.Timezone == "" {
			mw.Timezone = "UTC"
		}
		if err := s.maintenance.Create(ctx, mw); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "maintenance", ID: bmw.ID, Name: bmw.Title, Reason: err.Error(),
			})
			continue
		}
		maintMap[bmw.ID] = mw.ID
		summary.MaintenanceWindowsCreated++
	}

	// 13. Maintenance ↔ monitor
	for _, mm := range doc.MaintenanceMonitors {
		newMaint, okMaint := maintMap[mm.MaintenanceID]
		newMon, okMon := monitorMap[mm.MonitorID]
		if !okMaint || !okMon {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "maintenance_monitor", Reason: "maintenance or monitor was not imported",
			})
			continue
		}
		if err := s.maintMonitors.Assign(ctx, newMaint, newMon); err != nil {
			summary.Skipped = append(summary.Skipped, ImportSkipped{
				Kind: "maintenance_monitor", Reason: err.Error(),
			})
			continue
		}
		summary.MaintenanceMonitorsCreated++
	}

	return summary, nil
}

// orderGroupsByDepth returns groups ordered so a parent always appears before
// its children, at arbitrary nesting depth — a topological sort over
// ParentID (Kahn's algorithm). Groups whose parent chain never resolves
// within the document (a dangling or cyclic ParentID, which the service's own
// cycle detection should prevent but an older/hand-edited export might still
// contain) are appended at the end in ID order; Import skips them because
// their parent was never created, rather than looping forever trying to
// place them.
func orderGroupsByDepth(groups []BackupMonitorGroup) []BackupMonitorGroup {
	sorted := make([]BackupMonitorGroup, len(groups))
	copy(sorted, groups)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var ordered []BackupMonitorGroup
	done := make(map[int64]bool, len(sorted))
	remaining := sorted

	// Bounded by len(sorted) passes: each pass either resolves at least one
	// more group or makes no progress, in which case whatever is left has an
	// unresolved parent chain and is appended as-is below.
	for pass := 0; pass < len(sorted) && len(remaining) > 0; pass++ {
		var next []BackupMonitorGroup
		progressed := false
		for _, g := range remaining {
			if g.ParentID == nil || done[*g.ParentID] {
				ordered = append(ordered, g)
				done[g.ID] = true
				progressed = true
				continue
			}
			next = append(next, g)
		}
		remaining = next
		if !progressed {
			break
		}
	}
	return append(ordered, remaining...)
}

func findTagByName(ctx context.Context, repo ports.TagRepository, name string) *domain.Tag {
	all, err := repo.List(ctx)
	if err != nil {
		return nil
	}
	for _, t := range all {
		if strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
}

// uniqueStatusPageSlug returns slug, or slug-imported / slug-imported-N if taken.
func (s *BackupService) uniqueStatusPageSlug(ctx context.Context, base string) (string, error) {
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		base = "imported"
	}
	candidates := []string{base, base + "-imported"}
	for i := 2; i <= 50; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-imported-%d", base, i))
	}
	for _, c := range candidates {
		_, err := s.statusPages.GetBySlug(ctx, c)
		if err != nil {
			// Not found → available.
			if isNotFound(err) {
				return c, nil
			}
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate unique slug for %q", base)
}
