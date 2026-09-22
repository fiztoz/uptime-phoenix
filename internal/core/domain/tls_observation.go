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

// SameTLSObservation compares immutable source evidence without truncating expiry.
func SameTLSObservation(a, b *TLSObservation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.NotAfter.Equal(b.NotAfter) && a.DaysRemaining == b.DaysRemaining && a.Issuer == b.Issuer
}
