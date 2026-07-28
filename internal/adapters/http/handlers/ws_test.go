package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// These tests pin the R2.4 fix: the WebSocket origin check used to be
// disabled unconditionally (InsecureSkipVerify: true), so any website could
// open authenticated WS connections from a visitor's browser.
//
// The HTTP-level tests never complete a WebSocket handshake: coder/websocket
// runs its origin check BEFORE hijacking the connection, so a refused origin
// deterministically yields 403 against a plain httptest recorder, and an
// admitted origin proceeds to the hijack attempt (which fails on a recorder
// with a non-403 status). That is enough to observe the gate's effect without
// managing WS session teardown.

// newWSUpgradeRequest builds a syntactically valid WebSocket upgrade request
// (httptest sets Host to "example.com") carrying the given Origin header.
func newWSUpgradeRequest(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

// serveWSUpgrade drives HandleWS with the given origin policy. hub and
// authSvc are nil: every path exercised here fails before they are touched.
func serveWSUpgrade(t *testing.T, cfg WSConfig, origin string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	h := NewWSHandlers(nil, nil, cfg)
	e.GET("/ws", h.HandleWS)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, newWSUpgradeRequest(origin))
	return rec
}

func TestHandleWS_RejectsCrossOriginByDefault(t *testing.T) {
	rec := serveWSUpgrade(t, WSConfig{}, "http://evil.example.net")
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin upgrade with zero config = %d; want 403 (origin check must be ON by default)", rec.Code)
	}
}

func TestHandleWS_SameOriginPassesOriginGate(t *testing.T) {
	// Same-origin must clear the origin gate. The upgrade then fails at the
	// hijack step (recorders aren't hijackable) — any status but 403 proves
	// the gate itself admitted the request.
	rec := serveWSUpgrade(t, WSConfig{}, "http://example.com")
	if rec.Code == http.StatusForbidden {
		t.Errorf("same-origin upgrade rejected with 403; the origin gate must admit same-origin requests")
	}
}

func TestHandleWS_AllowedOriginPatternAdmitsOnlyListedHosts(t *testing.T) {
	cfg := WSConfig{AllowedOriginPatterns: []string{"localhost:5173"}}

	rec := serveWSUpgrade(t, cfg, "http://localhost:5173")
	if rec.Code == http.StatusForbidden {
		t.Errorf("upgrade from configured origin pattern rejected with 403; want admitted")
	}

	// The pattern must not open the door to unlisted hosts.
	rec = serveWSUpgrade(t, cfg, "http://evil.example.net")
	if rec.Code != http.StatusForbidden {
		t.Errorf("upgrade from unlisted origin = %d; want 403", rec.Code)
	}
}

func TestHandleWS_InsecureSkipDisablesOriginCheck(t *testing.T) {
	rec := serveWSUpgrade(t, WSConfig{InsecureSkipOriginCheck: true}, "http://evil.example.net")
	if rec.Code == http.StatusForbidden {
		t.Errorf("upgrade with origin check disabled rejected with 403; want admitted")
	}
}

func TestWSAcceptOptions_SecureByDefault(t *testing.T) {
	opts := wsAcceptOptions(WSConfig{})
	if opts.InsecureSkipVerify {
		t.Error("zero WSConfig must not skip origin verification")
	}
	if len(opts.OriginPatterns) != 0 {
		t.Errorf("zero WSConfig OriginPatterns = %v; want none", opts.OriginPatterns)
	}

	opts = wsAcceptOptions(WSConfig{
		AllowedOriginPatterns:   []string{"a.example.com", "b.example.com"},
		InsecureSkipOriginCheck: true,
	})
	if !opts.InsecureSkipVerify {
		t.Error("InsecureSkipOriginCheck did not carry into accept options")
	}
	if len(opts.OriginPatterns) != 2 || opts.OriginPatterns[0] != "a.example.com" || opts.OriginPatterns[1] != "b.example.com" {
		t.Errorf("OriginPatterns = %v; want the configured patterns verbatim", opts.OriginPatterns)
	}
}
