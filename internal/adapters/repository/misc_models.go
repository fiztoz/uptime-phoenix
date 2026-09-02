package repository

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// NotificationModel maps the notifications table.
type NotificationModel struct {
	bun.BaseModel `bun:"table:notifications"`

	ID            int64     `bun:"id,pk,autoincrement"`
	UserID        int64     `bun:"user_id"`
	Name          string    `bun:"name,notnull"`
	Type          string    `bun:"type,notnull"`
	Active        bool      `bun:"active,notnull,default:true"`
	IsDefault     bool      `bun:"is_default,notnull,default:false"`
	IncludeAckURL bool      `bun:"include_ack_url,notnull"`
	TemplateID    *int64    `bun:"template_id"`
	Config        JSONField `bun:"config,notnull"`
	CreatedAt     time.Time `bun:"created_at,notnull"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"`
}

// ToDomain converts a NotificationModel to a domain.Notification.
func (m *NotificationModel) ToDomain() *domain.Notification {
	return &domain.Notification{
		ID:            m.ID,
		UserID:        m.UserID,
		Name:          m.Name,
		Type:          m.Type,
		Active:        m.Active,
		IsDefault:     m.IsDefault,
		IncludeAckURL: m.IncludeAckURL,
		TemplateID:    m.TemplateID,
		Config:        m.Config.ToMap(),
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
}

// NotificationModelFromDomain converts a domain.Notification to a NotificationModel.
func NotificationModelFromDomain(n *domain.Notification) *NotificationModel {
	return &NotificationModel{
		ID:            n.ID,
		UserID:        n.UserID,
		Name:          n.Name,
		Type:          n.Type,
		Active:        n.Active,
		IsDefault:     n.IsDefault,
		IncludeAckURL: n.IncludeAckURL,
		TemplateID:    n.TemplateID,
		Config:        JSONField(n.Config),
		CreatedAt:     n.CreatedAt,
		UpdatedAt:     n.UpdatedAt,
	}
}

// NotificationTemplateModel maps the notification_templates table.
type NotificationTemplateModel struct {
	bun.BaseModel `bun:"table:notification_templates"`

	ID            int64     `bun:"id,pk,autoincrement"`
	UserID        int64     `bun:"user_id"`
	Name          string    `bun:"name,notnull"`
	Provider      string    `bun:"provider,notnull"`
	TitleTemplate string    `bun:"title_template,notnull"`
	BodyTemplate  string    `bun:"body_template,notnull"`
	Config        JSONField `bun:"config,notnull"`
	CreatedAt     time.Time `bun:"created_at,notnull"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"`
}

// ToDomain converts a NotificationTemplateModel to a domain.NotificationTemplate.
func (m *NotificationTemplateModel) ToDomain() *domain.NotificationTemplate {
	return &domain.NotificationTemplate{
		ID:            m.ID,
		UserID:        m.UserID,
		Name:          m.Name,
		Provider:      m.Provider,
		TitleTemplate: m.TitleTemplate,
		BodyTemplate:  m.BodyTemplate,
		Config:        m.Config.ToMap(),
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
}

// NotificationTemplateModelFromDomain converts a domain.NotificationTemplate
// to its persistence model.
func NotificationTemplateModelFromDomain(template *domain.NotificationTemplate) *NotificationTemplateModel {
	return &NotificationTemplateModel{
		ID:            template.ID,
		UserID:        template.UserID,
		Name:          template.Name,
		Provider:      template.Provider,
		TitleTemplate: template.TitleTemplate,
		BodyTemplate:  template.BodyTemplate,
		Config:        JSONField(template.Config),
		CreatedAt:     template.CreatedAt,
		UpdatedAt:     template.UpdatedAt,
	}
}

// StatusPageModel maps the status_pages table.
type StatusPageModel struct {
	bun.BaseModel `bun:"table:status_pages"`

	ID                   int64     `bun:"id,pk,autoincrement"`
	Slug                 string    `bun:"slug,notnull"`
	Title                string    `bun:"title,notnull"`
	Description          string    `bun:"description"`
	Icon                 string    `bun:"icon"`
	Favicon              string    `bun:"favicon"`
	Theme                string    `bun:"theme,notnull,default:'light'"`
	Published            bool      `bun:"published,notnull,default:true"`
	CustomDomain         string    `bun:"custom_domain"`
	PageAccessCredential string    `bun:"password_hash"` // optional access credential for protected pages
	FooterText           string    `bun:"footer_text"`
	CustomCSS            string    `bun:"custom_css"`
	DashboardStyle       string    `bun:"dashboard_style,notnull,default:'full'"`
	ShowTags             bool      `bun:"show_tags,notnull,default:false"`
	AutoResolveIncidents bool      `bun:"auto_resolve_incidents,notnull,default:false"`
	ShowPoweredBy        bool      `bun:"show_powered_by,notnull,default:true"`
	SLATarget            *float64  `bun:"sla_target"`
	CreatedAt            time.Time `bun:"created_at,notnull"`
	UpdatedAt            time.Time `bun:"updated_at,notnull"`
}

// ToDomain converts a StatusPageModel to a domain.StatusPage.
func (m *StatusPageModel) ToDomain() *domain.StatusPage {
	return &domain.StatusPage{
		ID:                   m.ID,
		Slug:                 m.Slug,
		Title:                m.Title,
		Description:          m.Description,
		Icon:                 m.Icon,
		Favicon:              m.Favicon,
		Theme:                m.Theme,
		Published:            m.Published,
		CustomDomain:         m.CustomDomain,
		PasswordHash:         m.PageAccessCredential,
		FooterText:           m.FooterText,
		CustomCSS:            m.CustomCSS,
		DashboardStyle:       domain.NormalizeDashboardStyle(m.DashboardStyle),
		ShowTags:             m.ShowTags,
		AutoResolveIncidents: m.AutoResolveIncidents,
		ShowPoweredBy:        m.ShowPoweredBy,
		SLATarget:            m.SLATarget,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
	}
}

// StatusPageModelFromDomain converts a domain.StatusPage to a StatusPageModel.
func StatusPageModelFromDomain(sp *domain.StatusPage) *StatusPageModel {
	return &StatusPageModel{
		ID:                   sp.ID,
		Slug:                 sp.Slug,
		Title:                sp.Title,
		Description:          sp.Description,
		Icon:                 sp.Icon,
		Favicon:              sp.Favicon,
		Theme:                sp.Theme,
		Published:            sp.Published,
		CustomDomain:         sp.CustomDomain,
		PageAccessCredential: sp.PasswordHash,
		FooterText:           sp.FooterText,
		CustomCSS:            sp.CustomCSS,
		DashboardStyle:       domain.NormalizeDashboardStyle(sp.DashboardStyle),
		ShowTags:             sp.ShowTags,
		AutoResolveIncidents: sp.AutoResolveIncidents,
		ShowPoweredBy:        sp.ShowPoweredBy,
		SLATarget:            sp.SLATarget,
		CreatedAt:            sp.CreatedAt,
		UpdatedAt:            sp.UpdatedAt,
	}
}

// TagModel maps the tags table.
type TagModel struct {
	bun.BaseModel `bun:"table:tags"`

	ID    int64  `bun:"id,pk,autoincrement"`
	Name  string `bun:"name,notnull"`
	Color string `bun:"color,notnull,default:'#666666'"`
}

// ToDomain converts a TagModel to a domain.Tag.
func (m *TagModel) ToDomain() *domain.Tag {
	return &domain.Tag{ID: m.ID, Name: m.Name, Color: m.Color}
}

// TagModelFromDomain converts a domain.Tag to a TagModel.
func TagModelFromDomain(t *domain.Tag) *TagModel {
	return &TagModel{ID: t.ID, Name: t.Name, Color: t.Color}
}

// MonitorTagModel maps the monitor_tags table.
type MonitorTagModel struct {
	bun.BaseModel `bun:"table:monitor_tags"`

	ID        int64  `bun:"id,pk,autoincrement"`
	MonitorID int64  `bun:"monitor_id,notnull"`
	TagID     int64  `bun:"tag_id,notnull"`
	Value     string `bun:"value"`
}

// ToDomain converts a MonitorTagModel to a domain.MonitorTag.
func (m *MonitorTagModel) ToDomain() *domain.MonitorTag {
	return &domain.MonitorTag{ID: m.ID, MonitorID: m.MonitorID, TagID: m.TagID, Value: m.Value}
}

// MaintenanceWindowModel maps the maintenance_windows table.
type MaintenanceWindowModel struct {
	bun.BaseModel `bun:"table:maintenance_windows"`

	ID          int64     `bun:"id,pk,autoincrement"`
	UserID      int64     `bun:"user_id"`
	Title       string    `bun:"title,notnull"`
	Description string    `bun:"description,notnull"`
	Active      bool      `bun:"active,notnull,default:true"`
	Strategy    string    `bun:"strategy,notnull,default:'single'"`
	StartDate   time.Time `bun:"start_date,nullzero"`
	EndDate     time.Time `bun:"end_date,nullzero"`
	CronExpr    string    `bun:"cron_expr"`
	Duration    int       `bun:"duration"`
	Timezone    string    `bun:"timezone,notnull,default:'UTC'"`
}

// ToDomain converts a MaintenanceWindowModel to a domain.MaintenanceWindow.
func (m *MaintenanceWindowModel) ToDomain() *domain.MaintenanceWindow {
	return &domain.MaintenanceWindow{
		ID:          m.ID,
		UserID:      m.UserID,
		Title:       m.Title,
		Description: m.Description,
		Active:      m.Active,
		Strategy:    m.Strategy,
		StartDate:   m.StartDate,
		EndDate:     m.EndDate,
		CronExpr:    m.CronExpr,
		Duration:    m.Duration,
		Timezone:    m.Timezone,
	}
}

// MaintenanceWindowModelFromDomain converts a domain.MaintenanceWindow to a model.
func MaintenanceWindowModelFromDomain(mw *domain.MaintenanceWindow) *MaintenanceWindowModel {
	tz := mw.Timezone
	if tz == "" {
		tz = "UTC"
	}
	return &MaintenanceWindowModel{
		ID:          mw.ID,
		UserID:      mw.UserID,
		Title:       mw.Title,
		Description: mw.Description,
		Active:      mw.Active,
		Strategy:    mw.Strategy,
		StartDate:   mw.StartDate,
		EndDate:     mw.EndDate,
		CronExpr:    mw.CronExpr,
		Duration:    mw.Duration,
		Timezone:    tz,
	}
}

// APIKeyModel maps the api_keys table.
type APIKeyModel struct {
	bun.BaseModel `bun:"table:api_keys"`

	ID         int64      `bun:"id,pk,autoincrement"`
	UserID     int64      `bun:"user_id,notnull"`
	Name       string     `bun:"name,notnull"`
	KeyDigest  string     `bun:"key_hash,notnull"` // SHA-256 digest, not the raw key
	Active     bool       `bun:"active,notnull,default:true"`
	ExpiresAt  *time.Time `bun:"expires_at"`
	Scopes     string     `bun:"scopes,notnull"` // JSON array, default '["read"]' set by DB
	LastUsedAt *time.Time `bun:"last_used_at"`
	CreatedAt  time.Time  `bun:"created_at,notnull"`
}

// ToDomain converts an APIKeyModel to a domain.APIKey.
func (m *APIKeyModel) ToDomain() *domain.APIKey {
	var scopes []string
	if m.Scopes != "" {
		_ = json.Unmarshal([]byte(m.Scopes), &scopes)
	}
	if scopes == nil {
		scopes = []string{}
	}
	return &domain.APIKey{
		ID:         m.ID,
		UserID:     m.UserID,
		Name:       m.Name,
		KeyHash:    m.KeyDigest,
		Active:     m.Active,
		ExpiresAt:  m.ExpiresAt,
		Scopes:     scopes,
		LastUsedAt: m.LastUsedAt,
		CreatedAt:  m.CreatedAt,
	}
}

// APIKeyModelFromDomain converts a domain.APIKey to an APIKeyModel.
func APIKeyModelFromDomain(ak *domain.APIKey) *APIKeyModel {
	scopesJSON, _ := json.Marshal(ak.Scopes)
	if ak.Scopes == nil {
		scopesJSON = []byte(`["read"]`)
	}
	return &APIKeyModel{
		ID:         ak.ID,
		UserID:     ak.UserID,
		Name:       ak.Name,
		KeyDigest:  ak.KeyHash,
		Active:     ak.Active,
		ExpiresAt:  ak.ExpiresAt,
		Scopes:     string(scopesJSON),
		LastUsedAt: ak.LastUsedAt,
		CreatedAt:  ak.CreatedAt,
	}
}

// SettingModel maps the settings table.
type SettingModel struct {
	bun.BaseModel `bun:"table:settings"`

	ID    int64  `bun:"id,pk,autoincrement"`
	Key   string `bun:"setting_key,notnull"`
	Value string `bun:"value,notnull"`
}

// IncidentModel maps the incidents table.
type IncidentModel struct {
	bun.BaseModel `bun:"table:incidents"`

	ID           int64     `bun:"id,pk,autoincrement"`
	StatusPageID int64     `bun:"status_page_id,notnull"`
	Title        string    `bun:"title,notnull"`
	Content      string    `bun:"content,notnull"`
	Style        string    `bun:"style,notnull,default:'warning'"`
	Pinned       bool      `bun:"pinned,notnull,default:true"`
	Active       bool      `bun:"active,notnull,default:true"`
	CreatedAt    time.Time `bun:"created_at,notnull"`
}

// ToDomain converts an IncidentModel to a domain.Incident.
func (m *IncidentModel) ToDomain() *domain.Incident {
	return &domain.Incident{
		ID:           m.ID,
		StatusPageID: m.StatusPageID,
		Title:        m.Title,
		Content:      m.Content,
		Style:        m.Style,
		Pinned:       m.Pinned,
		Active:       m.Active,
		CreatedAt:    m.CreatedAt,
	}
}

// IncidentUpdateModel maps the incident_updates table.
type IncidentUpdateModel struct {
	bun.BaseModel `bun:"table:incident_updates"`

	ID           int64     `bun:"id,pk,autoincrement"`
	IncidentID   int64     `bun:"incident_id,notnull"`
	StatusPageID int64     `bun:"status_page_id,notnull"`
	Status       string    `bun:"status,notnull"`
	Content      string    `bun:"content,notnull"`
	CreatedAt    time.Time `bun:"created_at,notnull"`
}

// ToDomain converts an IncidentUpdateModel to a domain.IncidentUpdate.
func (m *IncidentUpdateModel) ToDomain() *domain.IncidentUpdate {
	return &domain.IncidentUpdate{
		ID:           m.ID,
		IncidentID:   m.IncidentID,
		StatusPageID: m.StatusPageID,
		Status:       domain.NormalizeIncidentStatus(m.Status),
		Content:      m.Content,
		CreatedAt:    m.CreatedAt,
	}
}

// IncidentUpdateModelFromDomain converts a domain.IncidentUpdate to an IncidentUpdateModel.
func IncidentUpdateModelFromDomain(update *domain.IncidentUpdate) *IncidentUpdateModel {
	return &IncidentUpdateModel{
		ID:           update.ID,
		IncidentID:   update.IncidentID,
		StatusPageID: update.StatusPageID,
		Status:       string(update.Status),
		Content:      update.Content,
		CreatedAt:    update.CreatedAt,
	}
}

// ProxyModel maps the proxies table. There are no created_at/updated_at
// columns on this table (see migrations/001_init.up.sql), so unlike most
// models here there is no timestamp to default on Create.
type ProxyModel struct {
	bun.BaseModel `bun:"table:proxies"`

	ID        int64  `bun:"id,pk,autoincrement"`
	UserID    int64  `bun:"user_id,notnull"`
	Protocol  string `bun:"protocol,notnull"`
	Host      string `bun:"host,notnull"`
	Port      int    `bun:"port,notnull"`
	Auth      bool   `bun:"auth,notnull,default:false"`
	Username  string `bun:"username"`
	Password  string `bun:"password"` // plaintext proxy credential — never serialize to a View
	Active    bool   `bun:"active,notnull,default:true"`
	IsDefault bool   `bun:"is_default,notnull,default:false"`
}

// ToDomain converts a ProxyModel to a domain.Proxy.
func (m *ProxyModel) ToDomain() *domain.Proxy {
	return &domain.Proxy{
		ID:        m.ID,
		UserID:    m.UserID,
		Protocol:  m.Protocol,
		Host:      m.Host,
		Port:      m.Port,
		Auth:      m.Auth,
		Username:  m.Username,
		Password:  m.Password,
		Active:    m.Active,
		IsDefault: m.IsDefault,
	}
}

// ProxyModelFromDomain converts a domain.Proxy to a ProxyModel.
func ProxyModelFromDomain(p *domain.Proxy) *ProxyModel {
	return &ProxyModel{
		ID:        p.ID,
		UserID:    p.UserID,
		Protocol:  p.Protocol,
		Host:      p.Host,
		Port:      p.Port,
		Auth:      p.Auth,
		Username:  p.Username,
		Password:  p.Password,
		Active:    p.Active,
		IsDefault: p.IsDefault,
	}
}

// DockerHostModel maps the docker_hosts table.
type DockerHostModel struct {
	bun.BaseModel `bun:"table:docker_hosts"`

	ID           int64  `bun:"id,pk,autoincrement"`
	UserID       int64  `bun:"user_id,notnull"`
	Name         string `bun:"name,notnull"`
	DockerDaemon string `bun:"docker_daemon,notnull"`
	DockerType   string `bun:"docker_type,notnull,default:'socket'"`
}

// TLSInfoModel maps the tls_info table.
type TLSInfoModel struct {
	bun.BaseModel `bun:"table:tls_info"`

	ID        int64     `bun:"id,pk,autoincrement"`
	MonitorID int64     `bun:"monitor_id,notnull"`
	InfoJSON  JSONField `bun:"info_json,notnull"`
	CheckedAt time.Time `bun:"checked_at,notnull"`
}

// StatusPageCnameModel maps the status_page_cnames table.
type StatusPageCnameModel struct {
	bun.BaseModel `bun:"table:status_page_cnames"`

	ID           int64  `bun:"id,pk,autoincrement"`
	StatusPageID int64  `bun:"status_page_id,notnull"`
	Domain       string `bun:"domain,notnull"`
}

// StatusPageMonitorModel maps the status_page_monitors table.
type StatusPageMonitorModel struct {
	bun.BaseModel `bun:"table:status_page_monitors"`

	ID           int64 `bun:"id,pk,autoincrement"`
	StatusPageID int64 `bun:"status_page_id,notnull"`
	MonitorID    int64 `bun:"monitor_id,notnull"`
	DisplayOrder int   `bun:"display_order,notnull,default:1000"`
}

// StatusPageSubscriberModel maps the dormant pre-Sprint-C webhook table
// status_page_subscribers_legacy_webhook (renamed by migration 014).
// Domain.StatusPageSubscriber is now the email model; live code uses
// StatusPageEmailSubscriberModel. Integrator removes this legacy model
// after Track A lands.
type StatusPageSubscriberModel struct {
	bun.BaseModel `bun:"table:status_page_subscribers_legacy_webhook"`

	ID           int64     `bun:"id,pk,autoincrement"`
	StatusPageID int64     `bun:"status_page_id,notnull"`
	URL          string    `bun:"url,notnull"`
	Active       bool      `bun:"active,notnull,default:true"`
	Secret       string    `bun:"secret"`
	CreatedAt    time.Time `bun:"created_at,notnull"`
}

// MonitorNotificationModel maps the monitor_notification table.
type MonitorNotificationModel struct {
	bun.BaseModel `bun:"table:monitor_notification"`

	ID             int64 `bun:"id,pk,autoincrement"`
	MonitorID      int64 `bun:"monitor_id,notnull"`
	NotificationID int64 `bun:"notification_id,notnull"`
}

// MaintenanceWindowMonitorModel maps the maintenance_window_monitors table.
type MaintenanceWindowMonitorModel struct {
	bun.BaseModel `bun:"table:maintenance_window_monitors"`

	ID                  int64 `bun:"id,pk,autoincrement"`
	MaintenanceWindowID int64 `bun:"maintenance_window_id,notnull"`
	MonitorID           int64 `bun:"monitor_id,notnull"`
}

// NotificationSentHistoryModel maps the notification_sent_history table.
type NotificationSentHistoryModel struct {
	bun.BaseModel `bun:"table:notification_sent_history"`

	ID             int64     `bun:"id,pk,autoincrement"`
	NotificationID int64     `bun:"notification_id,notnull"`
	MonitorID      int64     `bun:"monitor_id,notnull"`
	LastSentAt     time.Time `bun:"last_sent_at,notnull"`
}

// IncidentModelFromDomain converts a domain.Incident to an IncidentModel.
func IncidentModelFromDomain(inc *domain.Incident) *IncidentModel {
	return &IncidentModel{
		ID:           inc.ID,
		StatusPageID: inc.StatusPageID,
		Title:        inc.Title,
		Content:      inc.Content,
		Style:        inc.Style,
		Pinned:       inc.Pinned,
		Active:       inc.Active,
		CreatedAt:    inc.CreatedAt,
	}
}

// AlertModel maps the alerts table (F2.2 alert lifecycle).
type AlertModel struct {
	bun.BaseModel `bun:"table:alerts"`

	ID            int64      `bun:"id,pk,autoincrement"`
	MonitorID     int64      `bun:"monitor_id,notnull"`
	Status        string     `bun:"status,notnull"`
	Message       string     `bun:"message,notnull"`
	FiredAt       time.Time  `bun:"fired_at,notnull"`
	AckedAt       *time.Time `bun:"acked_at"`
	AckedByUserID *int64     `bun:"acked_by_user_id"`
	ResolvedAt    *time.Time `bun:"resolved_at"`
	AckToken      string     `bun:"ack_token,notnull"`
	OpenMonitorID *int64     `bun:"open_monitor_id"`
	CreatedAt     time.Time  `bun:"created_at,notnull"`
	UpdatedAt     time.Time  `bun:"updated_at,notnull"`
}

// ToDomain converts an AlertModel to a domain.Alert.
func (m *AlertModel) ToDomain() *domain.Alert {
	if m == nil {
		return nil
	}
	return &domain.Alert{
		ID:            m.ID,
		MonitorID:     m.MonitorID,
		Status:        m.Status,
		Message:       m.Message,
		FiredAt:       m.FiredAt,
		AckedAt:       m.AckedAt,
		AckedByUserID: m.AckedByUserID,
		ResolvedAt:    m.ResolvedAt,
		AckToken:      m.AckToken,
		OpenMonitorID: m.OpenMonitorID,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
}

// AlertModelFromDomain converts a domain.Alert to an AlertModel.
func AlertModelFromDomain(a *domain.Alert) *AlertModel {
	return &AlertModel{
		ID:            a.ID,
		MonitorID:     a.MonitorID,
		Status:        a.Status,
		Message:       a.Message,
		FiredAt:       a.FiredAt,
		AckedAt:       a.AckedAt,
		AckedByUserID: a.AckedByUserID,
		ResolvedAt:    a.ResolvedAt,
		AckToken:      a.AckToken,
		OpenMonitorID: a.OpenMonitorID,
		CreatedAt:     a.CreatedAt,
		UpdatedAt:     a.UpdatedAt,
	}
}

// MonitorNotificationFromDomain converts a domain.MonitorNotification to a MonitorNotificationModel.
func MonitorNotificationFromDomain(mn *domain.MonitorNotification) *MonitorNotificationModel {
	return &MonitorNotificationModel{
		ID:             mn.ID,
		MonitorID:      mn.MonitorID,
		NotificationID: mn.NotificationID,
	}
}

// ToDomainMonitorNotification converts a MonitorNotificationModel to a domain.MonitorNotification.
func (m *MonitorNotificationModel) ToDomainMonitorNotification() *domain.MonitorNotification {
	return &domain.MonitorNotification{
		ID:             m.ID,
		MonitorID:      m.MonitorID,
		NotificationID: m.NotificationID,
	}
}

// StatusPageCnameModelFromDomain converts a domain.StatusPageCNAME to a StatusPageCnameModel.
func StatusPageCnameModelFromDomain(cname *domain.StatusPageCNAME) *StatusPageCnameModel {
	return &StatusPageCnameModel{
		ID:           cname.ID,
		StatusPageID: cname.StatusPageID,
		Domain:       cname.Domain,
	}
}

// ToDomainCNAME converts a StatusPageCnameModel to a domain.StatusPageCNAME.
func (m *StatusPageCnameModel) ToDomainCNAME() *domain.StatusPageCNAME {
	return &domain.StatusPageCNAME{
		ID:           m.ID,
		StatusPageID: m.StatusPageID,
		Domain:       m.Domain,
	}
}

// StatusPageMonitorModelFromDomain converts a domain.StatusPageMonitor to a StatusPageMonitorModel.
func StatusPageMonitorModelFromDomain(spm *domain.StatusPageMonitor) *StatusPageMonitorModel {
	return &StatusPageMonitorModel{
		ID:           spm.ID,
		StatusPageID: spm.StatusPageID,
		MonitorID:    spm.MonitorID,
		DisplayOrder: spm.DisplayOrder,
	}
}

// ToDomainSPMonitor converts a StatusPageMonitorModel to a domain.StatusPageMonitor.
func (m *StatusPageMonitorModel) ToDomainSPMonitor() *domain.StatusPageMonitor {
	return &domain.StatusPageMonitor{
		ID:           m.ID,
		StatusPageID: m.StatusPageID,
		MonitorID:    m.MonitorID,
		DisplayOrder: m.DisplayOrder,
	}
}

// NotificationSentHistoryModelFromDomain converts a domain.NotificationSentHistory to a NotificationSentHistoryModel.
func NotificationSentHistoryModelFromDomain(nsh *domain.NotificationSentHistory) *NotificationSentHistoryModel {
	return &NotificationSentHistoryModel{
		ID:             nsh.ID,
		NotificationID: nsh.NotificationID,
		MonitorID:      nsh.MonitorID,
		LastSentAt:     nsh.LastSentAt,
	}
}

// ToDomainSentHistory converts a NotificationSentHistoryModel to domain.NotificationSentHistory.
func (m *NotificationSentHistoryModel) ToDomainSentHistory() *domain.NotificationSentHistory {
	return &domain.NotificationSentHistory{
		ID:             m.ID,
		NotificationID: m.NotificationID,
		MonitorID:      m.MonitorID,
		LastSentAt:     m.LastSentAt,
	}
}

// CNAMEModelFromDomain is an alias for backwards compatibility.
func CNAMEModelFromDomain(cname *domain.StatusPageCNAME) *StatusPageCnameModel {
	return StatusPageCnameModelFromDomain(cname)
}
