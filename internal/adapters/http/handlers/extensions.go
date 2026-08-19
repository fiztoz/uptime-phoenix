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

// ExtensionHandlers serves GET /api/extensions from the process-start catalog.
type ExtensionHandlers struct {
	items []ExtensionView
}

// NewExtensionHandlers parses raw PHOENIX_EXTENSIONS JSON once and stores the
// view list. Empty, unset, or malformed input logs and becomes an empty
// catalog so the sidebar never 500s.
func NewExtensionHandlers(raw string) *ExtensionHandlers {
	return &ExtensionHandlers{items: parseExtensionViews(raw)}
}

func parseExtensionViews(raw string) []ExtensionView {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []ExtensionView{}
	}
	var parsed []ExtensionView
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		slog.Warn("invalid PHOENIX_EXTENSIONS JSON; serving empty catalog", "error", err)
		return []ExtensionView{}
	}
	out := make([]ExtensionView, 0, len(parsed))
	for _, item := range parsed {
		id := strings.TrimSpace(item.ID)
		title := strings.TrimSpace(item.Title)
		path := strings.TrimSpace(item.Path)
		if id == "" || title == "" || path == "" {
			continue
		}
		out = append(out, ExtensionView{
			ID:    id,
			Title: title,
			Path:  path,
			Icon:  extensionIconPath(path, item.Icon),
		})
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
// Authentication is applied in the router (any authenticated user).
func (h *ExtensionHandlers) List(c echo.Context) error {
	items := h.items
	if items == nil {
		items = []ExtensionView{}
	}
	return c.JSON(http.StatusOK, items)
}
