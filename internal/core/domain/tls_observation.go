package domain

import "time"

// TLSObservation is sanitized certificate evidence from one check, not alert state.
// NotAfter retains the exact certificate expiry; it is never inferred from days.
type TLSObservation struct {
	NotAfter      time.Time
	DaysRemaining int64
	Issuer        string
}

// ValidTLSObservation accepts absent evidence or the bounded V1 certificate shape.
func ValidTLSObservation(tls *TLSObservation) bool {
	return tls == nil || !tls.NotAfter.IsZero() && tls.NotAfter.Year() >= 1 && tls.NotAfter.Year() <= 9999 && len(tls.Issuer) <= 256
}

// CertificateThresholds are the fixed, non-configurable expiry paging thresholds
// shared by the local and remote certificate alert paths.
var CertificateThresholds = []int{30, 14, 7}

// ValidCertificateSubjectIdentity enforces the V1 certificate incident subject:
// one fixed threshold plus the exact UTC expiry the threshold was crossed for.
// Rounded remaining days never substitute for the expiry instant.
func ValidCertificateSubjectIdentity(threshold int64, notAfter *time.Time) bool {
	if notAfter == nil || notAfter.IsZero() {
		return false
	}
	for _, candidate := range CertificateThresholds {
		if threshold == int64(candidate) {
			return true
		}
	}
	return false
}

// MostUrgentCertificateThreshold returns the tightest fixed threshold that
// daysRemaining has entered. days=20 -> 30; days=13 -> 14; days=5 -> 7; days=45
// -> false. Negative days (an already-expired certificate) take the tightest.
func MostUrgentCertificateThreshold(daysRemaining int64) (int, bool) {
	for _, threshold := range []int{7, 14, 30} {
		if daysRemaining <= int64(threshold) {
			return threshold, true
		}
	}
	return 0, false
}

// SameTLSObservation compares immutable source evidence without truncating expiry.
func SameTLSObservation(a, b *TLSObservation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.NotAfter.Equal(b.NotAfter) && a.DaysRemaining == b.DaysRemaining && a.Issuer == b.Issuer
}
