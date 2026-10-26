package domain

import "context"

// OutboundTrust says how far a user-configured outbound connection (a monitor
// check or a notification delivery) may reach. It follows the resource's
// owner: services.AccessService.OutboundTrust decides it, the scheduler and the
// notification services attach it to the context, and the network guard in
// internal/adapters/netguard enforces it on every dial.
type OutboundTrust uint8

const (
	// OutboundTrustUnset means nothing was attached. The guard treats it as
	// OutboundTrustRestricted, so a forgotten caller fails closed.
	OutboundTrustUnset OutboundTrust = iota
	// OutboundTrustRestricted applies to resources owned by a non-admin, by a
	// deleted user, or by an owner that could not be loaded: private, loopback,
	// CGNAT and unique-local destinations are refused unless the operator lists
	// them in OUTBOUND_ALLOWED_CIDRS, on top of the always-blocked floor.
	OutboundTrustRestricted
	// OutboundTrustAdmin applies to resources owned by an admin: only the
	// always-blocked floor (link-local, cloud metadata, multicast, unspecified)
	// applies.
	OutboundTrustAdmin
	// OutboundTrustProbe applies on a remote probe, which the operator deploys
	// into the network it monitors and which cannot see monitor ownership:
	// reach is the floor only, as for an admin, but response values are not
	// echoed into check messages, since the owner may not be an admin.
	OutboundTrustProbe
)

type outboundTrustKey struct{}

// WithOutboundTrust returns ctx carrying the outbound trust for the work it
// scopes.
func WithOutboundTrust(ctx context.Context, trust OutboundTrust) context.Context {
	return context.WithValue(ctx, outboundTrustKey{}, trust)
}

// OutboundTrustFrom returns the outbound trust attached to ctx, or
// OutboundTrustUnset.
func OutboundTrustFrom(ctx context.Context) OutboundTrust {
	if ctx == nil {
		return OutboundTrustUnset
	}
	trust, _ := ctx.Value(outboundTrustKey{}).(OutboundTrust)
	return trust
}
