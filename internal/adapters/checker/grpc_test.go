package checker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func grpcHealthServer(t *testing.T, status grpc_health_v1.HealthCheckResponse_ServingStatus) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("phoenix", status)
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String()
}

func TestGRPCChecker_Check_Up(t *testing.T) {
	result, err := (GRPCChecker{}).Check(context.Background(), map[string]any{
		"url":          grpcHealthServer(t, grpc_health_v1.HealthCheckResponse_SERVING),
		"service_name": "phoenix", "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp || !strings.Contains(result.Message, "SERVING") {
		t.Fatalf("result = %+v; want SERVING", result)
	}
}

func TestGRPCChecker_Check_Down(t *testing.T) {
	result, err := (GRPCChecker{}).Check(context.Background(), map[string]any{
		"url":          grpcHealthServer(t, grpc_health_v1.HealthCheckResponse_NOT_SERVING),
		"service_name": "phoenix", "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown || !strings.Contains(result.Message, "NOT_SERVING") {
		t.Fatalf("result = %+v; want NOT_SERVING", result)
	}
}

func TestGRPCChecker_Check_Timeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		time.Sleep(2 * time.Second)
	}()

	result, err := (GRPCChecker{}).Check(context.Background(), map[string]any{
		"url": listener.Addr().String(), "timeout": 1.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown {
		t.Fatalf("status = %s; want DOWN", result.Status)
	}
	message := strings.ToLower(result.Message)
	if !strings.Contains(message, "deadline") && !strings.Contains(message, "timeout") {
		t.Fatalf("message = %q; want timeout diagnostic", result.Message)
	}
}

// --- TLS transport credentials (issue #52) -----------------------------------

// grpcTestTLSCertificate returns a server certificate for the given hosts plus
// a trust pool holding its issuing CA — a real TLS identity for local checks.
func grpcTestTLSCertificate(t *testing.T, hosts ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "phoenix-checker-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "phoenix-checker-test-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			leafTemplate.IPAddresses = append(leafTemplate.IPAddresses, ip)
		} else {
			leafTemplate.DNSNames = append(leafTemplate.DNSNames, host)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, pool
}

func grpcHealthServerTLS(t *testing.T, status grpc_health_v1.HealthCheckResponse_ServingStatus, cert tls.Certificate) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
	healthServer := health.NewServer()
	healthServer.SetServingStatus("phoenix", status)
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String()
}

func TestGRPCChecker_Check_TLS_Up(t *testing.T) {
	// Regression for issue #52: a healthy TLS gRPC health server must report
	// UP through the actual checker (real handshake, real certificate check).
	cert, pool := grpcTestTLSCertificate(t, "127.0.0.1", "localhost")
	result, err := (GRPCChecker{tlsRootCAs: pool}).Check(context.Background(), map[string]any{
		"url":          grpcHealthServerTLS(t, grpc_health_v1.HealthCheckResponse_SERVING, cert),
		"service_name": "phoenix", "tls": true, "timeout": 3.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp || !strings.Contains(result.Message, "SERVING") {
		t.Fatalf("result = %+v; want SERVING over TLS", result)
	}
}

func TestGRPCChecker_Check_TLS_NotServing_Down(t *testing.T) {
	cert, pool := grpcTestTLSCertificate(t, "127.0.0.1", "localhost")
	result, err := (GRPCChecker{tlsRootCAs: pool}).Check(context.Background(), map[string]any{
		"url":          grpcHealthServerTLS(t, grpc_health_v1.HealthCheckResponse_NOT_SERVING, cert),
		"service_name": "phoenix", "tls": true, "timeout": 3.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown || !strings.Contains(result.Message, "NOT_SERVING") {
		t.Fatalf("result = %+v; want NOT_SERVING over TLS", result)
	}
}

func TestGRPCChecker_Check_TLS_UntrustedCertificate_Down(t *testing.T) {
	// The zero-value checker trusts system roots only: the local self-signed
	// chain must be rejected (issue #52: untrusted certificates report DOWN).
	cert, _ := grpcTestTLSCertificate(t, "127.0.0.1", "localhost")
	result, err := (GRPCChecker{}).Check(context.Background(), map[string]any{
		"url":          grpcHealthServerTLS(t, grpc_health_v1.HealthCheckResponse_SERVING, cert),
		"service_name": "phoenix", "tls": true, "timeout": 3.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown {
		t.Fatalf("result = %+v; want DOWN for untrusted certificate", result)
	}
	message := strings.ToLower(result.Message)
	if !strings.Contains(message, "certificate") && !strings.Contains(message, "x509") && !strings.Contains(message, "tls") {
		t.Fatalf("message = %q; want certificate verification diagnostic", result.Message)
	}
}

func TestGRPCChecker_Check_TLS_WrongServerName_Down(t *testing.T) {
	// Server-name validation: a certificate valid for a different name is
	// rejected even when its CA is trusted.
	cert, pool := grpcTestTLSCertificate(t, "grpc.example.test")
	result, err := (GRPCChecker{tlsRootCAs: pool}).Check(context.Background(), map[string]any{
		"url":          grpcHealthServerTLS(t, grpc_health_v1.HealthCheckResponse_SERVING, cert),
		"service_name": "phoenix", "tls": true, "timeout": 3.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown {
		t.Fatalf("result = %+v; want DOWN for server-name mismatch", result)
	}
}
