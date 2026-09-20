package probe_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
)

func generateTestCert(t *testing.T, notBefore, notAfter time.Time, dnsNames []string, ips []net.IP) (tls.Certificate, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "test-probe-hub",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	hash := sha256.Sum256(derBytes)
	fingerprint := hex.EncodeToString(hash[:])

	tlsCert := tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}

	return tlsCert, fingerprint
}

func TestPinnedClient_EndpointValidation(t *testing.T) {
	validFingerprint := strings.Repeat("a", 64)
	validPolicy := probe.EndpointPolicy{}

	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
		errMatch string
	}{
		{
			name:     "valid runtime endpoint",
			endpoint: "wss://hub.example.com/ws/probe/v1",
			wantErr:  false,
		},
		{
			name:     "valid enroll endpoint with custom port",
			endpoint: "wss://hub.example.com:8443/ws/probe/enroll/v1",
			wantErr:  false,
		},
		{
			name:     "valid IP endpoint",
			endpoint: "wss://127.0.0.1:8443/ws/probe/v1",
			wantErr:  false,
		},
		{
			name:     "valid IPv6 endpoint",
			endpoint: "wss://[::1]:8443/ws/probe/v1",
			wantErr:  false,
		},
		{
			name:     "reject https scheme",
			endpoint: "https://hub.example.com/ws/probe/v1",
			wantErr:  true,
			errMatch: "scheme must be wss",
		},
		{
			name:     "reject ws scheme",
			endpoint: "ws://hub.example.com/ws/probe/v1",
			wantErr:  true,
			errMatch: "scheme must be wss",
		},
		{
			name:     "reject userinfo",
			endpoint: "wss://user:pass@hub.example.com/ws/probe/v1",
			wantErr:  true,
			errMatch: "userinfo",
		},
		{
			name:     "reject query parameters",
			endpoint: "wss://hub.example.com/ws/probe/v1?token=secret",
			wantErr:  true,
			errMatch: "query",
		},
		{
			name:     "reject fragment",
			endpoint: "wss://hub.example.com/ws/probe/v1#hash",
			wantErr:  true,
			errMatch: "fragment",
		},
		{
			name:     "reject wrong path",
			endpoint: "wss://hub.example.com/ws/probe/v2",
			wantErr:  true,
			errMatch: "path must be",
		},
		{
			name:     "reject trailing slash",
			endpoint: "wss://hub.example.com/ws/probe/v1/",
			wantErr:  true,
			errMatch: "path must be",
		},
		{
			name:     "reject root path",
			endpoint: "wss://hub.example.com/",
			wantErr:  true,
			errMatch: "path must be",
		},
		{
			name:     "reject encoded path variant",
			endpoint: "wss://hub.example.com/ws%2fprobe/v1",
			wantErr:  true,
			errMatch: "encoded",
		},
		{
			name:     "reject port zero",
			endpoint: "wss://hub.example.com:0/ws/probe/v1",
			wantErr:  true,
			errMatch: "valid 16-bit integer",
		},
		{
			name:     "reject port overflow",
			endpoint: "wss://hub.example.com:70000/ws/probe/v1",
			wantErr:  true,
			errMatch: "valid 16-bit integer",
		},
		{
			name:     "reject invalid port string",
			endpoint: "wss://hub.example.com:abc/ws/probe/v1",
			wantErr:  true,
			errMatch: "malformed syntax",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, err := probe.NewPinnedHTTPClient(tc.endpoint, validFingerprint, validPolicy)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errMatch)
				}
				if !strings.Contains(err.Error(), tc.errMatch) {
					t.Fatalf("expected error containing %q, got %v", tc.errMatch, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if client == nil {
					t.Fatal("expected non-nil http.Client")
				}
			}
		})
	}

	t.Run("malformed url redacts secrets from error", func(t *testing.T) {
		fakeSecret := "secret-probe-token-to-redact"
		malformedURL := fmt.Sprintf("wss://admin:%s@bad-host-url%%\x00/ws/probe/v1", fakeSecret)
		_, err := probe.NewPinnedHTTPClient(malformedURL, validFingerprint, validPolicy)
		if err == nil {
			t.Fatal("expected error on malformed URL, got nil")
		}
		if strings.Contains(err.Error(), fakeSecret) {
			t.Fatalf("error exposed secret credentials: %v", err)
		}
		if !strings.Contains(err.Error(), "malformed syntax") {
			t.Fatalf("expected bounded malformed syntax error, got %v", err)
		}
	})
}

func TestPinnedClient_FingerprintValidation(t *testing.T) {
	validEndpoint := "wss://hub.example.com/ws/probe/v1"
	validPolicy := probe.EndpointPolicy{}

	tests := []struct {
		name        string
		fingerprint string
		wantErr     bool
		errMatch    string
	}{
		{
			name:        "valid 64-char lowercase hex",
			fingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			wantErr:     false,
		},
		{
			name:        "reject uppercase hex",
			fingerprint: "0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",
			wantErr:     true,
			errMatch:    "lowercase hexadecimal",
		},
		{
			name:        "reject non-hex characters",
			fingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeg",
			wantErr:     true,
			errMatch:    "lowercase hexadecimal",
		},
		{
			name:        "reject colons or separators",
			fingerprint: "01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef",
			wantErr:     true,
			errMatch:    "lowercase hexadecimal",
		},
		{
			name:        "reject short fingerprint",
			fingerprint: "0123456789abcdef",
			wantErr:     true,
			errMatch:    "exactly 64",
		},
		{
			name:        "reject empty fingerprint",
			fingerprint: "",
			wantErr:     true,
			errMatch:    "exactly 64",
		},
		{
			name:        "reject long fingerprint",
			fingerprint: strings.Repeat("a", 65),
			wantErr:     true,
			errMatch:    "exactly 64",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, err := probe.NewPinnedHTTPClient(validEndpoint, tc.fingerprint, validPolicy)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errMatch)
				}
				if !strings.Contains(err.Error(), tc.errMatch) {
					t.Fatalf("expected error containing %q, got %v", tc.errMatch, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if client == nil {
					t.Fatal("expected non-nil http.Client")
				}
			}
		})
	}
}

func TestPinnedClient_PolicyPrefixValidation(t *testing.T) {
	validEndpoint := "wss://hub.example.com/ws/probe/v1"
	validFingerprint := strings.Repeat("a", 64)

	t.Run("valid prefix", func(t *testing.T) {
		policy := probe.EndpointPolicy{
			AllowedCIDRs: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("127.0.0.1/32"),
			},
		}
		client, err := probe.NewPinnedHTTPClient(validEndpoint, validFingerprint, policy)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client == nil {
			t.Fatal("expected client")
		}
	})

	t.Run("invalid prefix fails", func(t *testing.T) {
		policy := probe.EndpointPolicy{
			AllowedCIDRs: []netip.Prefix{
				{}, // invalid zero value
			},
		}
		_, err := probe.NewPinnedHTTPClient(validEndpoint, validFingerprint, policy)
		if err == nil || !strings.Contains(err.Error(), "invalid allowed CIDR prefix") {
			t.Fatalf("expected invalid allowed CIDR prefix error, got %v", err)
		}
	})

	t.Run("caller policy mutation after construction does not broaden client authorization", func(t *testing.T) {
		cidrs := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
		policy := probe.EndpointPolicy{AllowedCIDRs: cidrs}

		client, err := probe.NewPinnedHTTPClient(validEndpoint, validFingerprint, policy)
		if err != nil {
			t.Fatal(err)
		}

		// Mutate original slice
		cidrs[0] = netip.MustParsePrefix("127.0.0.0/8")

		// The client's internal transport must still enforce the original policy (10.0.0.0/8), not 127.0.0.0/8
		// Verify by attempting connection to a loopback address: should fail
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://127.0.0.1:443/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Do(req)
		if err == nil {
			t.Fatal("expected failure after caller mutated external policy slice, got success")
		}
	})
}

func TestPinnedClient_DestinationPolicyUnit(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		policy  probe.EndpointPolicy
		wantErr bool
		errMsg  string
	}{
		// Unspecified
		{
			name:    "unspecified IPv4 forbidden",
			addr:    "0.0.0.0",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "unspecified",
		},
		{
			name:    "unspecified IPv6 forbidden",
			addr:    "::",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "unspecified",
		},
		{
			name:    "unspecified forbidden even with allowed CIDR",
			addr:    "0.0.0.0",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
			wantErr: true,
			errMsg:  "unspecified",
		},
		// Multicast
		{
			name:    "multicast IPv4 forbidden",
			addr:    "224.0.0.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "multicast",
		},
		{
			name:    "multicast IPv6 forbidden",
			addr:    "ff02::1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "multicast",
		},
		{
			name:    "multicast forbidden even with allowed CIDR",
			addr:    "224.0.0.1",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("224.0.0.0/4")}},
			wantErr: true,
			errMsg:  "multicast",
		},
		// Link-Local
		{
			name:    "link-local IPv4 forbidden",
			addr:    "169.254.1.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "link-local",
		},
		{
			name:    "link-local IPv6 forbidden",
			addr:    "fe80::1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "link-local",
		},
		{
			name:    "link-local forbidden even with allowed CIDR",
			addr:    "169.254.1.1",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}},
			wantErr: true,
			errMsg:  "link-local",
		},
		// Cloud Metadata
		{
			name:    "cloud metadata 169.254.169.254 forbidden",
			addr:    "169.254.169.254",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "link-local",
		},
		{
			name:    "cloud metadata IPv6 fd00:ec2::254 forbidden",
			addr:    "fd00:ec2::254",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("fd00::/8")}},
			wantErr: true,
			errMsg:  "cloud metadata",
		},
		{
			name:    "cloud metadata Alibaba 100.100.100.200 forbidden",
			addr:    "100.100.100.200",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}},
			wantErr: true,
			errMsg:  "cloud metadata",
		},
		// Mapped IPv4 addresses
		{
			name:    "mapped IPv4 loopback without CIDR forbidden",
			addr:    "::ffff:127.0.0.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "loopback",
		},
		{
			name:    "mapped IPv4 metadata forbidden",
			addr:    "::ffff:169.254.169.254",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}},
			wantErr: true,
			errMsg:  "link-local",
		},
		{
			name:    "mapped IPv4 private without CIDR forbidden",
			addr:    "::ffff:10.0.0.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "private",
		},
		{
			name:    "mapped IPv4 multicast forbidden",
			addr:    "::ffff:224.0.0.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "multicast",
		},
		{
			name:    "mapped IPv4 unspecified forbidden",
			addr:    "::ffff:0.0.0.0",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "unspecified",
		},
		// Without CIDRs: Private & Loopback forbidden
		{
			name:    "loopback 127.0.0.1 without CIDR forbidden",
			addr:    "127.0.0.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "loopback",
		},
		{
			name:    "loopback ::1 without CIDR forbidden",
			addr:    "::1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "loopback",
		},
		{
			name:    "private 10.0.0.1 without CIDR forbidden",
			addr:    "10.0.0.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "private",
		},
		{
			name:    "private 172.16.0.1 without CIDR forbidden",
			addr:    "172.16.0.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "private",
		},
		{
			name:    "private 192.168.1.1 without CIDR forbidden",
			addr:    "192.168.1.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "private",
		},
		{
			name:    "private fc00::1 without CIDR forbidden",
			addr:    "fc00::1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "private",
		},
		{
			name:    "CGNAT 100.64.0.1 without CIDR forbidden",
			addr:    "100.64.0.1",
			policy:  probe.EndpointPolicy{},
			wantErr: true,
			errMsg:  "carrier-grade NAT",
		},
		// Public unicast without CIDR permitted
		{
			name:    "public unicast IPv4 permitted without CIDR",
			addr:    "93.184.216.34",
			policy:  probe.EndpointPolicy{},
			wantErr: false,
		},
		{
			name:    "public unicast Cloudflare DNS permitted without CIDR",
			addr:    "1.1.1.1",
			policy:  probe.EndpointPolicy{},
			wantErr: false,
		},
		{
			name:    "public unicast IPv6 permitted without CIDR",
			addr:    "2606:2800:220:1:248:1893:25c8:1946",
			policy:  probe.EndpointPolicy{},
			wantErr: false,
		},
		// With explicit matching CIDR permitted
		{
			name:    "loopback 127.0.0.1 with matching CIDR permitted",
			addr:    "127.0.0.1",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}},
			wantErr: false,
		},
		{
			name:    "loopback ::1 with matching CIDR permitted",
			addr:    "::1",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("::1/128")}},
			wantErr: false,
		},
		{
			name:    "private 10.1.2.3 with matching CIDR permitted",
			addr:    "10.1.2.3",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
			wantErr: false,
		},
		{
			name:    "private 192.168.1.50 with non-matching CIDR forbidden",
			addr:    "192.168.1.50",
			policy:  probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
			wantErr: true,
			errMsg:  "does not match any allowed CIDR prefix",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.addr)
			err := probe.ValidateDestination(addr, tc.policy)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errMsg)
				}
				if !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("expected error containing %q, got %v", tc.errMsg, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestPinnedClient_RealTLSServerScenarios(t *testing.T) {
	now := time.Now().UTC()
	validCert, validFingerprint := generateTestCert(t, now.Add(-1*time.Hour), now.Add(1*time.Hour), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	_, otherFingerprint := generateTestCert(t, now.Add(-1*time.Hour), now.Add(1*time.Hour), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})

	expiredCert, expiredFingerprint := generateTestCert(t, now.Add(-2*time.Hour), now.Add(-1*time.Hour), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	futureCert, futureFingerprint := generateTestCert(t, now.Add(1*time.Hour), now.Add(2*time.Hour), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})

	loopbackPolicy := probe.EndpointPolicy{
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}

	t.Run("correct pin success with websocket dial", func(t *testing.T) {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ws/probe/v1" {
				http.NotFound(w, r)
				return
			}
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() {
				_ = c.Close(websocket.StatusNormalClosure, "done")
			}()
			// Echo one message
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			typ, msg, err := c.Read(ctx)
			if err == nil {
				_ = c.Write(ctx, typ, msg)
			}
		}))
		ts.TLS = &tls.Config{
			Certificates: []tls.Certificate{validCert},
			MinVersion:   tls.VersionTLS13,
		}
		ts.StartTLS()
		defer ts.Close()

		tsURL, err := url.Parse(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		endpoint := fmt.Sprintf("wss://%s/ws/probe/v1", tsURL.Host)

		client, err := probe.NewPinnedHTTPClient(endpoint, validFingerprint, loopbackPolicy)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		wsConn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
			HTTPClient: client,
		})
		if err != nil {
			t.Fatalf("websocket.Dial failed: %v", err)
		}
		defer func() {
			_ = wsConn.Close(websocket.StatusNormalClosure, "done")
		}()

		if err := wsConn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
			t.Fatalf("write msg: %v", err)
		}
		typ, msg, err := wsConn.Read(ctx)
		if err != nil {
			t.Fatalf("read msg: %v", err)
		}
		if typ != websocket.MessageText || string(msg) != "hello" {
			t.Fatalf("unexpected message: %s", string(msg))
		}
	})

	t.Run("wrong pin fails handshake", func(t *testing.T) {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = websocket.Accept(w, r, nil)
		}))
		ts.TLS = &tls.Config{
			Certificates: []tls.Certificate{validCert},
			MinVersion:   tls.VersionTLS13,
		}
		ts.StartTLS()
		defer ts.Close()

		tsURL, _ := url.Parse(ts.URL)
		endpoint := fmt.Sprintf("wss://%s/ws/probe/v1", tsURL.Host)

		client, err := probe.NewPinnedHTTPClient(endpoint, otherFingerprint, loopbackPolicy)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, _, err = websocket.Dial(ctx, endpoint, &websocket.DialOptions{
			HTTPClient: client,
		})
		if err == nil {
			t.Fatal("expected handshake failure on fingerprint mismatch, got success")
		}
		if !strings.Contains(err.Error(), "fingerprint mismatch") {
			t.Fatalf("expected fingerprint mismatch error, got %v", err)
		}
	})

	t.Run("expired certificate fails handshake", func(t *testing.T) {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		ts.TLS = &tls.Config{
			Certificates: []tls.Certificate{expiredCert},
			MinVersion:   tls.VersionTLS13,
		}
		ts.StartTLS()
		defer ts.Close()

		tsURL, _ := url.Parse(ts.URL)
		endpoint := fmt.Sprintf("wss://%s/ws/probe/v1", tsURL.Host)

		client, err := probe.NewPinnedHTTPClient(endpoint, expiredFingerprint, loopbackPolicy)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, _, err = websocket.Dial(ctx, endpoint, &websocket.DialOptions{
			HTTPClient: client,
		})
		if err == nil {
			t.Fatal("expected handshake failure on expired certificate, got success")
		}
		if !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expected expired certificate error, got %v", err)
		}
	})

	t.Run("not-yet-valid certificate fails handshake", func(t *testing.T) {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		ts.TLS = &tls.Config{
			Certificates: []tls.Certificate{futureCert},
			MinVersion:   tls.VersionTLS13,
		}
		ts.StartTLS()
		defer ts.Close()

		tsURL, _ := url.Parse(ts.URL)
		endpoint := fmt.Sprintf("wss://%s/ws/probe/v1", tsURL.Host)

		client, err := probe.NewPinnedHTTPClient(endpoint, futureFingerprint, loopbackPolicy)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, _, err = websocket.Dial(ctx, endpoint, &websocket.DialOptions{
			HTTPClient: client,
		})
		if err == nil {
			t.Fatal("expected handshake failure on future certificate, got success")
		}
		if !strings.Contains(err.Error(), "not yet valid") {
			t.Fatalf("expected not yet valid certificate error, got %v", err)
		}
	})

	t.Run("TLS 1.2 refusal", func(t *testing.T) {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		ts.TLS = &tls.Config{
			Certificates: []tls.Certificate{validCert},
			MinVersion:   tls.VersionTLS12,
			MaxVersion:   tls.VersionTLS12,
		}
		ts.StartTLS()
		defer ts.Close()

		tsURL, _ := url.Parse(ts.URL)
		endpoint := fmt.Sprintf("wss://%s/ws/probe/v1", tsURL.Host)

		client, err := probe.NewPinnedHTTPClient(endpoint, validFingerprint, loopbackPolicy)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, _, err = websocket.Dial(ctx, endpoint, &websocket.DialOptions{
			HTTPClient: client,
		})
		if err == nil {
			t.Fatal("expected handshake failure on TLS 1.2, got success")
		}
		// Go's TLS client produces "protocol version" error when version cannot be negotiated
		if !strings.Contains(err.Error(), "protocol version") && !strings.Contains(err.Error(), "handshake") {
			t.Fatalf("expected TLS protocol version error, got %v", err)
		}
	})

	t.Run("redirect rejection", func(t *testing.T) {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/other", http.StatusFound)
		}))
		ts.TLS = &tls.Config{
			Certificates: []tls.Certificate{validCert},
			MinVersion:   tls.VersionTLS13,
		}
		ts.StartTLS()
		defer ts.Close()

		tsURL, _ := url.Parse(ts.URL)
		endpoint := fmt.Sprintf("wss://%s/ws/probe/v1", tsURL.Host)

		client, err := probe.NewPinnedHTTPClient(endpoint, validFingerprint, loopbackPolicy)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+tsURL.Host+"/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err == nil {
			t.Fatal("expected redirect error, got success")
		}
		if !strings.Contains(err.Error(), "redirects are not allowed") {
			t.Fatalf("expected 'redirects are not allowed' error, got %v", err)
		}
	})

	t.Run("caller cancellation honors context", func(t *testing.T) {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
		}))
		ts.TLS = &tls.Config{
			Certificates: []tls.Certificate{validCert},
			MinVersion:   tls.VersionTLS13,
		}
		ts.StartTLS()
		defer ts.Close()

		tsURL, _ := url.Parse(ts.URL)
		endpoint := fmt.Sprintf("wss://%s/ws/probe/v1", tsURL.Host)

		client, err := probe.NewPinnedHTTPClient(endpoint, validFingerprint, loopbackPolicy)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+tsURL.Host+"/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err == nil {
			t.Fatal("expected cancellation error, got success")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("expected context.Canceled error, got %v", err)
		}
	})

	t.Run("loopback connection fails without explicit CIDR", func(t *testing.T) {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		ts.TLS = &tls.Config{
			Certificates: []tls.Certificate{validCert},
			MinVersion:   tls.VersionTLS13,
		}
		ts.StartTLS()
		defer ts.Close()

		tsURL, _ := url.Parse(ts.URL)
		endpoint := fmt.Sprintf("wss://%s/ws/probe/v1", tsURL.Host)

		// Policy with no CIDRs -> loopback should be rejected
		client, err := probe.NewPinnedHTTPClient(endpoint, validFingerprint, probe.EndpointPolicy{})
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+tsURL.Host+"/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err == nil {
			t.Fatal("expected destination policy rejection for loopback without CIDR, got success")
		}
		if !strings.Contains(err.Error(), "loopback destination") && !strings.Contains(err.Error(), "requires an explicit matching CIDR") {
			t.Fatalf("expected loopback policy error, got %v", err)
		}
	})

	t.Run("client reuse prevention across host port and path", func(t *testing.T) {
		endpoint := "wss://127.0.0.1:8443/ws/probe/v1"
		client, err := probe.NewPinnedHTTPClient(endpoint, validFingerprint, loopbackPolicy)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}

		// 1. Different host
		reqDiffHost, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://127.0.0.2:8443/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		reqDiffHost.Header.Set("Authorization", "Bearer secret-token")
		_, err = client.Do(reqDiffHost)
		if err == nil || !strings.Contains(err.Error(), "does not match pinned endpoint host") {
			t.Fatalf("expected different host rejection, got %v", err)
		}

		// 2. Different port
		reqDiffPort, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://127.0.0.1:9443/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		reqDiffPort.Header.Set("Authorization", "Bearer secret-token")
		_, err = client.Do(reqDiffPort)
		if err == nil || !strings.Contains(err.Error(), "does not match pinned endpoint port") {
			t.Fatalf("expected different port rejection, got %v", err)
		}

		// 3. Different path
		reqDiffPath, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://127.0.0.1:8443/ws/probe/enroll/v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		reqDiffPath.Header.Set("Authorization", "Bearer secret-token")
		_, err = client.Do(reqDiffPath)
		if err == nil || !strings.Contains(err.Error(), "does not match pinned endpoint path") {
			t.Fatalf("expected different path rejection, got %v", err)
		}

		// 4. Request with query
		reqQuery, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://127.0.0.1:8443/ws/probe/v1?token=leak", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Do(reqQuery)
		if err == nil || !strings.Contains(err.Error(), "query parameters") {
			t.Fatalf("expected query parameter rejection, got %v", err)
		}

		// 5. Request with userinfo
		reqUserinfo, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://127.0.0.1:8443/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		reqUserinfo.URL.User = url.UserPassword("user", "pass")
		_, err = client.Do(reqUserinfo)
		if err == nil || !strings.Contains(err.Error(), "userinfo") {
			t.Fatalf("expected userinfo rejection, got %v", err)
		}

		// 6. Request with encoded path
		reqEncodedPath, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://127.0.0.1:8443/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		reqEncodedPath.URL.RawPath = "/ws%2fprobe/v1"
		_, err = client.Do(reqEncodedPath)
		if err == nil || !strings.Contains(err.Error(), "encoded path variants") {
			t.Fatalf("expected encoded path rejection, got %v", err)
		}

		// 7. Request with conflicting Host header
		reqConflictingHost, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://127.0.0.1:8443/ws/probe/v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		reqConflictingHost.Host = "attacker.example.com"
		_, err = client.Do(reqConflictingHost)
		if err == nil || !strings.Contains(err.Error(), "Host header does not match") {
			t.Fatalf("expected conflicting Host rejection, got %v", err)
		}
	})
}
