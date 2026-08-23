package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// ExtensionView is the public catalog entry for one K8s-registered extension.
// Only id, title, path, and icon are on the wire. Helm-only keys (image,
// secretName, credentials) are ignored even if present in PHOENIX_EXTENSIONS.
// This is not a domain type and is never a monitor.
type ExtensionView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
	Icon  string `json:"icon"`
}

// extensionEntry is the internal catalog row. It extends the public view with
// operator-supplied launch wiring that must never be serialized into any
// response (see extensions_frame.go).
type extensionEntry struct {
	ExtensionView
	UIToken string `json:"uiToken"`
}

// ExtensionHandlers serves GET /api/extensions from the process-start catalog.
type ExtensionHandlers struct {
	items []extensionEntry
}

// NewExtensionHandlers parses raw PHOENIX_EXTENSIONS JSON once and stores the
// catalog. Empty, unset, or malformed input logs and becomes an empty catalog
// so the sidebar never 500s.
func NewExtensionHandlers(raw string) *ExtensionHandlers {
	return &ExtensionHandlers{items: parseExtensionEntries(raw)}
}

func parseExtensionEntries(raw string) []extensionEntry {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []extensionEntry{}
	}
	var parsed []extensionEntry
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		slog.Warn("invalid PHOENIX_EXTENSIONS JSON; serving empty catalog", "error", err)
		return []extensionEntry{}
	}
	out := make([]extensionEntry, 0, len(parsed))
	for _, item := range parsed {
		id := strings.TrimSpace(item.ID)
		title := strings.TrimSpace(item.Title)
		path := strings.TrimSpace(item.Path)
		if id == "" || title == "" || path == "" {
			continue
		}
		out = append(out, extensionEntry{
			ExtensionView: ExtensionView{
				ID:    id,
				Title: title,
				Path:  path,
				Icon:  extensionIconPath(path, item.Icon),
			},
			UIToken: strings.TrimSpace(item.UIToken),
		})
	}
	return out
}

// views returns only the public wire shape of the catalog.
func (h *ExtensionHandlers) views() []ExtensionView {
	out := make([]ExtensionView, 0, len(h.items))
	for _, item := range h.items {
		out = append(out, item.ExtensionView)
	}
	return out
}

// extensionIconPath is a same-origin path the plugin image serves (default
// {path}/icon.svg). Helm cannot extract files from a container image; the
// plugin must publish the asset on its Ingress prefix. Remote URLs, schemes,
// and ".." are rejected so the sidebar <img> cannot be pointed off-host.
func extensionIconPath(extPath, icon string) string {
	fallback := strings.TrimRight(extPath, "/") + "/icon.svg"
	icon = strings.TrimSpace(icon)
	if icon == "" || !safeExtensionIconPath(icon) {
		return fallback
	}
	return icon
}

func safeExtensionIconPath(icon string) bool {
	if !strings.HasPrefix(icon, "/") || strings.HasPrefix(icon, "//") {
		return false
	}
	if strings.ContainsAny(icon, ":\\") || strings.Contains(icon, "..") {
		return false
	}
	return true
}

// List handles GET /api/extensions. Always a JSON array, never null.
// Authentication and the view_extensions capability are applied in the router.
func (h *ExtensionHandlers) List(c echo.Context) error {
	return c.JSON(http.StatusOK, h.views())
}
