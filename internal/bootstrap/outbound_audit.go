package bootstrap

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const (
	// outboundAuditMaxLookups bounds the DNS lookups one startup audit makes,
	// so a large install does not send a burst of queries to its resolver.
	outboundAuditMaxLookups = 100
	outboundAuditLookupWait = 2 * time.Second
	outboundAuditBudget     = time.Minute
)

// outboundAuditUnguardedTypes are admin-only monitor types whose connections
// the outbound guard does not cover; a non-admin-owned one predates the rule.
var outboundAuditUnguardedTypes = map[string]bool{"database": true, "docker": true}

// auditRestrictedMonitorTargets logs one warning at startup when active
// monitors owned by a non-admin (or by a deleted user) will now be refused by
// the outbound guard (#88), so an operator who upgrades sees why those
// monitors went DOWN and can allowlist the ranges with OUTBOUND_ALLOWED_CIDRS
// or have an admin own them. It is best effort: names are resolved
// sequentially, at most outboundAuditMaxLookups of them, and anything it
// cannot resolve is counted, not guessed.
func auditRestrictedMonitorTargets(ctx context.Context, monitors ports.MonitorRepository, trust services.OutboundTrustResolver, log ports.Logger) {
	ctx, cancel := context.WithTimeout(ctx, outboundAuditBudget)
	defer cancel()
	list, err := monitors.ListActive(ctx)
	if err != nil {
		log.Debug("outbound audit: list monitors failed", "error", err)
		return
	}
	policy := netguard.Restricted()
	owners := map[int64]domain.OutboundTrust{}
	var blocked, unguarded, unresolved, lookups int
	for _, m := range list {
		t, seen := owners[m.UserID]
		if !seen {
			t = trust.OutboundTrust(ctx, m.UserID)
			owners[m.UserID] = t
		}
		if t == domain.OutboundTrustAdmin {
			continue
		}
		if outboundAuditUnguardedTypes[m.Type] {
			unguarded++
			continue
		}
		host := outboundAuditHost(m)
		if host == "" {
			continue
		}
		var addrs []netip.Addr
		if addr, err := netip.ParseAddr(host); err == nil {
			addrs = []netip.Addr{addr}
		} else {
			if lookups >= outboundAuditMaxLookups || ctx.Err() != nil {
				unresolved++
				continue
			}
			lookups++
			lookupCtx, lookupCancel := context.WithTimeout(ctx, outboundAuditLookupWait)
			addrs, err = net.DefaultResolver.LookupNetIP(lookupCtx, "ip", host)
			lookupCancel()
			if err != nil {
				unresolved++
				continue
			}
		}
		for _, addr := range addrs {
			if policy.Check(addr) != nil {
				blocked++
				break
			}
		}
	}
	if blocked == 0 && unguarded == 0 {
		return
	}
	log.Warn("outbound guard: some monitors owned by non-admin users target destinations their owner may not reach; their checks fail with 'destination address is not allowed' until an operator lists the ranges in "+netguard.EnvAllowedCIDRs+" or an admin owns the monitors (#88)",
		"blocked_monitors", blocked,
		"admin_only_type_monitors", unguarded,
		"unresolved_targets", unresolved,
		"allowlist_entries", netguard.AllowlistSize())
}

// outboundAuditHost returns the host a monitor's check dials, or "".
func outboundAuditHost(m *domain.Monitor) string {
	target := m.Target()
	switch m.Type {
	case "push":
		return ""
	case "dns":
		// A DNS check dials its resolver, not the name it asks about.
		target, _ = m.Config["resolve_server"].(string)
		if strings.TrimSpace(target) == "" {
			target = "8.8.8.8"
		}
	case "s3":
		target, _ = m.Config["endpoint"].(string)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return ""
		}
		return u.Hostname()
	}
	if host, _, err := net.SplitHostPort(target); err == nil {
		return host
	}
	return strings.Trim(target, "[]")
}
