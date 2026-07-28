package checker

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
)

// newProxyConfigFragment builds the exact "_proxy" config shape the
// scheduler injects into a checker config — see
// internal/adapters/scheduler/proxy_resolver.go's configFor, which returns:
//
//	map[string]any{
//		"protocol": p.Protocol,
//		"host":     p.Host,
//		"port":     p.Port,
//		"auth":     p.Auth,
//		"username": p.Username,
//		"password": p.Password,
//	}
//
// Note port is a plain Go int here (proxy_resolver.go builds this map
// directly in Go, never through a JSON round-trip), matching what
// buildProxyTransport's intFromAny in http.go expects.
func newProxyConfigFragment(t *testing.T, proxyServerURL, username, password string, auth bool) map[string]any {
	t.Helper()
	u, err := url.Parse(proxyServerURL)
	if err != nil {
		t.Fatalf("parse proxy server URL %q: %v", proxyServerURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse proxy server port from %q: %v", proxyServerURL, err)
	}
	return map[string]any{
		"protocol": "http",
		"host":     u.Hostname(),
		"port":     port,
		"auth":     auth,
		"username": username,
		"password": password,
	}
}

// newRecordingHTTPProxy stands up an httptest.Server that behaves like a
// forward HTTP proxy: for a plain-http target, Go's http.Transport (when
// configured with Proxy: http.ProxyURL(...)) sends the request directly to
// the proxy with an absolute-URI request line, so the proxy's r.URL arrives
// fully populated (scheme+host+path) instead of just a path — see the
// net/http docs on Request.URL for server-side proxy requests. This fake
// proxy records that it was hit (and the Proxy-Authorization header, if
// any) then forwards the request on to the real target and relays the
// response back, so the checker sees a normal 200 response but only ever
// dialed the proxy's address.
func newRecordingHTTPProxy(t *testing.T) (srv *httptest.Server, hit *atomic.Bool, lastProxyAuthHeader *atomic.Value) {
	t.Helper()
	hit = &atomic.Bool{}
	lastProxyAuthHeader = &atomic.Value{}
	lastProxyAuthHeader.Store("")

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		lastProxyAuthHeader.Store(r.Header.Get("Proxy-Authorization"))

		if !r.URL.IsAbs() {
			http.Error(w, "expected an absolute-URI proxy request, got: "+r.URL.String(), http.StatusBadRequest)
			return
		}

		// Forward to the real origin using a direct (non-proxied) client so
		// the fake proxy actually behaves like one instead of looping.
		fwdReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, "forward: "+err.Error(), http.StatusBadGateway)
			return
		}
		fwdReq.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(fwdReq)
		if err != nil {
			http.Error(w, "forward: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "read forwarded body: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, hit, lastProxyAuthHeader
}

// TestHTTPChecker_Check_RoutesThroughConfiguredProxy proves the check
// traffic actually traverses the proxy server named in the "_proxy" config
// fragment — not merely that buildProxyTransport compiles and returns a
// non-nil transport. If the origin were hit directly, proxyHit would stay
// false and this test would fail even though the top-level result is UP.
func TestHTTPChecker_Check_RoutesThroughConfiguredProxy(t *testing.T) {
	var originHit atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHit.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("origin-ok"))
	}))
	defer origin.Close()

	proxySrv, proxyHit, _ := newRecordingHTTPProxy(t)

	config := map[string]any{
		"url":     origin.URL,
		"timeout": 5.0,
		"_proxy":  newProxyConfigFragment(t, proxySrv.URL, "", "", false),
	}

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), config)
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}

	if !proxyHit.Load() {
		t.Fatal("proxy server was never hit — check did not traverse the configured proxy")
	}
	if !originHit.Load() {
		t.Fatal("origin server was never reached (even indirectly via the proxy)")
	}
	if result.Status.String() != "UP" {
		t.Fatalf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

// TestHTTPChecker_Check_NoProxyConfigured_GoesDirect asserts the absence of
// a "_proxy" key in config means the checker dials the origin directly —
// the proxy server, if one happens to exist, must never be hit.
func TestHTTPChecker_Check_NoProxyConfigured_GoesDirect(t *testing.T) {
	var originHit atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHit.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("origin-ok"))
	}))
	defer origin.Close()

	proxySrv, proxyHit, _ := newRecordingHTTPProxy(t)
	_ = proxySrv // exists only to prove it is NOT contacted

	config := map[string]any{
		"url":     origin.URL,
		"timeout": 5.0,
		// deliberately no "_proxy" key
	}

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), config)
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}

	if proxyHit.Load() {
		t.Fatal("proxy server was hit despite no _proxy config being present")
	}
	if !originHit.Load() {
		t.Fatal("origin server was never reached directly")
	}
	if result.Status.String() != "UP" {
		t.Fatalf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

// TestHTTPChecker_Check_ProxyAuth_SendsProxyAuthorizationHeader asserts that
// when the resolved proxy has auth enabled, the credentials reach the proxy
// as a standard Proxy-Authorization: Basic header — this is what
// url.UserPassword + http.ProxyURL produce, and this test proves it end to
// end rather than trusting that wiring blindly.
func TestHTTPChecker_Check_ProxyAuth_SendsProxyAuthorizationHeader(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("origin-ok"))
	}))
	defer origin.Close()

	proxySrv, proxyHit, lastProxyAuthHeader := newRecordingHTTPProxy(t)

	const username = "proxyuser"
	const password = "s3cr3t-proxy-pass"
	config := map[string]any{
		"url":     origin.URL,
		"timeout": 5.0,
		"_proxy":  newProxyConfigFragment(t, proxySrv.URL, username, password, true),
	}

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), config)
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if !proxyHit.Load() {
		t.Fatal("proxy server was never hit")
	}
	if result.Status.String() != "UP" {
		t.Fatalf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}

	gotHeader, _ := lastProxyAuthHeader.Load().(string)
	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	if gotHeader != wantHeader {
		t.Fatalf("Proxy-Authorization header = %q, want %q", gotHeader, wantHeader)
	}
}
