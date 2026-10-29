package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestEdgeHTTPIPExtractorSecurity(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := InitializeRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = identity.Close() })
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: identity.ProbeID, StreamID: identity.StreamID, Fingerprint: identity.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewEdgeHTTPHandler(identity, services.NewEdgeEnrollmentService(store),
		func(context.Context, *websocket.Conn, domain.EdgeEnrollment) error { return nil },
		func(context.Context) EdgeReadiness { return EdgeReadiness{} })
	if err != nil {
		t.Fatal(err)
	}
	if handler.IPExtractor == nil {
		t.Fatal("production edge server must configure IPExtractor")
	}
	// Test-only route: edge sockets authenticate through pinned TLS, not a
	// reverse proxy. No forwarding header is an authority for their client IP.
	handler.GET("/_echo_security_ip", func(c echo.Context) error {
		return c.String(http.StatusOK, c.RealIP())
	})
	for _, peer := range []string{"198.51.100.7", "10.1.2.3", "127.0.0.1", "2001:db8::7", "fd00::7", "fe80::7"} {
		t.Run(peer, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/_echo_security_ip", nil)
			req.RemoteAddr = net.JoinHostPort(peer, "40000")
			req.Header.Set(echo.HeaderXForwardedFor, "203.0.113.99")
			req.Header.Set(echo.HeaderXRealIP, "203.0.113.100")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != peer {
				t.Fatalf("status=%d client IP=%q; want 200/%q", rec.Code, rec.Body.String(), peer)
			}
		})
	}
}
