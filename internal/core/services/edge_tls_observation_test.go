package services

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestEdgeRecordingTLSMetadata(t *testing.T) {
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.FixedZone("local", 7*3600))
	expires := at.Add(7*24*time.Hour + 123*time.Nanosecond)
	for _, status := range []domain.Status{domain.StatusUp, domain.StatusDown} {
		f, config, assignment := edgeServiceFixture()
		result := ports.CheckResult{Status: status, Metadata: map[string]string{"tls_not_after": expires.Format(time.RFC3339Nano), "tls_days_remaining": "7", "tls_issuer": strings.Repeat("界", 100), "private_key": "must not leave checker"}}
		got, err := NewEdgeRecordingService(f, f, nil).Record(t.Context(), config, assignment, result, at)
		if err != nil || got.TLS == nil || !got.TLS.NotAfter.Equal(expires) || got.TLS.NotAfter.Location() != time.UTC || got.TLS.DaysRemaining != 7 || len(got.TLS.Issuer) > 256 || !utf8.ValidString(got.TLS.Issuer) {
			t.Fatalf("TLS evidence lost or malformed: %+v %v", got.TLS, err)
		}
		if got.Status != status && got.Status != domain.StatusPending || len(f.records[0].DeliveryIntents) != 0 {
			t.Fatal("certificate evidence changed availability or paged")
		}
	}
	for _, metadata := range []map[string]string{nil, {"tls_days_remaining": "3"}, {"tls_not_after": "bad"}, {"tls_not_after": "0001-01-01T00:00:00Z"}} {
		if got := edgeTLSObservation(metadata, at); got != nil {
			t.Fatal("missing/invalid exact expiry manufactured certificate", got)
		}
	}
}
