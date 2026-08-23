package handlers

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"
)

// uiTokenParam is the query key the extension side accepts for its launch
// credential hand-off (ecs-phoenix-ext's uiAuth reads it from the form/query
// and exchanges it for a session cookie).
const uiTokenParam = "ui_token"

// Frame handles GET /api/extensions/:id/frame. It is the iframe launch point:
// the router gates it behind authentication + the view_extensions capability,
// and it answers a 302 into the extension's own Ingress path. When the entry
// carries a launch credential, it is appended as the ui_token query parameter
// the extension accepts; the extension then swaps it for a cookie so internal
// links and forms keep working. Extensions without a credential redirect to
// the plain path. Unknown ids are 404.
//
// The credential never appears in the catalog response — this redirect is the
// only surface that releases it, and only to users Phoenix already authorized
// for extension access.
func (h *ExtensionHandlers) Frame(c echo.Context) error {
	id := strings.TrimSpace(c.Param("id"))
	for _, item := range h.items {
		if item.ID != id {
			continue
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.Redirect(http.StatusFound, extensionFrameLocation(item))
	}
	return c.JSON(http.StatusNotFound, map[string]string{"error": "extension not found"})
}

// extensionFrameLocation builds the redirect target for one catalog entry.
func extensionFrameLocation(item extensionEntry) string {
	if item.UIToken == "" {
		return item.Path
	}
	sep := "?"
	if strings.Contains(item.Path, "?") {
		sep = "&"
	}
	return item.Path + sep + uiTokenParam + "=" + url.QueryEscape(item.UIToken)
}
