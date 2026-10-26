package scheduler

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// ownerTrust resolves the outbound trust of each scheduled check's owner
// (services.AccessService.OutboundTrust) once per owner per scheduling pass.
// AccessService caches users, so a pass costs at most one user lookup per
// owner per cache TTL, never one per check. The trust travels on the check's
// context (domain.WithOutboundTrust) to the network guard in every checker.
//
// A nil resolver attaches nothing, which the guard treats as restricted:
// production wiring (internal/bootstrap) always sets one.
type ownerTrust struct {
	resolver services.OutboundTrustResolver
	memo     map[int64]domain.OutboundTrust
}

func newOwnerTrust(resolver services.OutboundTrustResolver) *ownerTrust {
	return &ownerTrust{resolver: resolver, memo: map[int64]domain.OutboundTrust{}}
}

func (o *ownerTrust) of(ctx context.Context, m *domain.Monitor) domain.OutboundTrust {
	if o == nil || o.resolver == nil || m == nil {
		return domain.OutboundTrustUnset
	}
	if trust, ok := o.memo[m.UserID]; ok {
		return trust
	}
	trust := o.resolver.OutboundTrust(ctx, m.UserID)
	o.memo[m.UserID] = trust
	return trust
}
