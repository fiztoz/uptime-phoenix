package checker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// withPolicy swaps the process guard for one test. Tests in this package run
// sequentially (no t.Parallel), so the swap cannot leak into another test.
func withPolicy(t *testing.T, p *netguard.Policy) {
	t.Helper()
	netguard.SetDefault(p)
	t.Cleanup(netguard.AllowLoopbackForTests)
}

func assertBlocked(t *testing.T, name, msg string) {
	t.Helper()
	if !strings.Contains(msg, "destination address is not allowed") {
		t.Errorf("%s: message %q; want the netguard refusal", name, msg)
	}
}

func TestCheckers_RefuseInternalTargets(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"secret":"from-internal"}`))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	portNum, _ := strconv.Atoi(port)

	withPolicy(t, netguard.NewPolicy(nil))
	ctx := context.Background()

	res, _ := (HTTPChecker{}).Check(ctx, map[string]any{"url": srv.URL, "timeout": 5.0, "json_query": "secret", "expected_value": "x"})
	assertBlocked(t, "http literal loopback", res.Message)
	if strings.Contains(res.Message, "from-internal") {
		t.Errorf("http: message leaks response content: %q", res.Message)
	}

	res, _ = (HTTPChecker{}).Check(ctx, map[string]any{"url": "http://localhost:" + port + "/", "timeout": 5.0})
	assertBlocked(t, "http hostname resolving to loopback", res.Message)

	res, _ = (HTTPChecker{}).Check(ctx, map[string]any{"url": "http://169.254.169.254/latest/meta-data/", "timeout": 5.0})
	assertBlocked(t, "http metadata", res.Message)

	res, _ = (HTTPChecker{}).Check(ctx, map[string]any{"url": "http://[::ffff:127.0.0.1]:" + port + "/", "timeout": 5.0})
	assertBlocked(t, "http ipv4-mapped loopback", res.Message)

	res, _ = (HTTPChecker{}).Check(ctx, map[string]any{"url": srv.URL, "timeout": 5.0, "tls_ignore": true})
	assertBlocked(t, "http tls_ignore transport", res.Message)

	res, _ = (TCPChecker{}).Check(ctx, map[string]any{"hostname": "127.0.0.1", "port": float64(portNum), "timeout": 5.0})
	assertBlocked(t, "tcp", res.Message)

	res, _ = (WebSocketChecker{}).Check(ctx, map[string]any{"url": "ws://127.0.0.1:" + port + "/", "timeout": 5.0})
	assertBlocked(t, "websocket", res.Message)

	if hits != 0 {
		t.Fatalf("internal server received %d requests; want 0", hits)
	}
}

// A public-looking first hop that redirects into an internal address is
// refused on the redirect hop. The first server is allowlisted to stand in
// for a public host.
func TestHTTPChecker_RefusesRedirectIntoInternalAddress(t *testing.T) {
	internalHits := 0
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalHits++
	}))
	defer internal.Close()
	_, internalPort, _ := net.SplitHostPort(strings.TrimPrefix(internal.URL, "http://"))

	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.2:"+internalPort+"/", http.StatusFound)
	}))
	defer public.Close()

	allowed, err := netguard.ParseAllowlist("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	withPolicy(t, netguard.NewPolicy(allowed))

	res, _ := (HTTPChecker{}).Check(context.Background(), map[string]any{"url": public.URL, "timeout": 5.0})
	assertBlocked(t, "redirect hop", res.Message)
	if internalHits != 0 {
		t.Fatalf("internal server reached via redirect %d times", internalHits)
	}

	res, _ = (HTTPChecker{}).Check(context.Background(), map[string]any{
		"url": public.URL, "timeout": 5.0,
		"_proxy": map[string]any{"protocol": "http", "host": "10.1.2.3", "port": 3128},
	})
	assertBlocked(t, "proxy on an internal address", res.Message)
}

// An allowlisted internal CIDR is reachable.
func TestHTTPChecker_AllowlistedInternalTargetIsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	allowed, err := netguard.ParseAllowlist("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	withPolicy(t, netguard.NewPolicy(allowed))

	res, _ := (HTTPChecker{}).Check(context.Background(), map[string]any{"url": srv.URL, "timeout": 5.0})
	if res.Status != domain.StatusUp {
		t.Fatalf("allowlisted target: status %s (%s); want UP", res.Status, res.Message)
	}
}
