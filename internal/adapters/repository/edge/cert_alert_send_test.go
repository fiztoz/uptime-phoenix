package edge

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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// certTarget serves HTTPS with a real self-signed certificate whose expiry is
// inside the 30-day paging window, so the whole path runs on genuine checker
// evidence rather than a fabricated metadata map.
func certTarget(t *testing.T, expiresIn time.Duration) (string, time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notBefore := time.Now().UTC().Add(-time.Hour)
	notAfter := time.Now().UTC().Add(expiresIn).Truncate(time.Second)
	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "phoenix-cert-paging", Organization: []string{"Uptime Phoenix Test"}},
		NotBefore:    notBefore, NotAfter: notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pair := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
	listener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
	})
	return "https://" + listener.Addr().String(), notAfter
}

// certCapturingSender records the exact alert context a provider would receive.
type certCapturingSender struct {
	alerts   []domain.AlertContext
	failOnce bool
}

func (s *certCapturingSender) Type() string                  { return "webhook" }
func (s *certCapturingSender) Validate(map[string]any) error { return nil }
func (s *certCapturingSender) Send(_ context.Context, _ map[string]any, alert domain.AlertContext) error {
	if s.failOnce {
		s.failOnce = false
		return &net.DNSError{Err: "simulated provider outage", IsTemporary: true}
	}
	s.alerts = append(s.alerts, alert)
	return nil
}

type certConfigReader struct{ config *domain.EdgeResolvedConfig }

func (r certConfigReader) Load(context.Context) (*domain.EdgeResolvedConfig, error) {
	return r.config, nil
}

// TestEdgeCertificatePagingSendsOnceAcrossRestart is the end-to-end effect test:
// a real HTTPS target with a near-expiry certificate produces exactly one
// provider alert through the durable source outbox, and a process restart plus
// further checks never re-send a delivered threshold.
func TestEdgeCertificatePagingSendsOnceAcrossRestart(t *testing.T) {
	s, dir := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	url, notAfter := certTarget(t, 5*24*time.Hour)
	c, a := certAssignment()
	a.Monitor.Config = map[string]any{"url": url, "tls_ignore": true}
	c.Assignments[0] = a
	// The provider lease is authorized against the accepted graph's exact metadata,
	// so the resolved config under test must carry what the store actually sealed.
	active, err := readActiveConfig(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	c.Metadata = active.Snapshot.ProbeConfigMetadata

	httpChecker, ok := checker.Get("http")
	if !ok {
		t.Fatal("HTTP checker unavailable")
	}
	result, err := httpChecker.Check(ctx, a.Monitor.Config)
	if err != nil || result.Status != domain.StatusUp || result.Metadata["tls_not_after"] == "" {
		t.Fatalf("real HTTPS fixture failed: %+v %v", result, err)
	}
	if _, err := services.NewEdgeRecordingService(s, s, nil).Record(ctx, c, a, result, time.Now().UTC()); err != nil {
		t.Fatalf("record against the accepted graph: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := certStoreReopen(t, dir)
	sender := &certCapturingSender{failOnce: true}
	delivery := services.NewEdgeDeliveryService(reopened, certConfigReader{c}, reopened, nil, func(kind string) (ports.NotificationSender, bool) {
		if kind != "webhook" {
			return nil, false
		}
		return sender, true
	})

	// The first attempt meets a transient provider failure. The delivered-threshold
	// cursor was already advanced at commit time, so the retry must reuse the same
	// durable intent instead of paging a second alert.
	if _, err := delivery.ProcessNext(ctx, testIdentity().ProbeID); err != nil {
		t.Fatalf("first delivery attempt: %v", err)
	}
	if len(sender.alerts) != 0 {
		t.Fatalf("a failed provider attempt reported success: %+v", sender.alerts)
	}
	if got := certCount(t, reopened, `SELECT COUNT(*) FROM edge_delivery_outbox WHERE status = 'retrying'`); got != 1 {
		t.Fatalf("transient failure did not leave one retrying intent: %d", got)
	}
	if got := certCount(t, reopened, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate'`); got != 1 {
		t.Fatalf("a provider retry opened extra certificate incidents: %d", got)
	}
	// Let the backoff elapse without sleeping on it.
	if _, err := reopened.db.ExecContext(ctx, `UPDATE edge_delivery_outbox SET available_at = ?`, time.Now().UTC().Add(-time.Hour).UnixMicro()); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.ProcessNext(ctx, testIdentity().ProbeID); err != nil {
		t.Fatalf("retry attempt: %v", err)
	}
	if len(sender.alerts) != 1 {
		t.Fatalf("certificate alert sent %d times, want exactly 1", len(sender.alerts))
	}
	alert := sender.alerts[0]
	if alert.EventKind != domain.AlertEventCertificateExpiry || alert.CertThreshold != 7 ||
		alert.CertNotAfter == nil || !alert.CertNotAfter.Equal(notAfter) ||
		alert.CertDaysRemaining < 4 || alert.CertDaysRemaining > 5 ||
		alert.ProbeID != testIdentity().ProbeID || alert.AssignmentGeneration != 1 || alert.MonitorID != 17 ||
		!strings.Contains(alert.Message, "threshold: 7 days") || !strings.Contains(alert.Message, "Phoenix") {
		t.Fatalf("provider received a malformed certificate alert: %+v", alert)
	}
	if alert.AlertScope != domain.AlertScopeMonitor || alert.DeliveryScope != domain.IncidentScopeRegional {
		t.Fatalf("certificate alert lost its regional scope: %+v", alert)
	}

	// The outcome travels as source-owned telemetry, not as hub provider work.
	batch, err := reopened.ReadReplayBatch(ctx, 0, 64, defaultMaxBatchBytes)
	if err != nil {
		t.Fatal(err)
	}
	var sawDeliveryResult bool
	for _, item := range batch.Items {
		if item.Kind == "delivery.result" && strings.Contains(string(item.Payload), `"certificate_expiry"`) {
			sawDeliveryResult = true
		}
	}
	if !sawDeliveryResult {
		t.Fatalf("certificate delivery outcome never became telemetry: %+v", batch.Items)
	}
	if got := certCount(t, reopened, `SELECT COUNT(*) FROM edge_delivery_outbox`); got != 1 {
		t.Fatalf("one threshold must create one durable intent, got %d", got)
	}

	// Further checks on the same certificate, across another restart, page nothing.
	for _, offset := range []time.Duration{2 * time.Minute, 4 * time.Minute} {
		check, err := httpChecker.Check(ctx, a.Monitor.Config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := services.NewEdgeRecordingService(reopened, reopened, nil).Record(ctx, c, a, check, time.Now().UTC().Add(offset)); err != nil {
			t.Fatal(err)
		}
		if _, err := delivery.ProcessNext(ctx, testIdentity().ProbeID); err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.alerts) != 1 {
		t.Fatalf("delivered threshold re-sent: %d alerts", len(sender.alerts))
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	third := certStoreReopen(t, dir)
	check, err := httpChecker.Check(ctx, a.Monitor.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.NewEdgeRecordingService(third, third, nil).Record(ctx, c, a, check, time.Now().UTC().Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := certCount(t, third, `SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'certificate' AND status = 'firing' AND certificate_threshold = 7`); got != 1 {
		t.Fatalf("cold start re-opened a delivered certificate incident: %d", got)
	}
	if got := certCount(t, third, `SELECT COUNT(*) FROM edge_delivery_outbox`); got != 1 {
		t.Fatalf("cold start duplicated provider work: %d", got)
	}
}
