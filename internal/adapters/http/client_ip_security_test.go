package http_test

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
)

func TestEchoTrustedProxyResolution(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.42.0.8/32"), netip.MustParsePrefix("10.42.0.9/32"), netip.MustParsePrefix("fd00:1234::8/128")}
	for _, tc := range []struct {
		name     string
		peer     string
		xff      []string
		prefixes []netip.Prefix
		want     string
	}{
		{"public direct ignores spoof", "198.51.100.7", []string{"203.0.113.99"}, trusted, "198.51.100.7"},
		{"private direct ignores spoof", "10.5.6.7", []string{"203.0.113.99"}, trusted, "10.5.6.7"},
		{"loopback is not implicitly trusted", "127.0.0.1", []string{"203.0.113.99"}, trusted, "127.0.0.1"},
		{"link local is not implicitly trusted", "fe80::7", []string{"203.0.113.99"}, trusted, "fe80::7"},
		{"proxied public client", "10.42.0.8", []string{"203.0.113.99, 198.51.100.7"}, trusted, "198.51.100.7"},
		{"proxied private client stops before spoof", "10.42.0.8", []string{"203.0.113.99, 10.5.6.7, 10.42.0.9"}, trusted, "10.5.6.7"},
		{"multiple header lines", "10.42.0.8", []string{"203.0.113.99", "10.5.6.7, 10.42.0.9"}, trusted, "10.5.6.7"},
		{"proxied IPv6 client", "fd00:1234::8", []string{"203.0.113.99, [fd00:5678::7]"}, trusted, "fd00:5678::7"},
		{"missing header ignores X-Real-IP", "10.42.0.8", nil, trusted, "10.42.0.8"},
		{"malformed nearest hop fails closed", "10.42.0.8", []string{"203.0.113.99, invalid"}, trusted, "10.42.0.8"},
		{"invalid programmatic prefix fails closed", "10.42.0.8", []string{"203.0.113.99"}, []netip.Prefix{trusted[0], {}}, "10.42.0.8"},
		{"IPv4 catch-all fails closed", "10.42.0.8", []string{"203.0.113.99"}, []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, "10.42.0.8"},
		{"IPv6 catch-all fails closed", "fd00:1234::8", []string{"203.0.113.99"}, []netip.Prefix{netip.MustParsePrefix("::/0")}, "fd00:1234::8"},
		{"mapped IPv4 prefix fails closed", "10.42.0.8", []string{"203.0.113.99"}, []netip.Prefix{netip.MustParsePrefix("::ffff:10.42.0.8/128")}, "10.42.0.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := authRateLimitRouterWithOptions(t, httppkg.RouterOptions{TrustedProxyCIDRs: tc.prefixes})
			e.GET("/_echo_security_ip", func(c echo.Context) error {
				return c.String(http.StatusOK, c.RealIP())
			})
			req := httptest.NewRequest(http.MethodGet, "/_echo_security_ip", nil)
			req.RemoteAddr = net.JoinHostPort(tc.peer, "40000")
			for _, line := range tc.xff {
				req.Header.Add(echo.HeaderXForwardedFor, line)
			}
			req.Header.Set(echo.HeaderXRealIP, "203.0.113.100")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
				t.Fatalf("status=%d client IP=%q; want 200/%q", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestEchoTrustedProxyCredentialBudget(t *testing.T) {
	e := authRateLimitRouterWithOptions(t, httppkg.RouterOptions{TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.42.0.8/32")}})
	post := func(client string, attempt int) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.RemoteAddr = "10.42.0.8:40000"
		req.Header.Set(echo.HeaderXForwardedFor, fmt.Sprintf("203.0.113.%d, %s", attempt+1, client))
		req.Header.Set(echo.HeaderXRealIP, fmt.Sprintf("203.0.113.%d", attempt+1))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < httppkg.SignInAttemptsPerMinute; i++ {
		if status := post("10.5.6.7", i); status != http.StatusBadRequest {
			t.Fatalf("attempt %d: status=%d; want 400", i+1, status)
		}
	}
	if status := post("10.5.6.7", httppkg.SignInAttemptsPerMinute); status != http.StatusTooManyRequests {
		t.Fatalf("spoofed prefix bypassed proxied private-client budget: status=%d", status)
	}
	if status := post("10.5.6.8", httppkg.SignInAttemptsPerMinute+1); status != http.StatusBadRequest {
		t.Fatalf("distinct proxied client shares exhausted budget: status=%d", status)
	}
}
