package handlers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// BadgeHandlers serves shields.io-style SVG badges (status/uptime/ping) for
// a single monitor, embeddable in external READMEs the way Uptime Kuma's
// badge feature is. These are PUBLIC endpoints — see router.go — so anyone
// who knows (or guesses) a monitor id can read that monitor's live status,
// uptime percentage, and latest ping via the badge image. This is
// intentional (it mirrors Uptime Kuma), but it does mean badges leak a
// monitor's health to unauthenticated callers by design.
//
// Unlike the authenticated JSON handlers, an unknown or invalid monitor id
// does NOT return 404. It renders a gray "unknown" badge with HTTP 200
// instead, because badges are embedded as <img> tags in rendered Markdown —
// a 404 would show a broken-image icon, whereas a gray "unknown" badge
// degrades gracefully.
type BadgeHandlers struct {
	monitors   ports.MonitorRepository
	heartbeats ports.HeartbeatRepository
	aggregate  *services.AggregateService
}

// NewBadgeHandlers creates handlers for the public badge endpoints.
func NewBadgeHandlers(
	monitors ports.MonitorRepository,
	heartbeats ports.HeartbeatRepository,
	aggregate *services.AggregateService,
) *BadgeHandlers {
	return &BadgeHandlers{
		monitors:   monitors,
		heartbeats: heartbeats,
		aggregate:  aggregate,
	}
}

// Badge colors, chosen to match the shields.io default palette so embedded
// badges look familiar.
const (
	badgeColorGreen = "#4c1"    // brightgreen — up / healthy uptime
	badgeColorAmber = "#dfb317" // yellow — pending / degraded uptime
	badgeColorRed   = "#e05d44" // red — down / poor uptime
	badgeColorBlue  = "#007ec6" // blue — maintenance
	badgeColorGray  = "#9f9f9f" // lightgrey — unknown / no data
	badgeLabelColor = "#555"    // dark gray label box (shields default)
)

// Status handles GET /api/badge/:id/status.svg.
// Renders "status: UP|DOWN|PENDING|MAINTENANCE", colored by state.
// A monitor with no heartbeat yet renders "PENDING" (amber).
func (h *BadgeHandlers) Status(c echo.Context) error {
	ctx := c.Request().Context()

	monitorID, ok := parseBadgeMonitorID(c)
	if !ok {
		return h.renderUnknown(c, "status")
	}

	if _, err := h.monitors.GetByID(ctx, monitorID); err != nil {
		return h.renderUnknown(c, "status")
	}

	status, err := h.latestStatus(ctx, monitorID)
	if err != nil {
		return h.renderUnknown(c, "status")
	}

	return h.render(c, "status", status.String(), statusColor(status))
}

// Uptime handles GET /api/badge/:id/uptime.svg?duration=24h|30d.
// Renders "uptime: 99.9%", colored by how close the percentage is to 100.
// duration defaults to 24h; any value other than "24h" or "30d" also falls
// back to 24h.
func (h *BadgeHandlers) Uptime(c echo.Context) error {
	ctx := c.Request().Context()

	monitorID, ok := parseBadgeMonitorID(c)
	if !ok {
		return h.renderUnknown(c, "uptime")
	}

	if _, err := h.monitors.GetByID(ctx, monitorID); err != nil {
		return h.renderUnknown(c, "uptime")
	}

	window, _ := parseBadgeDuration(c.QueryParam("duration"))
	now := time.Now().UTC()
	pct, err := h.aggregate.GetUptimePercent(ctx, monitorID, now.Add(-window), now)
	if err != nil || pct == nil {
		return h.renderUnknown(c, "uptime")
	}

	return h.render(c, "uptime", formatUptimePercent(*pct), uptimeColor(*pct))
}

// Ping handles GET /api/badge/:id/ping.svg.
// Renders "ping: <n>ms" from the monitor's latest heartbeat. Renders
// "ping: n/a" (gray) if there is no heartbeat yet or the latest heartbeat
// has no measured latency.
func (h *BadgeHandlers) Ping(c echo.Context) error {
	ctx := c.Request().Context()

	monitorID, ok := parseBadgeMonitorID(c)
	if !ok {
		return h.renderUnknown(c, "ping")
	}

	if _, err := h.monitors.GetByID(ctx, monitorID); err != nil {
		return h.renderUnknown(c, "ping")
	}

	latest, err := h.heartbeats.GetLatest(ctx, monitorID)
	if err != nil || latest == nil || latest.Ping <= 0 {
		return h.render(c, "ping", "n/a", badgeColorGray)
	}

	return h.render(c, "ping", fmt.Sprintf("%dms", latest.Ping), pingColor(latest.Ping))
}

// latestStatus resolves the monitor's current status from its latest
// heartbeat. A monitor that exists but has no heartbeat yet is reported as
// StatusPending (matches the UI's "pending" treatment for new monitors).
func (h *BadgeHandlers) latestStatus(ctx context.Context, monitorID int64) (domain.Status, error) {
	latest, err := h.heartbeats.GetLatest(ctx, monitorID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return domain.StatusPending, nil
		}
		return 0, err
	}
	if latest == nil {
		return domain.StatusPending, nil
	}
	return latest.Status, nil
}

// render writes a badge SVG response with the given label/value/color.
func (h *BadgeHandlers) render(c echo.Context, label, value, color string) error {
	svg := renderBadgeSVG(label, value, color)
	c.Response().Header().Set(echo.HeaderCacheControl, "public, max-age=60")
	return c.Blob(http.StatusOK, "image/svg+xml; charset=utf-8", svg)
}

// renderUnknown writes a gray "unknown" badge with HTTP 200.
func (h *BadgeHandlers) renderUnknown(c echo.Context, label string) error {
	return h.render(c, label, "unknown", badgeColorGray)
}

// parseBadgeMonitorID extracts and validates the :id path param.
func parseBadgeMonitorID(c echo.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// parseBadgeDuration maps the duration query param to a lookback window.
// Returns the canonical duration string alongside the parsed value so
// callers can label the badge if desired.
func parseBadgeDuration(raw string) (time.Duration, string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "30d":
		return 30 * 24 * time.Hour, "30d"
	default:
		return 24 * time.Hour, "24h"
	}
}

// formatUptimePercent formats a percentage the way shields.io-style badges
// conventionally do: whole numbers print without a decimal, everything else
// gets one decimal place.
func formatUptimePercent(pct float64) string {
	if pct >= 99.995 {
		return "100%"
	}
	if pct <= 0.005 {
		return "0%"
	}
	return fmt.Sprintf("%.1f%%", pct)
}

// statusColor maps a monitor status to its badge color.
func statusColor(status domain.Status) string {
	switch status {
	case domain.StatusUp:
		return badgeColorGreen
	case domain.StatusDown:
		return badgeColorRed
	case domain.StatusMaintenance:
		return badgeColorBlue
	case domain.StatusPending:
		return badgeColorAmber
	default:
		return badgeColorGray
	}
}

// uptimeColor scales the badge color with how close uptime is to 100%.
func uptimeColor(pct float64) string {
	switch {
	case pct >= 99:
		return badgeColorGreen
	case pct >= 95:
		return badgeColorAmber
	default:
		return badgeColorRed
	}
}

// pingColor scales the badge color with response latency.
func pingColor(ms int) string {
	switch {
	case ms <= 300:
		return badgeColorGreen
	case ms <= 1000:
		return badgeColorAmber
	default:
		return badgeColorRed
	}
}

// ---------------------------------------------------------------------------
// SVG rendering (hand-rolled, no external badge library).
// ---------------------------------------------------------------------------

const (
	badgeHeight    = 20
	badgeFontSize  = 11
	badgePadding   = 10 // total horizontal padding per segment (5px each side)
	badgeFontScale = 10 // shields.io-style scale(.1) crisp-text trick
)

// renderBadgeSVG hand-rolls a two-segment shields.io-style flat badge:
// a gray label box followed by a colored value box, with approximate
// (measured-ish) text widths — no external SVG/badge library involved.
//
// The entire SVG is drawn in a 10× coordinate system (viewBox) and then
// displayed at the final pixel size via width/height. This gives the
// shields.io-style sub-pixel-crisp text without needing per-element
// scale(.1) transforms, which break when the SVG is embedded as an <img>.
func renderBadgeSVG(label, value, color string) []byte {
	labelText := escapeXML(label)
	valueText := escapeXML(value)

	labelTextWidth := estimateTextWidth(label)
	valueTextWidth := estimateTextWidth(value)

	// All coordinates are in the 10× internal coordinate system.
	labelWidth := (labelTextWidth + badgePadding) * badgeFontScale
	valueWidth := (valueTextWidth + badgePadding) * badgeFontScale
	totalWidth := labelWidth + valueWidth
	totalHeight := badgeHeight * badgeFontScale
	fontSize := badgeFontSize * badgeFontScale

	labelCenterX := labelWidth / 2
	valueCenterX := labelWidth + valueWidth/2

	ariaLabel := escapeXMLAttr(label + ": " + value)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s">`,
		totalWidth/badgeFontScale, badgeHeight, totalWidth, totalHeight, ariaLabel)
	fmt.Fprintf(&b, `<title>%s</title>`, ariaLabel)
	b.WriteString(`<linearGradient id="s" x2="0" y2="100%">`)
	b.WriteString(`<stop offset="0" stop-color="#bbb" stop-opacity=".1"/>`)
	b.WriteString(`<stop offset="1" stop-opacity=".1"/>`)
	b.WriteString(`</linearGradient>`)
	fmt.Fprintf(&b, `<clipPath id="r"><rect width="%d" height="%d" rx="30" fill="#fff"/></clipPath>`,
		totalWidth, totalHeight)
	b.WriteString(`<g clip-path="url(#r)">`)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="%s"/>`, labelWidth, totalHeight, badgeLabelColor)
	fmt.Fprintf(&b, `<rect x="%d" width="%d" height="%d" fill="%s"/>`, labelWidth, valueWidth, totalHeight, color)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="url(#s)"/>`, totalWidth, totalHeight)
	b.WriteString(`</g>`)
	fmt.Fprintf(&b, `<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="%d">`,
		fontSize)
	writeBadgeText(&b, labelCenterX, labelTextWidth*badgeFontScale, labelText)
	writeBadgeText(&b, valueCenterX, valueTextWidth*badgeFontScale, valueText)
	b.WriteString(`</g>`)
	b.WriteString(`</svg>`)

	return []byte(b.String())
}

// writeBadgeText emits the shadow+foreground text pair for one badge
// segment. Coordinates are already in the 10× viewBox system, so no
// per-element scale transform is needed — the viewBox handles the
// downscaling for crisp rendering.
func writeBadgeText(b *strings.Builder, centerX, textWidth int, text string) {
	y := (badgeHeight*badgeFontScale + badgeFontSize*badgeFontScale) / 2
	fmt.Fprintf(b, `<text x="%d" y="%d" fill="#010101" fill-opacity=".3" textLength="%d">%s</text>`,
		centerX, y+badgeFontScale, textWidth, text)
	fmt.Fprintf(b, `<text x="%d" y="%d" textLength="%d">%s</text>`,
		centerX, y, textWidth, text)
}

// estimateTextWidth approximates rendered text width in pixels at 11px
// Verdana. It is a heuristic per-character table, not real font metrics —
// good enough for "measured-ish" badge segment widths without pulling in a
// font-shaping library.
func estimateTextWidth(s string) int {
	var w float64
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			w += 7
		case r == '.' || r == ':':
			w += 3.5
		case r == '%':
			w += 8.5
		case r == ' ':
			w += 4
		case unicode.IsUpper(r):
			w += 8
		default:
			w += 6.5
		}
	}
	return int(math.Ceil(w))
}

var xmlTextEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

var xmlAttrEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

// escapeXML escapes text for use inside an SVG <text>/<title> element body.
func escapeXML(s string) string {
	return xmlTextEscaper.Replace(s)
}

// escapeXMLAttr escapes text for use inside a double-quoted XML attribute.
func escapeXMLAttr(s string) string {
	return xmlAttrEscaper.Replace(s)
}
