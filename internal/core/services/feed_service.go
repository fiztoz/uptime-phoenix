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

// recentResolvedIncidentDays bounds how far back resolved incidents appear in
// the public Atom feed. Active incidents always appear regardless of age.
const recentResolvedIncidentDays = 90

// FeedService builds public status-page syndication feeds (Atom incidents and
// iCal maintenance calendars). It never returns domain types with secrets —
// only plain feed DTOs safe for unauthenticated responses.
type FeedService struct {
	pages       ports.StatusPageRepository
	incidents   ports.IncidentRepository
	spMonitors  ports.StatusPageMonitorRepository
	maintenance *MaintenanceService
	passwords   ports.PasswordHasher
	// baseURL is the optional absolute PUBLIC_URL used to mint feed self and
	// alternate links. Empty yields path-only or relative-safe identifiers.
	baseURL string
}

// NewFeedService creates a FeedService. maintenance may be nil (calendar then
// returns zero events after access is verified). passwords may be nil only when
// no protected pages will be requested.
func NewFeedService(
	pages ports.StatusPageRepository,
	incidents ports.IncidentRepository,
	spMonitors ports.StatusPageMonitorRepository,
	maintenance *MaintenanceService,
	passwords ports.PasswordHasher,
	baseURL string,
) *FeedService {
	return &FeedService{
		pages:       pages,
		incidents:   incidents,
		spMonitors:  spMonitors,
		maintenance: maintenance,
		passwords:   passwords,
		baseURL:     strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
}

// IncidentFeed is the service-layer DTO for an Atom/RSS incidents feed.
// It carries no PasswordHash and no domain types.
type IncidentFeed struct {
	Slug        string
	Title       string
	Description string
	SelfLink    string
	PageLink    string
	Updated     time.Time
	Entries     []IncidentFeedEntry
}

// IncidentFeedEntry is one incident in the public feed.
type IncidentFeedEntry struct {
	ID        int64
	Title     string
	Content   string
	Style     string
	Active    bool
	CreatedAt time.Time
	Link      string
}

// CalendarFeed is the service-layer DTO for a public maintenance iCal feed.
type CalendarFeed struct {
	Slug   string
	Title  string
	Events []CalendarEvent
}

// CalendarEvent is one maintenance window as a calendar VEVENT payload.
type CalendarEvent struct {
	UID         string
	Summary     string
	Description string
	Start       time.Time
	End         time.Time
}

// IncidentFeed builds the public incidents feed for a published status page.
//
// Access control (fail closed):
//   - unknown or unpublished slug → domain.ErrNotFound
//   - protected page with missing/wrong access code → domain.ErrUnauthorized
//   - unprotected page accepts empty access code
//
// Entries include every active incident plus resolved incidents created within
// recentResolvedIncidentDays. PasswordHash never appears in the result.
func (s *FeedService) IncidentFeed(ctx context.Context, slug, accessCode string) (*IncidentFeed, error) {
	sp, err := s.loadPublishedPage(ctx, slug)
	if err != nil {
		return nil, err
	}
	if err := s.requireAccess(sp, accessCode); err != nil {
		return nil, err
	}

	incidents, err := s.incidents.ListByStatusPage(ctx, sp.ID)
	if err != nil {
		return nil, fmt.Errorf("incident feed: list incidents: %w", err)
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -recentResolvedIncidentDays)
	entries := make([]IncidentFeedEntry, 0, len(incidents))
	var updated time.Time
	for _, inc := range incidents {
		if inc == nil {
			continue
		}
		if !inc.Active {
			created := inc.CreatedAt.UTC()
			if !created.IsZero() && created.Before(cutoff) {
				continue
			}
		}
		created := inc.CreatedAt.UTC()
		if created.After(updated) {
			updated = created
		}
		entries = append(entries, IncidentFeedEntry{
			ID:        inc.ID,
			Title:     inc.Title,
			Content:   inc.Content,
			Style:     inc.Style,
			Active:    inc.Active,
			CreatedAt: created,
			Link:      s.pageLink(sp.Slug),
		})
	}

	// Active first, then newest created_at, then id desc as a deterministic
	// tie-break (second-precision timestamps can collide).
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Active != entries[j].Active {
			return entries[i].Active
		}
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		}
		return entries[i].ID > entries[j].ID
	})

	if updated.IsZero() {
		updated = sp.UpdatedAt.UTC()
	}
	if updated.IsZero() {
		updated = time.Now().UTC()
	}

	return &IncidentFeed{
		Slug:        sp.Slug,
		Title:       sp.Title,
		Description: sp.Description,
		SelfLink:    s.feedLink(sp.Slug, "feed.xml"),
		PageLink:    s.pageLink(sp.Slug),
		Updated:     updated,
		Entries:     entries,
	}, nil
}

// CalendarFeed builds the public maintenance calendar for a published status
// page. Events cover maintenance windows linked to any monitor assigned to the
// page. Access control matches IncidentFeed (fail closed).
func (s *FeedService) CalendarFeed(ctx context.Context, slug, accessCode string) (*CalendarFeed, error) {
	sp, err := s.loadPublishedPage(ctx, slug)
	if err != nil {
		return nil, err
	}
	if err := s.requireAccess(sp, accessCode); err != nil {
		return nil, err
	}

	feed := &CalendarFeed{
		Slug:   sp.Slug,
		Title:  sp.Title,
		Events: []CalendarEvent{},
	}
	if s.maintenance == nil || s.spMonitors == nil {
		return feed, nil
	}

	links, err := s.spMonitors.ListByStatusPage(ctx, sp.ID)
	if err != nil {
		return nil, fmt.Errorf("calendar feed: list status page monitors: %w", err)
	}
	monitorIDs := make([]int64, 0, len(links))
	for _, l := range links {
		if l != nil {
			monitorIDs = append(monitorIDs, l.MonitorID)
		}
	}

	windows, err := s.maintenance.ListForMonitors(ctx, monitorIDs)
	if err != nil {
		return nil, fmt.Errorf("calendar feed: list maintenance: %w", err)
	}

	events := make([]CalendarEvent, 0, len(windows))
	for _, mw := range windows {
		if mw == nil || !mw.Active {
			continue
		}
		start := mw.StartDate.UTC()
		end := mw.EndDate.UTC()
		// Cron windows without absolute bounds still need a VEVENT envelope so
		// calendar clients can list the scheduled work; use a zero-length stamp
		// only when both bounds are missing (should not happen for single).
		if start.IsZero() && end.IsZero() {
			continue
		}
		if end.IsZero() {
			// Duration is minutes for cron strategy.
			if mw.Duration > 0 {
				end = start.Add(time.Duration(mw.Duration) * time.Minute)
			} else {
				end = start.Add(time.Hour)
			}
		}
		if start.IsZero() {
			start = end.Add(-time.Hour)
		}
		desc := mw.Description
		if mw.Strategy == "cron" && mw.CronExpr != "" {
			if desc != "" {
				desc += "\n"
			}
			desc += "Schedule: " + mw.CronExpr
			if mw.Timezone != "" {
				desc += " (" + mw.Timezone + ")"
			}
		}
		events = append(events, CalendarEvent{
			UID:         fmt.Sprintf("phoenix-maint-%d@%s", mw.ID, sp.Slug),
			Summary:     mw.Title,
			Description: desc,
			Start:       start,
			End:         end,
		})
	}

	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].Start.Equal(events[j].Start) {
			return events[i].Start.Before(events[j].Start)
		}
		return events[i].UID < events[j].UID
	})
	feed.Events = events
	return feed, nil
}

func (s *FeedService) loadPublishedPage(ctx context.Context, slug string) (*domain.StatusPage, error) {
	if s.pages == nil {
		return nil, fmt.Errorf("feed: %w", domain.ErrInternal)
	}
	sp, err := s.pages.GetBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("feed: %w", err)
	}
	if !sp.Published {
		return nil, fmt.Errorf("feed: %w", domain.ErrNotFound)
	}
	return sp, nil
}

// requireAccess enforces password protection fail-closed. Unprotected pages
// always succeed. Protected pages require a matching access code.
func (s *FeedService) requireAccess(sp *domain.StatusPage, accessCode string) error {
	if sp.PasswordHash == "" {
		return nil
	}
	if accessCode == "" {
		return fmt.Errorf("feed access: %w", domain.ErrUnauthorized)
	}
	if s.passwords == nil {
		return fmt.Errorf("feed access: %w: password hasher is not configured", domain.ErrInternal)
	}
	if err := s.passwords.Verify(sp.PasswordHash, accessCode); err != nil {
		return fmt.Errorf("feed access: %w", domain.ErrUnauthorized)
	}
	return nil
}

func (s *FeedService) pageLink(slug string) string {
	path := "/" + slug
	if s.baseURL == "" {
		return path
	}
	return s.baseURL + path
}

func (s *FeedService) feedLink(slug, name string) string {
	path := "/api/status/" + slug + "/" + name
	if s.baseURL == "" {
		return path
	}
	return s.baseURL + path
}
