package handlers

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// FeedHandlers serves public status-page Atom and iCal feeds.
//
// Access control matches other protected public status endpoints but fails
// closed: a password-protected page without a valid access code returns 403
// (never an empty feed that looks healthy). Access codes are accepted via
// query param `access_code` or header `X-Access-Code` so feed readers and
// calendar clients can subscribe without a POST body.
//
// Wire shapes are pure syndication markup — domain types and PasswordHash
// never leave this handler.
type FeedHandlers struct {
	svc *services.FeedService
}

// NewFeedHandlers binds handlers to a FeedService.
func NewFeedHandlers(svc *services.FeedService) *FeedHandlers {
	return &FeedHandlers{svc: svc}
}

// Atom handles GET /api/status/:slug/feed.xml — Atom 1.0 incidents feed.
func (h *FeedHandlers) Atom(c echo.Context) error {
	if h.svc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("feeds unavailable"))
	}
	slug := c.Param("slug")
	if slug == "" {
		return badRequest(c, "slug is required")
	}

	feed, err := h.svc.IncidentFeed(c.Request().Context(), slug, accessCodeFromRequest(c))
	if err != nil {
		return mapFeedError(c, err)
	}

	body, err := renderAtom(feed)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
	return c.Blob(http.StatusOK, "application/atom+xml; charset=utf-8", body)
}

// Calendar handles GET /api/status/:slug/calendar.ics — iCal maintenance feed.
func (h *FeedHandlers) Calendar(c echo.Context) error {
	if h.svc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("feeds unavailable"))
	}
	slug := c.Param("slug")
	if slug == "" {
		return badRequest(c, "slug is required")
	}

	cal, err := h.svc.CalendarFeed(c.Request().Context(), slug, accessCodeFromRequest(c))
	if err != nil {
		return mapFeedError(c, err)
	}

	body := renderICal(cal)
	return c.Blob(http.StatusOK, "text/calendar; charset=utf-8", []byte(body))
}

// accessCodeFromRequest reads the page access code from the query string or
// X-Access-Code header. Never logs the value.
func accessCodeFromRequest(c echo.Context) string {
	if v := strings.TrimSpace(c.QueryParam("access_code")); v != "" {
		return v
	}
	return strings.TrimSpace(c.Request().Header.Get("X-Access-Code"))
}

func mapFeedError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody("not found"))
	case errors.Is(err, domain.ErrUnauthorized):
		// Match public status-page verify-access: 403 "access denied".
		return c.JSON(http.StatusForbidden, errorBody("access denied"))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}

// ---------------------------------------------------------------------------
// Atom 1.0 (stdlib encoding/xml)
// ---------------------------------------------------------------------------

type atomFeed struct {
	XMLName  xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Title    string      `xml:"title"`
	ID       string      `xml:"id"`
	Updated  string      `xml:"updated"`
	Links    []atomLink  `xml:"link"`
	Subtitle string      `xml:"subtitle,omitempty"`
	Entries  []atomEntry `xml:"entry"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr,omitempty"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr,omitempty"`
}

type atomEntry struct {
	Title      string         `xml:"title"`
	ID         string         `xml:"id"`
	Updated    string         `xml:"updated"`
	Published  string         `xml:"published"`
	Summary    string         `xml:"summary,omitempty"`
	Content    atomContent    `xml:"content"`
	Links      []atomLink     `xml:"link"`
	Categories []atomCategory `xml:"category,omitempty"`
}

type atomContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

type atomCategory struct {
	Term string `xml:"term,attr"`
}

func renderAtom(feed *services.IncidentFeed) ([]byte, error) {
	if feed == nil {
		return nil, fmt.Errorf("nil feed")
	}
	af := atomFeed{
		Title:    feed.Title + " — Incidents",
		ID:       atomID(feed.Slug, "feed"),
		Updated:  feed.Updated.UTC().Format(time.RFC3339),
		Subtitle: feed.Description,
		Links: []atomLink{
			{Rel: "self", Href: feed.SelfLink, Type: "application/atom+xml"},
			{Rel: "alternate", Href: feed.PageLink, Type: "text/html"},
		},
		Entries: make([]atomEntry, 0, len(feed.Entries)),
	}
	for _, e := range feed.Entries {
		status := "resolved"
		if e.Active {
			status = "active"
		}
		summary := e.Content
		if summary == "" {
			if e.Active {
				summary = "Active incident"
			} else {
				summary = "Resolved incident"
			}
		}
		cats := []atomCategory{{Term: status}}
		if e.Style != "" {
			cats = append(cats, atomCategory{Term: e.Style})
		}
		af.Entries = append(af.Entries, atomEntry{
			Title:     e.Title,
			ID:        atomID(feed.Slug, "incident-"+strconv.FormatInt(e.ID, 10)),
			Updated:   e.CreatedAt.UTC().Format(time.RFC3339),
			Published: e.CreatedAt.UTC().Format(time.RFC3339),
			Summary:   summary,
			Content: atomContent{
				Type: "text",
				Body: e.Content,
			},
			Links: []atomLink{
				{Rel: "alternate", Href: e.Link, Type: "text/html"},
			},
			Categories: cats,
		})
	}

	out, err := xml.MarshalIndent(af, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

func atomID(slug, suffix string) string {
	return "tag:phoenix,status:" + slug + ":" + suffix
}

// ---------------------------------------------------------------------------
// iCalendar (RFC 5545) — pure string builder, no deps
// ---------------------------------------------------------------------------

func renderICal(cal *services.CalendarFeed) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//Phoenix//Status Page//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("METHOD:PUBLISH\r\n")
	if cal != nil && cal.Title != "" {
		writeICalLine(&b, "X-WR-CALNAME", cal.Title+" — Maintenance")
	}
	stamp := time.Now().UTC()
	if cal != nil {
		for _, ev := range cal.Events {
			b.WriteString("BEGIN:VEVENT\r\n")
			writeICalLine(&b, "UID", ev.UID)
			writeICalLine(&b, "DTSTAMP", formatICalUTC(stamp))
			writeICalLine(&b, "DTSTART", formatICalUTC(ev.Start))
			writeICalLine(&b, "DTEND", formatICalUTC(ev.End))
			writeICalLine(&b, "SUMMARY", ev.Summary)
			if ev.Description != "" {
				writeICalLine(&b, "DESCRIPTION", ev.Description)
			}
			b.WriteString("END:VEVENT\r\n")
		}
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

func formatICalUTC(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// writeICalLine writes a property line with RFC 5545 text escaping and folding.
func writeICalLine(b *strings.Builder, name, value string) {
	escaped := icalEscape(value)
	line := name + ":" + escaped
	// Fold at 75 octets (RFC 5545 §3.1). Cut on UTF-8 rune boundaries.
	for len(line) > 75 {
		cut := 75
		for cut > 0 && !utf8RuneStart(line[cut]) {
			cut--
		}
		if cut == 0 {
			cut = 75
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]
	}
	b.WriteString(line)
	b.WriteString("\r\n")
}

func utf8RuneStart(b byte) bool {
	// Continuation bytes are 10xxxxxx; a start is anything else.
	return b&0xC0 != 0x80
}

func icalEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, "\r\n", "\\n")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\n")
	return s
}
