package domain

import "time"

const (
	// DashboardStyleFull renders the detailed monitor presentation.
	DashboardStyleFull = "full"
	// DashboardStyleGrid renders monitor cards in a compact grid.
	DashboardStyleGrid = "grid"
	// DashboardStylePills renders monitors as compact status pills.
	DashboardStylePills = "pills"
)

// NormalizeDashboardStyle returns a supported dashboard style, defaulting
// empty and unknown values to the backwards-compatible full presentation.
func NormalizeDashboardStyle(style string) string {
	switch style {
	case DashboardStyleGrid, DashboardStylePills:
		return style
	default:
		return DashboardStyleFull
	}
}

// StatusPageMonitor represents the link between a status page and a monitor.
type StatusPageMonitor struct {
	ID           int64
	StatusPageID int64
	MonitorID    int64
	DisplayOrder int
}

// StatusPageCNAME represents a custom domain CNAME for a status page.
type StatusPageCNAME struct {
	ID           int64
	StatusPageID int64
	Domain       string
}

// StatusPage represents a public status page configuration.
type StatusPage struct {
	ID          int64
	Slug        string
	Title       string
	Description string
	// Icon is the page logo (http(s) URL or data:image data-URL). Empty uses
	// the default Phoenix mark when ShowPoweredBy is true.
	Icon string
	// Favicon is the browser-tab icon (http(s) or data:image). Empty leaves
	// the default site favicon.
	Favicon              string
	Theme                string // "light", "dark", "auto"
	Published            bool
	CustomDomain         string
	PasswordHash         string
	FooterText           string
	CustomCSS            string
	DashboardStyle       string // normalized through NormalizeDashboardStyle
	ShowTags             bool
	AutoResolveIncidents bool
	// ShowPoweredBy controls the public "Powered by Phoenix" mark and the
	// default mascot when Icon is empty. Default true (F3.5 white-label).
	ShowPoweredBy bool
	SLATarget     *float64 // nil hides the optional SLA target on public uptime history
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// StatusPageSubscriber is a double-opt-in email subscriber for a status page.
//
// Active is false until the subscriber confirms via the emailed token.
// ConfirmedAt is set when Active becomes true and cleared on re-subscribe
// of a deleted row (new pending subscription).
//
// Pre-Sprint-C webhook rows (url/secret) live in the legacy table
// status_page_subscribers_legacy_webhook and are not represented here.
type StatusPageSubscriber struct {
	ID           int64
	StatusPageID int64
	Email        string // normalized lowercase
	Active       bool   // true only after confirmation
	ConfirmedAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// StatusPageSubscriptionChannel binds one SMTP notification channel to a
// status page for outbound subscription mail (confirmation, incidents,
// maintenance). At most one row per status page (PK = StatusPageID).
type StatusPageSubscriptionChannel struct {
	StatusPageID   int64
	NotificationID int64
}
