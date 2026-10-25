package bootstrap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type auditMonitors struct {
	ports.MonitorRepository
	list []*domain.Monitor
}

func (r auditMonitors) ListActive(context.Context) ([]*domain.Monitor, error) { return r.list, nil }

type auditTrust struct{ admins map[int64]bool }

func (a auditTrust) OutboundTrust(_ context.Context, owner int64) domain.OutboundTrust {
	if a.admins[owner] {
		return domain.OutboundTrustAdmin
	}
	return domain.OutboundTrustRestricted
}

func (a auditTrust) NotificationOutboundTrust(context.Context, *domain.Notification) domain.OutboundTrust {
	return domain.OutboundTrustRestricted
}

type auditLog struct {
	ports.Logger
	warns []string
}

func (l *auditLog) Warn(msg string, args ...any) {
	l.warns = append(l.warns, msg+" "+fmt.Sprintln(args...))
}
func (l *auditLog) Debug(string, ...any) {}

// The startup audit counts non-admin-owned monitors whose targets the guard
// will refuse, ignores admin-owned ones, and stays silent when nothing is
// affected.
func TestAuditRestrictedMonitorTargets(t *testing.T) {
	netguard.SetRestricted(netguard.NewPolicy(nil))
	t.Cleanup(func() { netguard.SetRestricted(nil) })
	mon := func(owner int64, typ string, cfg map[string]any) *domain.Monitor {
		return &domain.Monitor{UserID: owner, Type: typ, Active: true, Config: cfg}
	}
	trust := auditTrust{admins: map[int64]bool{1: true}}

	log := &auditLog{}
	auditRestrictedMonitorTargets(context.Background(), auditMonitors{list: []*domain.Monitor{
		mon(1, "http", map[string]any{"url": "http://10.0.0.5/health"}),                          // admin-owned: fine
		mon(2, "http", map[string]any{"url": "http://10.0.0.5/health"}),                          // blocked
		mon(2, "tcp", map[string]any{"hostname": "192.168.1.10"}),                                // blocked
		mon(0, "dns", map[string]any{"hostname": "example.com", "resolve_server": "10.96.0.10"}), // ownerless, resolver blocked
		mon(2, "http", map[string]any{"url": "https://1.1.1.1/"}),                                // public
		mon(2, "database", map[string]any{"hostname": "db.internal"}),                            // admin-only type
	}}, trust, log)
	if len(log.warns) != 1 {
		t.Fatalf("warnings = %v; want exactly one", log.warns)
	}
	for _, want := range []string{"blocked_monitors 3", "admin_only_type_monitors 1"} {
		if !strings.Contains(log.warns[0], want) {
			t.Fatalf("warning %q; want it to contain %q", log.warns[0], want)
		}
	}

	quiet := &auditLog{}
	auditRestrictedMonitorTargets(context.Background(), auditMonitors{list: []*domain.Monitor{
		mon(1, "http", map[string]any{"url": "http://10.0.0.5/health"}),
		mon(2, "http", map[string]any{"url": "https://1.1.1.1/"}),
	}}, trust, quiet)
	if len(quiet.warns) != 0 {
		t.Fatalf("warnings = %v; want none", quiet.warns)
	}
}
