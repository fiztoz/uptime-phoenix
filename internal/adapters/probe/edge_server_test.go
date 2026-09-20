package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestEdgeServerPinnedEnrollmentAndRuntimeRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := InitializeRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = identity.Close() }()
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: identity.ProbeID, StreamID: identity.StreamID, Fingerprint: identity.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	enrollment := services.NewEdgeEnrollmentService(store)
	token, err := enrollment.Issue(t.Context(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	runtimeToken := RuntimeTokenPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
	accepted := make(chan domain.EdgeEnrollment, 1)
	handler, err := NewEdgeHTTPHandler(identity, enrollment, func(ctx context.Context, conn *websocket.Conn, binding domain.EdgeEnrollment) error {
		select {
		case accepted <- binding:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, _, err := conn.Read(ctx)
		return err
	}, func(context.Context) EdgeReadiness {
		return EdgeReadiness{Ready: true, DBWritable: true, SchedulerHealthy: true, ConfigRevision: 1}
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity.Certificate}}
	server.StartTLS()
	defer server.Close()
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https")
	policy := EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	dial := func(path, pin, bearer string) (*websocket.Conn, *http.Response, error) {
		t.Helper()
		client, err := NewPinnedHTTPClient(endpoint+path, pin, policy)
		if err != nil {
			return nil, nil, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		return websocket.Dial(ctx, endpoint+path, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + bearer}}, Subprotocols: []string{"phoenix.probe.v1"}, CompressionMode: websocket.CompressionDisabled})
	}
	if conn, _, err := dial("/ws/probe/enroll/v1", strings.Repeat("f", 64), token); err == nil {
		_ = conn.CloseNow()
		t.Fatal("wrong pin accepted")
	}
	if conn, resp, err := dial("/ws/probe/enroll/v1", identity.Fingerprint, runtimeToken); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		if conn != nil {
			_ = conn.CloseNow()
		}
		t.Fatalf("wrong enrollment credential: %v %v", resp, err)
	}
	conn, _, err := dial("/ws/probe/enroll/v1", identity.Fingerprint, token)
	if err != nil {
		t.Fatal(err)
	}
	hubID, enrollmentID := uuid.NewString(), uuid.NewString()
	request, err := encodeFrame("enroll.request", 0, EnrollRequest{HubID: hubID, ProbeID: identity.ProbeID, EnrollmentID: enrollmentID, ProtocolMin: 1, ProtocolMax: 1, Capabilities: []string{"snapshot.v1"}, CredentialVersion: 1, Token: runtimeToken})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	// Do not use receipt arrival as the activation signal. Wait for the durable
	// binding, then drop the socket without processing its enrollment result.
	for {
		if _, valid, err := enrollment.AuthenticateRuntime(ctx, runtimeToken); err != nil {
			t.Fatal(err)
		} else if valid {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err := conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
	if conn, resp, err := dial("/ws/probe/enroll/v1", identity.Fingerprint, token); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		if conn != nil {
			_ = conn.CloseNow()
		}
		t.Fatalf("consumed token upgraded again: %v %v", resp, err)
	}
	if conn, resp, err := dial("/ws/probe/v1", identity.Fingerprint, token); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		if conn != nil {
			_ = conn.CloseNow()
		}
		t.Fatalf("enrollment token accepted at runtime: %v %v", resp, err)
	}
	runtime, _, err := dial("/ws/probe/v1", identity.Fingerprint, runtimeToken)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case bound := <-accepted:
		if bound.HubID != hubID || bound.ProbeID != identity.ProbeID || bound.EnrollmentID != enrollmentID {
			t.Fatal("runtime handler received untrusted binding")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := runtime.CloseNow(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/api/auth/login": 404, "/api/monitors": 404, "/api/probes": 404, "/": 404} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, server.URL+path, nil))
		if rec.Code != want {
			t.Fatalf("%s status %d, want %d", path, rec.Code, want)
		}
		body := rec.Body.Bytes()
		if bytes.Contains(body, []byte(token)) || bytes.Contains(body, []byte(runtimeToken)) {
			t.Fatal("credential exposed by HTTP view")
		}
	}
}

func TestEdgeBearerRejectsAlternateCredentialChannels(t *testing.T) {
	for name, mutate := range map[string]func(*http.Request){
		"plaintext":      func(r *http.Request) { r.TLS = nil },
		"old TLS":        func(r *http.Request) { r.TLS.Version = tls.VersionTLS12 },
		"query":          func(r *http.Request) { r.URL.RawQuery = "token=secret" },
		"empty query":    func(r *http.Request) { r.URL.ForceQuery = true },
		"encoded path":   func(r *http.Request) { r.URL.RawPath = "/ws/probe/%761" },
		"browser origin": func(r *http.Request) { r.Header.Set("Origin", "https://example.com") },
		"duplicate auth": func(r *http.Request) { r.Header.Add("Authorization", "Bearer other") },
		"no subprotocol": func(r *http.Request) { r.Header.Del("Sec-WebSocket-Protocol") },
		"mixed protocol": func(r *http.Request) { r.Header.Set("Sec-WebSocket-Protocol", "phoenix.probe.v1, foreign") },
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "https://localhost/ws/probe/v1", nil)
			r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
			r.Header.Set("Authorization", "Bearer phx_probe_secret")
			r.Header.Set("Sec-WebSocket-Protocol", "phoenix.probe.v1")
			mutate(r)
			if token, err := edgeBearer(r, "/ws/probe/v1"); err == nil || token != "" {
				t.Fatal("unsafe credential channel accepted")
			}
		})
	}
	if _, err := NewEdgeHTTPHandler(nil, nil, nil, nil); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("missing server dependencies: %v", err)
	}
}
