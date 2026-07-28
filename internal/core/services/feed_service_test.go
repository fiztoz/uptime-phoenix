package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestFeedService_IncidentFeed_UnknownSlugNotFound(t *testing.T) {
	t.Parallel()
	svc := NewFeedService(newFakeSPRepo(), newFakeIncidentRepo(), newFakeSPMonitorRepo(), nil, fakeStatusPagePasswordHasher{}, "")
	_, err := svc.IncidentFeed(context.Background(), "missing", "")
	if !errors.Is(err, ports.ErrNotFound) && !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestFeedService_IncidentFeed_UnpublishedNotFound(t *testing.T) {
	t.Parallel()
	spRepo := newFakeSPRepo()
	_ = spRepo.Create(context.Background(), &domain.StatusPage{
		Slug: "draft", Title: "Draft", Published: false,
	})
	svc := NewFeedService(spRepo, newFakeIncidentRepo(), newFakeSPMonitorRepo(), nil, nil, "")
	_, err := svc.IncidentFeed(context.Background(), "draft", "")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want domain.ErrNotFound", err)
	}
}

func TestFeedService_IncidentFeed_ProtectedRequiresAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	hasher := fakeStatusPagePasswordHasher{}
	hash, err := hasher.Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	sp := &domain.StatusPage{
		Slug: "private", Title: "Private", Published: true, PasswordHash: hash,
	}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := incRepo.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID, Title: "Secret outage", Active: true, Content: "details",
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewFeedService(spRepo, incRepo, newFakeSPMonitorRepo(), nil, hasher, "https://status.example")

	if _, err := svc.IncidentFeed(ctx, "private", ""); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("no code error = %v, want ErrUnauthorized", err)
	}
	if _, err := svc.IncidentFeed(ctx, "private", "wrong"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong code error = %v, want ErrUnauthorized", err)
	}

	feed, err := svc.IncidentFeed(ctx, "private", "correct horse")
	if err != nil {
		t.Fatalf("valid access: %v", err)
	}
	if len(feed.Entries) != 1 || feed.Entries[0].Title != "Secret outage" {
		t.Fatalf("entries = %+v, want secret outage", feed.Entries)
	}
	// Never surface password material in feed DTOs.
	if strings.Contains(feed.Title, "hashed:") || strings.Contains(feed.Description, hash) {
		t.Fatalf("feed leaked hash material: %+v", feed)
	}
}

func TestFeedService_IncidentFeed_IncludesActiveTitle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	sp := &domain.StatusPage{Slug: "acme", Title: "Acme Status", Published: true}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := incRepo.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "API elevated errors",
		Content:      "We are investigating.",
		Style:        "danger",
		Active:       true,
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// Old resolved incident should be filtered out.
	if err := incRepo.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "Ancient resolved",
		Active:       false,
		CreatedAt:    time.Now().UTC().AddDate(0, 0, -(recentResolvedIncidentDays + 10)),
	}); err != nil {
		t.Fatal(err)
	}
	// Recent resolved stays.
	if err := incRepo.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "Recent blip",
		Active:       false,
		CreatedAt:    time.Now().UTC().AddDate(0, 0, -3),
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewFeedService(spRepo, incRepo, newFakeSPMonitorRepo(), nil, nil, "https://status.example")
	feed, err := svc.IncidentFeed(ctx, "acme", "")
	if err != nil {
		t.Fatal(err)
	}
	if feed.Title != "Acme Status" {
		t.Fatalf("title = %q", feed.Title)
	}
	if feed.SelfLink != "https://status.example/api/status/acme/feed.xml" {
		t.Fatalf("self link = %q", feed.SelfLink)
	}
	titles := map[string]bool{}
	for _, e := range feed.Entries {
		titles[e.Title] = true
	}
	if !titles["API elevated errors"] {
		t.Fatalf("missing active incident title; got %v", titles)
	}
	if !titles["Recent blip"] {
		t.Fatalf("missing recent resolved; got %v", titles)
	}
	if titles["Ancient resolved"] {
		t.Fatalf("ancient resolved should be excluded; got %v", titles)
	}
	// Active first.
	if len(feed.Entries) < 1 || !feed.Entries[0].Active || feed.Entries[0].Title != "API elevated errors" {
		t.Fatalf("first entry should be active: %+v", feed.Entries)
	}
}

func TestFeedService_CalendarFeed_IncludesMaintenanceVEVENTSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spRepo := newFakeSPRepo()
	spMon := newFakeSPMonitorRepo()
	sp := &domain.StatusPage{Slug: "acme", Title: "Acme Status", Published: true}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := spMon.AddMonitor(ctx, sp.ID, 42, 10); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	mw := &domain.MaintenanceWindow{
		ID:          7,
		Title:       "Database failover",
		Description: "Planned primary failover",
		Active:      true,
		Strategy:    "single",
		StartDate:   start,
		EndDate:     end,
		Timezone:    "UTC",
	}
	maintRepo := &fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{7: mw}}
	linkRepo := &fakeMaintLinkRepo{byMonitor: map[int64][]*domain.MaintenanceWindow{42: {mw}}}
	maintSvc := NewMaintenanceService(maintRepo, linkRepo, &fakeCronEval{})

	svc := NewFeedService(spRepo, newFakeIncidentRepo(), spMon, maintSvc, nil, "")
	cal, err := svc.CalendarFeed(ctx, "acme", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(cal.Events), cal.Events)
	}
	ev := cal.Events[0]
	if ev.Summary != "Database failover" {
		t.Fatalf("summary = %q", ev.Summary)
	}
	if !ev.Start.Equal(start) || !ev.End.Equal(end) {
		t.Fatalf("bounds = %v–%v, want %v–%v", ev.Start, ev.End, start, end)
	}
	if !strings.Contains(ev.UID, "7") {
		t.Fatalf("uid = %q, want to include maintenance id", ev.UID)
	}
}

func TestFeedService_CalendarFeed_ProtectedWithoutAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spRepo := newFakeSPRepo()
	hasher := fakeStatusPagePasswordHasher{}
	hash, _ := hasher.Hash("secretcode")
	_ = spRepo.Create(ctx, &domain.StatusPage{
		Slug: "locked", Title: "Locked", Published: true, PasswordHash: hash,
	})
	svc := NewFeedService(spRepo, newFakeIncidentRepo(), newFakeSPMonitorRepo(), nil, hasher, "")
	if _, err := svc.CalendarFeed(ctx, "locked", ""); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}
