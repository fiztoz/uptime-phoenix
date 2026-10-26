package checker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// withAllowlist swaps the restricted tier's allowlist for one test (the
// package TestMain allowlists loopback for the ordinary checker tests). Tests
// in this package run sequentially (no t.Parallel), so the swap cannot leak.
func withAllowlist(t *testing.T, raw string) {
	t.Helper()
	allowed, err := netguard.ParseAllowlist(raw)
	if err != nil {
		t.Fatal(err)
	}
	netguard.SetRestricted(netguard.NewPolicy(allowed))
	t.Cleanup(netguard.AllowLoopbackForTests)
}

func adminCtx() context.Context {
	return domain.WithOutboundTrust(context.Background(), domain.OutboundTrustAdmin)
}

func restrictedCtx() context.Context {
	return domain.WithOutboundTrust(context.Background(), domain.OutboundTrustRestricted)
}

func assertBlocked(t *testing.T, name, msg string) {
	t.Helper()
	if !strings.Contains(msg, "destination address is not allowed") {
		t.Errorf("%s: message %q; want the netguard refusal", name, msg)
	}
}

// countingServer is an internal HTTP target that counts every request.
func countingServer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// countingListener accepts and counts TCP connections on loopback.
func countingListener(t *testing.T) (int, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepts atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, &accepts
}

// A monitor owned by a non-admin (or with no owner trust on the context)
// cannot reach a private target through any guarded checker.
func TestCheckers_RestrictedOwnerCannotReachInternalTargets(t *testing.T) {
	srv, hits := countingServer(t, `{"secret":"from-internal"}`)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	tcpPort, accepts := countingListener(t)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	udpPort := udp.LocalAddr().(*net.UDPAddr).Port
	withAllowlist(t, "")

	for name, ctx := range map[string]context.Context{"unset": context.Background(), "restricted": restrictedCtx()} {
		res, _ := (HTTPChecker{}).Check(ctx, map[string]any{"url": srv.URL, "timeout": 5.0, "json_query": "secret", "expected_value": "x", "save_body_on_error": true})
		assertBlocked(t, name+" http literal loopback", res.Message)
		if strings.Contains(res.Message, "from-internal") {
			t.Errorf("%s http: message leaks response content: %q", name, res.Message)
		}
		res, _ = (HTTPChecker{}).Check(ctx, map[string]any{"url": "http://localhost:" + port + "/", "timeout": 5.0})
		assertBlocked(t, name+" http hostname resolving to loopback", res.Message)
		res, _ = (HTTPChecker{}).Check(ctx, map[string]any{"url": "http://[::ffff:127.0.0.1]:" + port + "/", "timeout": 5.0})
		assertBlocked(t, name+" http ipv4-mapped loopback", res.Message)
		res, _ = (HTTPChecker{}).Check(ctx, map[string]any{"url": srv.URL, "timeout": 5.0, "tls_ignore": true})
		assertBlocked(t, name+" http tls_ignore transport", res.Message)
		res, _ = (S3Checker{}).Check(ctx, map[string]any{"endpoint": srv.URL, "bucket": "b", "access_key": "a", "secret_key": "s", "timeout": 5.0})
		assertBlocked(t, name+" s3", res.Message)
		res, _ = (WebSocketChecker{}).Check(ctx, map[string]any{"url": "ws://127.0.0.1:" + port + "/", "timeout": 5.0})
		assertBlocked(t, name+" websocket", res.Message)

		res, _ = (TCPChecker{}).Check(ctx, map[string]any{"hostname": "127.0.0.1", "port": float64(tcpPort), "timeout": 5.0})
		assertBlocked(t, name+" tcp", res.Message)
		res, _ = (GRPCChecker{}).Check(ctx, map[string]any{"url": "127.0.0.1:" + strconv.Itoa(tcpPort), "timeout": 2.0})
		assertBlocked(t, name+" grpc", res.Message)
		res, _ = (RabbitMQChecker{}).Check(ctx, map[string]any{"url": "amqp://guest:guest@127.0.0.1:" + strconv.Itoa(tcpPort) + "/", "timeout": 2.0})
		assertBlocked(t, name+" rabbitmq", res.Message)
		for _, scheme := range []string{"tcp", "ws"} {
			res, _ = (MQTTChecker{}).Check(ctx, map[string]any{"broker": scheme + "://127.0.0.1:" + strconv.Itoa(tcpPort), "timeout": 2.0})
			assertBlocked(t, name+" mqtt "+scheme, res.Message)
		}
		res, _ = (MQTTChecker{}).Check(ctx, map[string]any{"broker": "unix:///var/run/mosquitto.sock", "timeout": 2.0})
		assertBlocked(t, name+" mqtt unix socket", res.Message)

		res, _ = (DNSChecker{}).Check(ctx, map[string]any{"hostname": "example.com", "resolve_server": "127.0.0.1", "timeout": 2.0})
		assertBlocked(t, name+" dns", res.Message)
		res, _ = (SNMPChecker{}).Check(ctx, map[string]any{"hostname": "127.0.0.1", "port": float64(udpPort), "oid": "1.3.6.1.2.1.1.1.0", "timeout": 1.0})
		assertBlocked(t, name+" snmp", res.Message)
		res, _ = (PingChecker{}).Check(ctx, map[string]any{"hostname": "127.0.0.1", "count": 1.0, "timeout": 1.0})
		assertBlocked(t, name+" ping", res.Message)
	}

	if n := hits.Load(); n != 0 {
		t.Fatalf("internal HTTP server received %d requests; want 0", n)
	}
	if n := accepts.Load(); n != 0 {
		t.Fatalf("internal TCP listener accepted %d connections; want 0", n)
	}
	_ = udp.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, _ := udp.ReadFrom(make([]byte, 64)); n != 0 {
		t.Fatalf("internal SNMP agent received a %d-byte datagram; want none", n)
	}
}

// An admin-owned monitor reaches a private target with no allowlist at all,
// so admin-only installs keep working after upgrade.
func TestCheckers_AdminOwnerReachesPrivateTarget(t *testing.T) {
	srv, hits := countingServer(t, `{"status":"broken"}`)
	tcpPort, accepts := countingListener(t)
	withAllowlist(t, "")

	res, _ := (HTTPChecker{}).Check(adminCtx(), map[string]any{"url": srv.URL, "timeout": 5.0})
	if res.Status != domain.StatusUp {
		t.Fatalf("admin-owned http: status %s (%s); want UP", res.Status, res.Message)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("internal server received %d requests; want 1", n)
	}
	res, _ = (TCPChecker{}).Check(adminCtx(), map[string]any{"hostname": "127.0.0.1", "port": float64(tcpPort), "timeout": 5.0})
	if res.Status != domain.StatusUp {
		t.Fatalf("admin-owned tcp: status %s (%s); want UP", res.Status, res.Message)
	}
	// The handshake completes in the kernel before the listener goroutine
	// runs Accept, so wait briefly for the accept to be counted.
	for deadline := time.Now().Add(2 * time.Second); accepts.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if n := accepts.Load(); n == 0 {
		t.Fatal("admin-owned tcp check never connected")
	}

	// Admin-owned monitors get the response value back in assertion failures
	// and keep save_body_on_* (restricted owners get neither).
	res, _ = (HTTPChecker{}).Check(adminCtx(), map[string]any{
		"url": srv.URL, "timeout": 5.0, "json_query": "status", "json_operator": "equals",
		"expected_value": "ok", "save_body_on_error": true,
	})
	if res.Status != domain.StatusDown || !strings.Contains(res.Message, `(got "broken")`) {
		t.Fatalf("admin-owned assertion message %q; want the actual value", res.Message)
	}
	if res.Metadata["response_body"] == "" {
		t.Fatal("admin-owned save_body_on_error did not save the body")
	}
}

// A restricted owner whose monitor reaches an allowlisted target still gets
// no response content back.
func TestHTTPChecker_RestrictedOwnerAllowlistedTargetHidesBody(t *testing.T) {
	srv, hits := countingServer(t, `{"status":"broken"}`)
	withAllowlist(t, "127.0.0.1")

	res, _ := (HTTPChecker{}).Check(restrictedCtx(), map[string]any{
		"url": srv.URL, "timeout": 5.0, "json_query": "status", "json_operator": "equals",
		"expected_value": "ok", "save_body_on_error": true,
	})
	if n := hits.Load(); n != 1 {
		t.Fatalf("allowlisted target received %d requests; want 1", n)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status %s; want DOWN", res.Status)
	}
	if strings.Contains(res.Message, "broken") || res.Metadata["response_body"] != "" {
		t.Fatalf("restricted owner got response content back: %q %v", res.Message, res.Metadata)
	}
}

// The metadata floor holds for admin-owned monitors and for an allowlist that
// covers everything.
func TestHTTPChecker_MetadataBlockedEvenWhenAllowlistedEverything(t *testing.T) {
	withAllowlist(t, "0.0.0.0/0, ::/0")
	for name, ctx := range map[string]context.Context{"admin": adminCtx(), "restricted": restrictedCtx()} {
		for _, target := range []string{
			"http://169.254.169.254/latest/meta-data/",
			"http://[fd00:ec2::254]/latest/meta-data/",
			"http://168.63.129.16/machine",
			"http://[::ffff:169.254.169.254]/",
			"http://[64:ff9b::a9fe:a9fe]/",
		} {
			res, _ := (HTTPChecker{}).Check(ctx, map[string]any{"url": target, "timeout": 2.0})
			assertBlocked(t, name+" "+target, res.Message)
		}
	}
	// The allowlist itself is honored for restricted owners.
	srv, hits := countingServer(t, "ok")
	res, _ := (HTTPChecker{}).Check(restrictedCtx(), map[string]any{"url": srv.URL, "timeout": 5.0})
	if res.Status != domain.StatusUp || hits.Load() != 1 {
		t.Fatalf("0.0.0.0/0 allowlist: status %s (%s), hits %d; want UP, 1", res.Status, res.Message, hits.Load())
	}
}

// An allowlisted CIDR is reachable for a restricted owner; a neighboring
// address is not.
func TestHTTPChecker_AllowlistEntryWorksForRestrictedOwner(t *testing.T) {
	srv, hits := countingServer(t, "ok")
	withAllowlist(t, "127.0.0.1/32")

	res, _ := (HTTPChecker{}).Check(restrictedCtx(), map[string]any{"url": srv.URL, "timeout": 5.0})
	if res.Status != domain.StatusUp {
		t.Fatalf("allowlisted target: status %s (%s); want UP", res.Status, res.Message)
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	res, _ = (HTTPChecker{}).Check(restrictedCtx(), map[string]any{"url": "http://127.0.0.2:" + port + "/", "timeout": 5.0})
	assertBlocked(t, "neighbor of the allowlisted address", res.Message)
	if n := hits.Load(); n != 1 {
		t.Fatalf("server received %d requests; want 1", n)
	}
}

// A redirect from an allowed hop into an internal address is refused on the
// redirect hop.
func TestHTTPChecker_RefusesRedirectIntoInternalAddress(t *testing.T) {
	internal, internalHits := countingServer(t, "secret")
	_, internalPort, _ := net.SplitHostPort(strings.TrimPrefix(internal.URL, "http://"))
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.2:"+internalPort+"/", http.StatusFound)
	}))
	defer public.Close()
	withAllowlist(t, "127.0.0.1") // stands in for a public first hop

	res, _ := (HTTPChecker{}).Check(restrictedCtx(), map[string]any{"url": public.URL, "timeout": 5.0})
	assertBlocked(t, "redirect hop", res.Message)
	if n := internalHits.Load(); n != 0 {
		t.Fatalf("internal server reached via redirect %d times", n)
	}
}

// A proxy cannot carry a restricted owner's request to an internal target:
// the target is checked before the request is handed to the proxy, so the
// proxy is never contacted. The same proxy serves an admin-owned monitor.
// The environment proxy (HTTP_PROXY) goes through the same guard; see
// netguard's TestTransport_EnvironmentStyleProxyCannotReachBlockedTarget.
func TestHTTPChecker_ProxyCannotReachBlockedTargetForRestrictedOwner(t *testing.T) {
	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		_, _ = w.Write([]byte("proxied"))
	}))
	defer proxySrv.Close()
	withAllowlist(t, "")
	cfg := func() map[string]any {
		return map[string]any{
			"url": "http://10.255.255.1/internal", "timeout": 5.0,
			"_proxy": newProxyConfigFragment(t, proxySrv.URL, "", "", false),
		}
	}

	res, _ := (HTTPChecker{}).Check(restrictedCtx(), cfg())
	assertBlocked(t, "restricted owner through proxy", res.Message)
	if n := proxyHits.Load(); n != 0 {
		t.Fatalf("proxy received %d requests for a blocked target; want 0", n)
	}

	res, _ = (HTTPChecker{}).Check(adminCtx(), cfg())
	if res.Status != domain.StatusUp || proxyHits.Load() != 1 {
		t.Fatalf("admin owner through proxy: status %s (%s), proxy hits %d; want UP, 1", res.Status, res.Message, proxyHits.Load())
	}

	// The floor applies behind a proxy too.
	metadata := cfg()
	metadata["url"] = "http://169.254.169.254/latest/meta-data/"
	res, _ = (HTTPChecker{}).Check(adminCtx(), metadata)
	assertBlocked(t, "admin owner metadata through proxy", res.Message)
	if n := proxyHits.Load(); n != 1 {
		t.Fatalf("proxy received %d requests; want 1 (metadata must not be proxied)", n)
	}
}

// A remote probe reaches private targets (floor only) but, not knowing the
// owner, does not echo response values; save_body_* (accepted by the API only
// on admin-owned monitors) still works there.
func TestHTTPChecker_ProbeTrustReachesPrivateButHidesValues(t *testing.T) {
	srv, hits := countingServer(t, `{"status":"broken"}`)
	withAllowlist(t, "")
	probeCtx := domain.WithOutboundTrust(context.Background(), domain.OutboundTrustProbe)

	res, _ := (HTTPChecker{}).Check(probeCtx, map[string]any{
		"url": srv.URL, "timeout": 5.0, "json_query": "status", "json_operator": "equals",
		"expected_value": "ok", "save_body_on_error": true,
	})
	if n := hits.Load(); n != 1 {
		t.Fatalf("probe check reached the private server %d times; want 1", n)
	}
	if strings.Contains(res.Message, `(got "broken")`) {
		t.Fatalf("probe check echoed the JSON value: %q", res.Message)
	}
	if res.Metadata["response_body"] == "" {
		t.Fatal("probe check dropped save_body_on_error")
	}
	res, _ = (HTTPChecker{}).Check(probeCtx, map[string]any{"url": "http://169.254.169.254/", "timeout": 2.0})
	assertBlocked(t, "probe metadata", res.Message)
}
