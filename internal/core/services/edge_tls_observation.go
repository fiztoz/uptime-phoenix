package services

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func edgeTLSObservation(metadata map[string]string, at time.Time) *domain.TLSObservation {
	notAfter, days, issuer, ok := parseCertMetadata(metadata, at)
	if !ok {
		return nil
	}
	// Only the protocol's three public certificate fields leave the checker.
	// Bound a potentially long issuer without splitting UTF-8 or failing the
	// availability observation just because a certificate has a long DN.
	issuer = strings.ToValidUTF8(issuer, "\uFFFD")
	if len(issuer) > 256 {
		issuer = issuer[:256]
		for !utf8.ValidString(issuer) {
			issuer = issuer[:len(issuer)-1]
		}
	}
	evidence := &domain.TLSObservation{NotAfter: notAfter.UTC(), DaysRemaining: int64(days), Issuer: issuer}
	if !domain.ValidTLSObservation(evidence) {
		return nil
	}
	return evidence
}
