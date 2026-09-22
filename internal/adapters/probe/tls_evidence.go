package probe

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func wireTLS(source *domain.TLSObservation) *TLSObservation {
	if source == nil {
		return nil
	}
	return &TLSObservation{NotAfter: Timestamp(source.NotAfter.UTC()), DaysRemaining: source.DaysRemaining, Issuer: source.Issuer}
}

func sourceTLS(wire *TLSObservation) *domain.TLSObservation {
	if wire == nil {
		return nil
	}
	return &domain.TLSObservation{NotAfter: time.Time(wire.NotAfter).UTC(), DaysRemaining: wire.DaysRemaining, Issuer: wire.Issuer}
}
