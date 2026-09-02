package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// publicChartRangeHours is the fixed lookback window for the public status
// page's per-monitor response-time chart.
const publicChartRangeHours = 24

const (
	// StatusPageAccessCodeMinLength is the minimum number of Unicode characters
	// accepted for new or replacement status-page access codes.
	StatusPageAccessCodeMinLength = 8
	// StatusPageAccessCodeMaxBytes matches bcrypt's maximum UTF-8 input size at
	// the password adapter boundary.
	StatusPageAccessCodeMaxBytes = 72
	// statusPageBrandAssetMaxDecoded is the max decoded payload for data:image
	// logos/favicons (F3.5). Keeps rows bounded while allowing small PNG/SVG uploads.
	statusPageBrandAssetMaxDecoded = 256 * 1024
)

// publicChartHeartbeatMax bounds the number of raw heartbeat samples read
// per monitor before bucketing. A public status page can list many
// monitors in one request, so the per-monitor cap is tighter than the
// authenticated single-monitor chart endpoint.
const publicChartHeartbeatMax = 500

// statusPageIncidentNotifier is satisfied by SubscriptionService for email
// fan-out on incident create/resolve. Optional — nil means no email.
type statusPageIncidentNotifier interface {
	NotifyIncidentCreated(ctx context.Context, inc *domain.Incident) error
	NotifyIncidentResolved(ctx context.Context, inc *domain.Incident) error
	NotifyIncidentUpdated(ctx context.Context, inc *domain.Incident, update *domain.IncidentUpdate) error
}

// statusPageSubscriptionAvailability reports whether a page accepts new
// email subscribers (PUBLIC_URL + active SMTP channel). Optional.
type statusPageSubscriptionAvailability interface {
	SubscriptionsAvailable(ctx context.Context, statusPageID int64) bool
}

// StatusPageService handles status page CRUD, incident management, custom
// domain aliases, and monitor assignments.
type StatusPageService struct {
	repo            ports.StatusPageRepository
	incidentRepo    ports.IncidentRepository
	incidentUpdates ports.IncidentUpdateRepository
	cnameRepo       ports.StatusPageCNAMERepository
	spMonitorRepo   ports.StatusPageMonitorRepository
	monitorRepo     ports.MonitorRepository
	hbRepo          ports.HeartbeatRepository
	passwords       ports.PasswordHasher
	tlsInfo         ports.TLSInfoRepository
	incidentMail    statusPageIncidentNotifier
	subAvail        statusPageSubscriptionAvailability
}

const (
	// publicUptimeBarDays is the length of the public status page's uptime bar,
	// matching the 90-day bar the frontend renders.
	publicUptimeBarDays = 90
	// publicUptimeHistoryMonths is the number of calendar-month summaries shown.
	publicUptimeHistoryMonths = 12
	// publicUptimeHistoryQuarters is the number of calendar-quarter summaries shown.
	publicUptimeHistoryQuarters = 4
)

// PublicMonitorStatus holds the status of a single monitor on a public status page.
type PublicMonitorStatus struct {
	ID             int64               `json:"id"`
	Name           string              `json:"name"`
	Type           string              `json:"type"`
	Status         string              `json:"status"`
	UptimePercent  *float64            `json:"uptime_percent"`
	UptimeData     []PublicUptimeDay   `json:"uptime_data"`
	UptimeHistory  PublicUptimeHistory `json:"uptime_history"`
	Chart          *PublicMonitorChart `json:"chart"`
	CertExpiryDate *string             `json:"cert_expiry_date,omitempty"`
	CertDaysLeft   *int                `json:"cert_days_left,omitempty"`
}

// PublicUptimeDay is one day of the public status page's uptime bar. The bar is
// always exactly publicUptimeBarDays entries, oldest first, so the frontend can
// render a fixed-width strip without padding it itself.
type PublicUptimeDay struct {
	Date   string `json:"date"`   // YYYY-MM-DD, UTC
	Status string `json:"status"` // up | down | pending | maintenance | none
}

// PublicUptimePeriod summarizes measured uptime for one UTC calendar period.
// UptimePercent is nil when the period has no effective checks; Phoenix never
// invents a pass or failure when there is no evidence.
type PublicUptimePeriod struct {
	Label         string   `json:"label"`
	StartDate     string   `json:"start_date"`
	EndDate       string   `json:"end_date"`
	UptimePercent *float64 `json:"uptime_percent"`
	Complete      bool     `json:"complete"`
}

// PublicUptimeHistory contains newest-first monthly and quarterly summaries.
type PublicUptimeHistory struct {
	Monthly   []PublicUptimePeriod `json:"monthly"`
	Quarterly []PublicUptimePeriod `json:"quarterly"`
}

// PublicChartBucket is the wire shape of a single bucketed response-time
// sample on a public status page monitor chart. Field names/types mirror
// the authenticated chart_aggregate endpoint's chartBucketView so the
// frontend ResponseTimeChart component can consume either without
// translation.
type PublicChartBucket struct {
	Time string  `json:"time"`
	Min  int     `json:"min"`
	Avg  float64 `json:"avg"`
	Max  int     `json:"max"`
}

// PublicDowntimeInterval is the wire shape of a contiguous down/pending
// period on a public status page monitor chart.
type PublicDowntimeInterval struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// PublicMonitorChart holds bounded response-time chart data for a single
// monitor on a public status page. It is a service-layer DTO with json
// tags — never a domain type — and is always non-nil with (possibly empty)
// slices so the frontend never has to special-case a missing chart object.
type PublicMonitorChart struct {
	Buckets           []PublicChartBucket      `json:"buckets"`
	DowntimeIntervals []PublicDowntimeInterval `json:"downtime_intervals"`
}

// PublicIncidentUpdateView is the wire shape of an incident timeline update.
type PublicIncidentUpdateView struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// PublicIncidentView is the wire shape of an incident on a public status page.
type PublicIncidentView struct {
	ID         int64                       `json:"id"`
	Title      string                      `json:"title"`
	Content    string                      `json:"content"`
	Style      string                      `json:"style"`
	Active     bool                        `json:"active"`
	CreatedAt  string                      `json:"created_at"`
	ResolvedAt string                      `json:"resolved_at,omitempty"`
	Updates    []*PublicIncidentUpdateView `json:"updates"`
}

// PublicStatusResponse is the full response for a public status page.
type PublicStatusResponse struct {
	StatusPage             *domain.StatusPage     `json:"status_page"`
	Monitors               []*PublicMonitorStatus `json:"monitors"`
	Incidents              []*PublicIncidentView  `json:"incidents"`
	SubscriptionsAvailable bool                   `json:"subscriptions_available"`
}

// NewStatusPageService creates a new StatusPageService with all required
// repository dependencies.
func NewStatusPageService(
	repo ports.StatusPageRepository,
	incidentRepo ports.IncidentRepository,
	cnameRepo ports.StatusPageCNAMERepository,
	spMonitorRepo ports.StatusPageMonitorRepository,
	monitorRepo ports.MonitorRepository,
	hbRepo ports.HeartbeatRepository,
	passwords ports.PasswordHasher,
) *StatusPageService {
	return &StatusPageService{
		repo:          repo,
		incidentRepo:  incidentRepo,
		cnameRepo:     cnameRepo,
		spMonitorRepo: spMonitorRepo,
		monitorRepo:   monitorRepo,
		hbRepo:        hbRepo,
		passwords:     passwords,
	}
}

// SetTLSInfoRepo attaches a TLS info repository so public status pages can
// surface certificate expiry when cached data exists. Never invents zeros.
func (s *StatusPageService) SetTLSInfoRepo(repo ports.TLSInfoRepository) {
	s.tlsInfo = repo
}

// SetIncidentNotifier attaches best-effort email fan-out for incident
// create/resolve/update (SubscriptionService). CRUD still succeeds if mail fails.
func (s *StatusPageService) SetIncidentNotifier(n statusPageIncidentNotifier) {
	s.incidentMail = n
}

// SetIncidentUpdateRepo attaches timeline-update persistence. It is optional so
// older tests can wire the incident service narrowly, but production sets it.
func (s *StatusPageService) SetIncidentUpdateRepo(repo ports.IncidentUpdateRepository) {
	s.incidentUpdates = repo
}

// SetSubscriptionAvailability attaches the PUBLIC_URL + SMTP channel probe
// used for the public subscriptions_available flag.
func (s *StatusPageService) SetSubscriptionAvailability(a statusPageSubscriptionAvailability) {
	s.subAvail = a
}

// Create creates a new public status page.
func (s *StatusPageService) Create(ctx context.Context, sp *domain.StatusPage) error {
	if sp.Slug == "" {
		return fmt.Errorf("status page service: %w: slug is required", domain.ErrValidation)
	}
	sp.Slug = strings.ToLower(strings.TrimSpace(sp.Slug))
	sp.DashboardStyle = domain.NormalizeDashboardStyle(sp.DashboardStyle)
	if err := normalizeStatusPageSLATarget(sp); err != nil {
		return err
	}
	if err := validateStatusPageBranding(sp); err != nil {
		return err
	}
	// F3.5: default show_powered_by true when the caller left it unset via zero value.
	// Handlers that intentionally hide branding pass ShowPoweredBy=false after binding *bool.
	// Create always receives an explicit value from the handler once F3.5 is wired.
	return s.repo.Create(ctx, sp)
}

// GetBySlug retrieves a published status page by its slug.
func (s *StatusPageService) GetBySlug(ctx context.Context, slug string) (*domain.StatusPage, error) {
	return s.repo.GetBySlug(ctx, slug)
}

// GetPublicStatus returns public status-page data. Access-protected pages
// return only their public metadata until GetPublicStatusWithAccess verifies
// an access code; monitor and incident data never crosses the anonymous wire.
func (s *StatusPageService) GetPublicStatus(ctx context.Context, slug string) (*PublicStatusResponse, error) {
	return s.getPublicStatus(ctx, slug, nil)
}

// GetPublicStatusWithAccess verifies an access code and returns the complete
// public status-page payload. Unprotected pages also accept this path.
func (s *StatusPageService) GetPublicStatusWithAccess(
	ctx context.Context,
	slug string,
	accessCode string,
) (*PublicStatusResponse, error) {
	return s.getPublicStatus(ctx, slug, &accessCode)
}

func (s *StatusPageService) getPublicStatus(
	ctx context.Context,
	slug string,
	accessCode *string,
) (*PublicStatusResponse, error) {
	sp, err := s.repo.GetBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("get public status: %w", err)
	}
	if !sp.Published {
		return nil, fmt.Errorf("get public status: %w", domain.ErrNotFound)
	}

	resp := &PublicStatusResponse{
		StatusPage: sp,
		Monitors:   []*PublicMonitorStatus{},
		Incidents:  []*PublicIncidentView{},
	}
	if s.subAvail != nil {
		resp.SubscriptionsAvailable = s.subAvail.SubscriptionsAvailable(ctx, sp.ID)
	}
	if sp.PasswordHash != "" {
		if accessCode == nil {
			return resp, nil
		}
		if s.passwords == nil {
			return nil, fmt.Errorf("get public status: %w: password hasher is not configured", domain.ErrInternal)
		}
		if err := s.passwords.Verify(sp.PasswordHash, *accessCode); err != nil {
			return nil, fmt.Errorf("get public status: %w", domain.ErrUnauthorized)
		}
	}

	// Get assigned monitors. A failure here degrades gracefully: we still
	// return the page (it may legitimately have zero monitors), but surface
	// the error so operators notice a degraded backend.
	spMonitors, err := s.spMonitorRepo.ListByStatusPage(ctx, sp.ID)
	if err != nil {
		slog.Warn("status page service: list monitors failed, returning partial page", "slug", slug, "error", err)
		return resp, nil
	}

	for _, spm := range spMonitors {
		if ms := s.monitorPublicStatus(ctx, spm.MonitorID); ms != nil {
			resp.Monitors = append(resp.Monitors, ms)
		}
	}

	// Get incidents and their timeline updates.
	incidents, err := s.incidentRepo.ListByStatusPage(ctx, sp.ID)
	if err == nil {
		updatesByIncident := s.incidentUpdatesByIncident(ctx, sp.ID)
		for _, inc := range incidents {
			updates := updatesByIncident[inc.ID]
			iv := &PublicIncidentView{
				ID:        inc.ID,
				Title:     inc.Title,
				Content:   inc.Content,
				Style:     inc.Style,
				Active:    inc.Active,
				CreatedAt: inc.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
				Updates:   publicIncidentUpdateViews(updates),
			}
			if !inc.Active {
				if resolvedAt := resolvedAtFromUpdates(updates); !resolvedAt.IsZero() {
					iv.ResolvedAt = resolvedAt.UTC().Format("2006-01-02T15:04:05Z")
				}
			}
			resp.Incidents = append(resp.Incidents, iv)
		}
	}

	return resp, nil
}

func (s *StatusPageService) incidentUpdatesByIncident(
	ctx context.Context,
	statusPageID int64,
) map[int64][]*domain.IncidentUpdate {
	updatesByIncident := make(map[int64][]*domain.IncidentUpdate)
	if s.incidentUpdates == nil {
		return updatesByIncident
	}
	updates, err := s.incidentUpdates.ListByStatusPage(ctx, statusPageID)
	if err != nil {
		slog.Warn("status page service: list incident updates failed", "status_page_id", statusPageID, "error", err)
		return updatesByIncident
	}
	for _, update := range updates {
		updatesByIncident[update.IncidentID] = append(updatesByIncident[update.IncidentID], update)
	}
	return updatesByIncident
}

func publicIncidentUpdateViews(updates []*domain.IncidentUpdate) []*PublicIncidentUpdateView {
	views := make([]*PublicIncidentUpdateView, 0, len(updates))
	for _, update := range updates {
		views = append(views, &PublicIncidentUpdateView{
			ID:        update.ID,
			Status:    string(update.Status),
			Content:   update.Content,
			CreatedAt: update.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return views
}

func resolvedAtFromUpdates(updates []*domain.IncidentUpdate) time.Time {
	for i := len(updates) - 1; i >= 0; i-- {
		if updates[i].Status == domain.IncidentStatusResolved {
			return updates[i].CreatedAt
		}
	}
	return time.Time{}
}

// monitorPublicStatus builds a single monitor's public status row. It returns
// nil if the monitor cannot be loaded (e.g. it was deleted after being
// assigned), so the caller can skip it without failing the whole page.
func (s *StatusPageService) monitorPublicStatus(ctx context.Context, monitorID int64) *PublicMonitorStatus {
	mon, err := s.monitorRepo.GetByID(ctx, monitorID)
	if err != nil {
		return nil
	}

	ms := &PublicMonitorStatus{
		ID:     mon.ID,
		Name:   mon.Name,
		Type:   mon.Type,
		Status: "pending",
	}

	// Get latest heartbeat for status.
	if hb, err := s.hbRepo.GetLatest(ctx, mon.ID); err == nil && hb != nil {
		switch hb.Status {
		case domain.StatusUp:
			ms.Status = "up"
		case domain.StatusDown:
			ms.Status = "down"
		case domain.StatusMaintenance:
			ms.Status = "maintenance"
		}
	}

	ms.UptimeData, ms.UptimePercent = s.monitorUptimeBar(ctx, mon.ID)
	ms.UptimeHistory = s.monitorUptimeHistory(ctx, mon.ID)
	ms.Chart = s.monitorPublicChart(ctx, mon.ID)
	s.attachPublicCert(ctx, mon.ID, ms)

	return ms
}

// attachPublicCert fills cert_expiry_date / cert_days_left only when TLS
// cache data exists. Never invents a zero-day or empty date.
func (s *StatusPageService) attachPublicCert(ctx context.Context, monitorID int64, ms *PublicMonitorStatus) {
	if s.tlsInfo == nil {
		return
	}
	info, err := s.tlsInfo.GetByMonitorID(ctx, monitorID)
	if err != nil || info == nil {
		return
	}
	if info.NotAfter.IsZero() {
		return
	}
	expiry := info.NotAfter.UTC().Format(time.RFC3339)
	ms.CertExpiryDate = &expiry
	days := info.DaysRemaining
	ms.CertDaysLeft = &days
}

// dayCounts is a single day's heartbeat tally, from either daily aggregates or
// raw heartbeats.
type dayCounts struct {
	up, down, pending, maint, total int
}

// monitorUptimeBar builds the public uptime bar and the uptime percentage over
// the public uptime window.
//
// Days are keyed by UTC calendar date. Maintenance checks are excluded from the
// percentage — a planned window must not count against a monitor's uptime (same
// rule as AggregateService.GetUptimePercent).
//
// It reads the daily rollups first (cheap, and the rollup loop keeps them
// current) and falls back to raw heartbeats when a monitor is too young to have
// any — otherwise a brand-new monitor's bar would read as 90 empty days.
func (s *StatusPageService) monitorUptimeBar(ctx context.Context, monitorID int64) ([]PublicUptimeDay, *float64) {
	days := publicUptimeBarDays
	now := time.Now().UTC()
	from := now.Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))

	byDay := s.uptimeDayCounts(ctx, monitorID, from, now)

	bar := make([]PublicUptimeDay, 0, days)
	var totalUp, totalChecks, totalMaint int
	for i := 0; i < days; i++ {
		key := from.AddDate(0, 0, i).Format(time.DateOnly)
		c := byDay[key]
		if c != nil {
			totalUp += c.up
			totalChecks += c.total
			totalMaint += c.maint
		}
		bar = append(bar, PublicUptimeDay{Date: key, Status: dayStatus(c)})
	}

	effective := totalChecks - totalMaint
	if effective <= 0 {
		// No effective checks means there is no evidence for either 0% or 100%.
		// Return an explicit unknown so the public page never invents reliability.
		return bar, nil
	}
	percentage := (float64(totalUp) / float64(effective)) * 100.0
	return bar, &percentage
}

// dayStatus collapses a day's tally to a single bar segment, worst-first: one
// failed check makes the day red, which is the point of a status page.
func dayStatus(c *dayCounts) string {
	switch {
	case c == nil || c.total == 0:
		return "none"
	case c.down > 0:
		return "down"
	case c.pending > 0:
		return "pending"
	case c.maint > 0:
		return "maintenance"
	case c.up > 0:
		return "up"
	default:
		return "none"
	}
}

// uptimeDayCounts tallies checks per UTC calendar day over [from, now]. It reads
// the daily rollups first (cheap, and the rollup loop keeps them current) and
// falls back to raw heartbeats when a monitor is too young to have any —
// otherwise a brand-new monitor's bar would read as 90 empty days.
func (s *StatusPageService) uptimeDayCounts(ctx context.Context, monitorID int64, from, now time.Time) map[string]*dayCounts {
	byDay := make(map[string]*dayCounts)
	at := func(t time.Time) *dayCounts {
		key := t.UTC().Format(time.DateOnly)
		if d, ok := byDay[key]; ok {
			return d
		}
		d := &dayCounts{}
		byDay[key] = d
		return d
	}

	aggs, err := s.hbRepo.GetAggregate1d(ctx, monitorID, from)
	if err != nil {
		slog.Warn("status page: read daily aggregates failed", "monitor_id", monitorID, "error", err)
	}
	for _, a := range aggs {
		if a.Bucket.Before(from) {
			continue
		}
		d := at(a.Bucket)
		d.up += a.UpCount
		d.down += a.DownCount
		d.pending += a.PendingCount
		d.maint += a.MaintCount
		d.total += a.TotalChecks
	}
	if len(byDay) > 0 {
		return byDay
	}

	heartbeats, err := s.hbRepo.ListByMonitor(ctx, monitorID, from, now)
	if err != nil {
		slog.Warn("status page: read heartbeats for uptime failed", "monitor_id", monitorID, "error", err)
		return byDay
	}
	for _, hb := range heartbeats {
		d := at(hb.Time)
		d.total++
		switch hb.Status {
		case domain.StatusUp:
			d.up++
		case domain.StatusDown:
			d.down++
		case domain.StatusPending:
			d.pending++
		case domain.StatusMaintenance:
			d.maint++
		}
	}
	return byDay
}

// monitorUptimeHistory builds newest-first monthly and quarterly uptime summaries
// from the existing UTC daily rollups. A monitor without daily rollups falls
// back to raw heartbeats through uptimeDayCounts, matching the 90-day bar.
func (s *StatusPageService) monitorUptimeHistory(ctx context.Context, monitorID int64) PublicUptimeHistory {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	quarterMonth := time.Month(((int(now.Month())-1)/3)*3 + 1)
	quarterStart := time.Date(now.Year(), quarterMonth, 1, 0, 0, 0, 0, time.UTC)

	monthFrom := monthStart.AddDate(0, -(publicUptimeHistoryMonths - 1), 0)
	quarterFrom := quarterStart.AddDate(0, -3*(publicUptimeHistoryQuarters-1), 0)
	from := monthFrom
	if quarterFrom.Before(from) {
		from = quarterFrom
	}
	byDay := s.uptimeDayCounts(ctx, monitorID, from, now)

	history := PublicUptimeHistory{
		Monthly:   make([]PublicUptimePeriod, 0, publicUptimeHistoryMonths),
		Quarterly: make([]PublicUptimePeriod, 0, publicUptimeHistoryQuarters),
	}
	for i := 0; i < publicUptimeHistoryMonths; i++ {
		start := monthStart.AddDate(0, -i, 0)
		history.Monthly = append(history.Monthly, uptimePeriod(
			start.Format("January 2006"), start, start.AddDate(0, 1, -1), today, byDay,
		))
	}
	for i := 0; i < publicUptimeHistoryQuarters; i++ {
		start := quarterStart.AddDate(0, -3*i, 0)
		quarter := (int(start.Month())-1)/3 + 1
		history.Quarterly = append(history.Quarterly, uptimePeriod(
			fmt.Sprintf("Q%d %d", quarter, start.Year()), start, start.AddDate(0, 3, -1), today, byDay,
		))
	}
	return history
}

func uptimePeriod(label string, start, periodEnd, today time.Time, byDay map[string]*dayCounts) PublicUptimePeriod {
	end := periodEnd
	if end.After(today) {
		end = today
	}
	var totalUp, totalChecks, totalMaint int
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		if counts := byDay[day.Format(time.DateOnly)]; counts != nil {
			totalUp += counts.up
			totalChecks += counts.total
			totalMaint += counts.maint
		}
	}
	var percentage *float64
	if effective := totalChecks - totalMaint; effective > 0 {
		value := float64(totalUp) / float64(effective) * 100
		percentage = &value
	}
	return PublicUptimePeriod{
		Label:         label,
		StartDate:     start.Format(time.DateOnly),
		EndDate:       end.Format(time.DateOnly),
		UptimePercent: percentage,
		Complete:      periodEnd.Before(today),
	}
}

// monitorPublicChart builds bounded response-time chart data (bucketed
// min/avg/max ping plus downtime intervals) for one monitor on a public
// status page, covering the last publicChartRangeHours. It degrades
// gracefully to an empty (non-nil) chart on a query error so a single
// broken monitor never fails the whole page.
func (s *StatusPageService) monitorPublicChart(ctx context.Context, monitorID int64) *PublicMonitorChart {
	chart := &PublicMonitorChart{
		Buckets:           []PublicChartBucket{},
		DowntimeIntervals: []PublicDowntimeInterval{},
	}

	now := time.Now().UTC()
	from := now.Add(-publicChartRangeHours * time.Hour)

	heartbeats, err := s.hbRepo.ListByMonitor(ctx, monitorID, from, now)
	if err != nil {
		slog.Warn("status page service: list heartbeats failed for chart, returning empty chart", "monitor_id", monitorID, "error", err)
		return chart
	}

	// Bound the sample count so a busy monitor can't slow down a public
	// page that lists many monitors. Keep the most recent samples.
	if len(heartbeats) > publicChartHeartbeatMax {
		sort.Slice(heartbeats, func(i, j int) bool {
			return heartbeats[i].Time.Before(heartbeats[j].Time)
		})
		heartbeats = heartbeats[len(heartbeats)-publicChartHeartbeatMax:]
	}

	bucketDur := BucketDurationForRange(publicChartRangeHours)
	for _, b := range BucketHeartbeats(heartbeats, bucketDur) {
		chart.Buckets = append(chart.Buckets, PublicChartBucket{
			Time: b.Time.Format(time.RFC3339),
			Min:  b.Min,
			Avg:  b.Avg,
			Max:  b.Max,
		})
	}

	for _, iv := range DetectDowntimeIntervals(heartbeats) {
		chart.DowntimeIntervals = append(chart.DowntimeIntervals, PublicDowntimeInterval{
			Start: iv.Start.Format(time.RFC3339),
			End:   iv.End.Format(time.RFC3339),
		})
	}

	return chart
}

// GetByID retrieves a status page by its ID.
func (s *StatusPageService) GetByID(ctx context.Context, id int64) (*domain.StatusPage, error) {
	return s.repo.GetByID(ctx, id)
}

// List retrieves all status pages.
func (s *StatusPageService) List(ctx context.Context) ([]*domain.StatusPage, error) {
	return s.repo.List(ctx)
}

// Update updates a status page configuration.
func (s *StatusPageService) Update(ctx context.Context, sp *domain.StatusPage) error {
	sp.DashboardStyle = domain.NormalizeDashboardStyle(sp.DashboardStyle)
	if err := normalizeStatusPageSLATarget(sp); err != nil {
		return err
	}
	if err := validateStatusPageBranding(sp); err != nil {
		return err
	}
	return s.repo.Update(ctx, sp)
}

// validateStatusPageBranding enforces F3.5 logo/favicon rules: empty, http(s),
// same-origin path (/icon.svg), or data:image/* data-URLs under the size cap.
func validateStatusPageBranding(sp *domain.StatusPage) error {
	if err := validateBrandAsset("icon", sp.Icon); err != nil {
		return err
	}
	if err := validateBrandAsset("favicon", sp.Favicon); err != nil {
		return err
	}
	return nil
}

func validateBrandAsset(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// Same-origin path (Kuma-style /icon.svg, or /brand/phoenix-mascot.svg).
	// Reject protocol-relative //host and path traversal; anything else is
	// fetched by the browser from this Phoenix origin.
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		if strings.Contains(raw, "\\") || strings.Contains(raw, "..") {
			return fmt.Errorf("status page service: %w: %s must be a data:image/* data-URL, http(s) URL, or same-origin path", domain.ErrValidation, field)
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host != "" || u.Scheme != "" {
			return fmt.Errorf("status page service: %w: %s must be a data:image/* data-URL, http(s) URL, or same-origin path", domain.ErrValidation, field)
		}
		return nil
	}
	if strings.HasPrefix(raw, "data:") {
		// data:image/png;base64,....
		meta, payload, ok := strings.Cut(raw, ",")
		if !ok || !strings.HasPrefix(meta, "data:image/") {
			return fmt.Errorf("status page service: %w: %s must be a data:image/* data-URL, http(s) URL, or same-origin path", domain.ErrValidation, field)
		}
		if !strings.Contains(meta, ";base64") {
			return fmt.Errorf("status page service: %w: %s data-URL must be base64-encoded", domain.ErrValidation, field)
		}
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			// Some browsers emit raw base64 without padding; try RawStd.
			decoded, err = base64.RawStdEncoding.DecodeString(payload)
			if err != nil {
				return fmt.Errorf("status page service: %w: %s data-URL is not valid base64", domain.ErrValidation, field)
			}
		}
		if len(decoded) > statusPageBrandAssetMaxDecoded {
			return fmt.Errorf("status page service: %w: %s must be at most %d bytes decoded", domain.ErrValidation, field, statusPageBrandAssetMaxDecoded)
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("status page service: %w: %s must be an http(s) URL, same-origin path, or data:image data-URL", domain.ErrValidation, field)
	}
	return nil
}

func normalizeStatusPageSLATarget(sp *domain.StatusPage) error {
	if sp.SLATarget == nil {
		return nil
	}
	target := *sp.SLATarget
	if target == 0 {
		sp.SLATarget = nil
		return nil
	}
	if math.IsNaN(target) || math.IsInf(target, 0) || target < 0 || target > 100 {
		return fmt.Errorf("status page service: %w: sla_target must be greater than 0 and at most 100", domain.ErrValidation)
	}
	return nil
}

// Delete deletes a status page by its ID.
func (s *StatusPageService) Delete(ctx context.Context, id int64) error {
	return s.repo.Delete(ctx, id)
}

// ---------------------------------------------------------------------------
// Incident Management
// ---------------------------------------------------------------------------

// CreateIncident creates a new incident on a status page.
func (s *StatusPageService) CreateIncident(ctx context.Context, inc *domain.Incident) error {
	if inc.Title == "" {
		return fmt.Errorf("status page service: %w: incident title is required", domain.ErrValidation)
	}
	if inc.Style == "" {
		inc.Style = "warning"
	}
	if err := s.incidentRepo.Create(ctx, inc); err != nil {
		return err
	}
	if s.incidentUpdates != nil {
		update := &domain.IncidentUpdate{
			IncidentID:   inc.ID,
			StatusPageID: inc.StatusPageID,
			Status:       domain.IncidentStatusInvestigating,
			Content:      inc.Content,
		}
		if err := s.incidentUpdates.Create(ctx, update); err != nil {
			return fmt.Errorf("status page service: create initial incident update: %w", err)
		}
	}
	if s.incidentMail != nil && inc.Active {
		if err := s.incidentMail.NotifyIncidentCreated(ctx, inc); err != nil {
			slog.Error("status page service: incident-created mail failed",
				"incident_id", inc.ID, "error", err)
		}
	}
	return nil
}

// GetIncidentByID retrieves an incident by its ID.
func (s *StatusPageService) GetIncidentByID(ctx context.Context, id int64) (*domain.Incident, error) {
	return s.incidentRepo.GetByID(ctx, id)
}

// ListIncidents retrieves all incidents for a status page, ordered by
// created_at descending (most recent first).
func (s *StatusPageService) ListIncidents(ctx context.Context, statusPageID int64) ([]*domain.Incident, error) {
	return s.incidentRepo.ListByStatusPage(ctx, statusPageID)
}

// ListAllIncidents returns every incident across all status pages.
func (s *StatusPageService) ListAllIncidents(ctx context.Context) ([]*domain.Incident, error) {
	return s.incidentRepo.ListAll(ctx)
}

// ListIncidentUpdates retrieves timeline updates for one incident, oldest first.
func (s *StatusPageService) ListIncidentUpdates(
	ctx context.Context,
	incidentID int64,
) ([]*domain.IncidentUpdate, error) {
	if s.incidentUpdates == nil {
		return []*domain.IncidentUpdate{}, nil
	}
	updates, err := s.incidentUpdates.ListByIncident(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("status page service: list incident updates: %w", err)
	}
	return updates, nil
}

// CreateIncidentUpdate appends a markdown timeline update to an incident. Status
// progression is monotonic: investigating → identified → monitoring → resolved.
func (s *StatusPageService) CreateIncidentUpdate(
	ctx context.Context,
	incidentID int64,
	status domain.IncidentStatus,
	content string,
) (*domain.IncidentUpdate, error) {
	if s.incidentUpdates == nil {
		return nil, fmt.Errorf("status page service: %w: incident update repository is not configured", domain.ErrInternal)
	}
	inc, err := s.incidentRepo.GetByID(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("status page service: create incident update: %w", err)
	}
	status = domain.NormalizeIncidentStatus(string(status))
	if status == "" {
		status = domain.IncidentStatusInvestigating
	}
	if !domain.ValidIncidentStatus(status) {
		return nil, fmt.Errorf("status page service: %w: invalid incident update status", domain.ErrValidation)
	}
	updates, err := s.incidentUpdates.ListByIncident(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("status page service: read incident updates: %w", err)
	}
	if len(updates) > 0 {
		previous := updates[len(updates)-1].Status
		if !domain.IncidentStatusProgresses(previous, status) {
			return nil, fmt.Errorf("status page service: %w: incident status cannot move backward", domain.ErrValidation)
		}
	}
	if !inc.Active && status != domain.IncidentStatusResolved {
		return nil, fmt.Errorf("status page service: %w: resolved incidents only accept resolved updates", domain.ErrValidation)
	}

	update := &domain.IncidentUpdate{
		IncidentID:   inc.ID,
		StatusPageID: inc.StatusPageID,
		Status:       status,
		Content:      strings.TrimSpace(content),
	}
	if err := s.incidentUpdates.Create(ctx, update); err != nil {
		return nil, fmt.Errorf("status page service: create incident update: %w", err)
	}
	if status == domain.IncidentStatusResolved && inc.Active {
		inc.Active = false
		if err := s.incidentRepo.Update(ctx, inc); err != nil {
			return nil, fmt.Errorf("status page service: resolve incident from update: %w", err)
		}
	}
	if s.incidentMail != nil {
		if err := s.incidentMail.NotifyIncidentUpdated(ctx, inc, update); err != nil {
			slog.Error("status page service: incident-update mail failed",
				"incident_id", inc.ID, "incident_update_id", update.ID, "error", err)
		}
	}
	return update, nil
}

// UpdateIncident updates an incident's fields (title, content, style, etc.).
// An active→inactive transition appends a resolved timeline update and triggers
// resolve mail (best-effort).
func (s *StatusPageService) UpdateIncident(ctx context.Context, inc *domain.Incident) error {
	var wasActive bool
	if prev, err := s.incidentRepo.GetByID(ctx, inc.ID); err == nil && prev != nil {
		wasActive = prev.Active
	}
	if err := s.incidentRepo.Update(ctx, inc); err != nil {
		return err
	}
	if wasActive && !inc.Active {
		if s.incidentUpdates != nil {
			update := &domain.IncidentUpdate{
				IncidentID:   inc.ID,
				StatusPageID: inc.StatusPageID,
				Status:       domain.IncidentStatusResolved,
				Content:      "Resolved.",
			}
			if err := s.incidentUpdates.Create(ctx, update); err != nil {
				return fmt.Errorf("status page service: create resolved incident update: %w", err)
			}
		}
		if s.incidentMail != nil {
			if err := s.incidentMail.NotifyIncidentResolved(ctx, inc); err != nil {
				slog.Error("status page service: incident-resolved mail failed",
					"incident_id", inc.ID, "error", err)
			}
		}
	}
	return nil
}

// DeleteIncident deletes a resolved incident by its ID. Active incidents must
// be resolved first so an ongoing outage cannot disappear without a resolution
// record.
func (s *StatusPageService) DeleteIncident(ctx context.Context, id int64) error {
	inc, err := s.incidentRepo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("status page service: get incident for deletion: %w", err)
	}
	if inc.Active {
		return fmt.Errorf("status page service: delete incident: %w", domain.ErrIncidentActive)
	}
	if err := s.incidentRepo.Delete(ctx, id); err != nil {
		return fmt.Errorf("status page service: delete incident: %w", err)
	}
	return nil
}

// ResolveIncident marks an incident as inactive (resolved). It sets Active=false
// on the incident so it no longer shows as ongoing on the status page.
func (s *StatusPageService) ResolveIncident(ctx context.Context, id int64) error {
	inc, err := s.incidentRepo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("status page service: resolve incident: %w", err)
	}
	if !inc.Active {
		return nil
	}
	inc.Active = false
	if err := s.incidentRepo.Update(ctx, inc); err != nil {
		return err
	}
	if s.incidentUpdates != nil {
		update := &domain.IncidentUpdate{
			IncidentID:   inc.ID,
			StatusPageID: inc.StatusPageID,
			Status:       domain.IncidentStatusResolved,
			Content:      "Resolved.",
		}
		if err := s.incidentUpdates.Create(ctx, update); err != nil {
			return fmt.Errorf("status page service: create resolved incident update: %w", err)
		}
	}
	if s.incidentMail != nil {
		if err := s.incidentMail.NotifyIncidentResolved(ctx, inc); err != nil {
			slog.Error("status page service: incident-resolved mail failed",
				"incident_id", inc.ID, "error", err)
		}
	}
	return nil
}

// AutoResolveOnRecovery resolves active incidents on status pages that include
// monitorID and have AutoResolveIncidents enabled. Called from the notification
// dispatcher when a monitor recovers (DOWN→UP). Returns nil even when no pages
// match — a missing assignment is not an error.
func (s *StatusPageService) AutoResolveOnRecovery(ctx context.Context, monitorID int64) error {
	pages, err := s.repo.List(ctx)
	if err != nil {
		return fmt.Errorf("status page service: auto-resolve: list pages: %w", err)
	}
	for _, sp := range pages {
		if !sp.AutoResolveIncidents {
			continue
		}
		links, err := s.spMonitorRepo.ListByStatusPage(ctx, sp.ID)
		if err != nil {
			slog.Error("status page service: auto-resolve: list monitors",
				"status_page_id", sp.ID, "error", err)
			continue
		}
		linked := false
		for _, l := range links {
			if l.MonitorID == monitorID {
				linked = true
				break
			}
		}
		if !linked {
			continue
		}
		incidents, err := s.incidentRepo.ListByStatusPage(ctx, sp.ID)
		if err != nil {
			slog.Error("status page service: auto-resolve: list incidents",
				"status_page_id", sp.ID, "error", err)
			continue
		}
		for _, inc := range incidents {
			if !inc.Active {
				continue
			}
			inc.Active = false
			if err := s.incidentRepo.Update(ctx, inc); err != nil {
				slog.Error("status page service: auto-resolve: update incident",
					"incident_id", inc.ID, "error", err)
				continue
			}
			if s.incidentUpdates != nil {
				update := &domain.IncidentUpdate{
					IncidentID:   inc.ID,
					StatusPageID: inc.StatusPageID,
					Status:       domain.IncidentStatusResolved,
					Content:      "Automatically resolved after monitor recovery.",
				}
				if err := s.incidentUpdates.Create(ctx, update); err != nil {
					slog.Error("status page service: auto-resolve: create incident update",
						"incident_id", inc.ID, "error", err)
				}
			}
			if s.incidentMail != nil {
				if err := s.incidentMail.NotifyIncidentResolved(ctx, inc); err != nil {
					slog.Error("status page service: auto-resolve mail failed",
						"incident_id", inc.ID, "error", err)
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Status Page Monitor Assignments
// ---------------------------------------------------------------------------

// AddMonitor assigns a monitor to a status page with the given display order.
// If the monitor is already linked, it returns ports.ErrMonitorAlreadyLinked
// instead of the generic ports.ErrConflict, giving the caller a clear signal
// for a user-facing message.
func (s *StatusPageService) AddMonitor(ctx context.Context, spID, monitorID int64, displayOrder int) error {
	if displayOrder <= 0 {
		displayOrder = 1000
	}
	err := s.spMonitorRepo.AddMonitor(ctx, spID, monitorID, displayOrder)
	if errors.Is(err, ports.ErrConflict) {
		return ports.ErrMonitorAlreadyLinked
	}
	return err
}

// RemoveMonitor removes a monitor from a status page.
func (s *StatusPageService) RemoveMonitor(ctx context.Context, spID, monitorID int64) error {
	return s.spMonitorRepo.RemoveMonitor(ctx, spID, monitorID)
}

// ListMonitors retrieves all monitors assigned to a status page, ordered by
// display_order ascending.
func (s *StatusPageService) ListMonitors(ctx context.Context, statusPageID int64) ([]*domain.StatusPageMonitor, error) {
	return s.spMonitorRepo.ListByStatusPage(ctx, statusPageID)
}

// ReorderMonitors replaces the display_order of every monitor on a status page
// in one transaction. monitorIDs is ordered: index 0 → display_order 10,
// index 1 → display_order 20, etc. Any monitor currently assigned but absent
// from the list is removed. This is the only safe way to reorder — the
// alternative (remove + re-add) drops the row between the two calls, which
// can lose the assignment on a network error.
func (s *StatusPageService) ReorderMonitors(ctx context.Context, spID int64, monitorIDs []int64) error {
	return s.spMonitorRepo.ReorderMonitors(ctx, spID, monitorIDs)
}

// ---------------------------------------------------------------------------
// Custom Domain (CNAME) Management
// ---------------------------------------------------------------------------

// AddCNAME registers a custom domain alias for a status page.
func (s *StatusPageService) AddCNAME(ctx context.Context, cname *domain.StatusPageCNAME) error {
	return s.cnameRepo.Create(ctx, cname)
}

// RemoveCNAME removes a custom domain alias by its ID.
func (s *StatusPageService) RemoveCNAME(ctx context.Context, id int64) error {
	return s.cnameRepo.Delete(ctx, id)
}

// ListCNAMEs retrieves all custom domain aliases for a status page.
func (s *StatusPageService) ListCNAMEs(ctx context.Context, statusPageID int64) ([]*domain.StatusPageCNAME, error) {
	return s.cnameRepo.ListByStatusPage(ctx, statusPageID)
}

// ResolveDomain looks up a status page by its custom domain. Returns the
// status page if a matching CNAME record is found.
func (s *StatusPageService) ResolveDomain(ctx context.Context, domain string) (*domain.StatusPage, error) {
	cname, err := s.cnameRepo.GetByDomain(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("status page service: resolve domain: %w", err)
	}
	return s.repo.GetByID(ctx, cname.StatusPageID)
}

// SetPassword validates and hashes a raw status-page access code through the
// configured password port. An empty value explicitly removes protection.
func (s *StatusPageService) SetPassword(password string) (string, error) {
	if password == "" {
		return "", nil
	}
	if utf8.RuneCountInString(password) < StatusPageAccessCodeMinLength {
		return "", fmt.Errorf(
			"status page service: %w: access code must be at least %d characters",
			domain.ErrValidation,
			StatusPageAccessCodeMinLength,
		)
	}
	if len([]byte(password)) > StatusPageAccessCodeMaxBytes {
		return "", fmt.Errorf(
			"status page service: %w: access code must be at most %d UTF-8 bytes",
			domain.ErrValidation,
			StatusPageAccessCodeMaxBytes,
		)
	}
	if s.passwords == nil {
		return "", fmt.Errorf("status page service: %w: password hasher is not configured", domain.ErrInternal)
	}
	hash, err := s.passwords.Hash(password)
	if err != nil {
		return "", fmt.Errorf("status page service: hash password: %w", err)
	}
	return hash, nil
}
