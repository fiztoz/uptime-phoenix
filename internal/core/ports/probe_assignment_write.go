package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeAssignmentWriter commits complete desired assignment sets under
// optimistic revision control. It is the live-operator write surface: every
// member must be a currently enabled registration (backup Restore deliberately
// relaxes that and is not a substitute).
//
// ReplaceWithBindings replaces one complete set. Nil bindings preserve bindings
// on retained members and supply none for new ones; an explicit list (possibly
// empty) replaces all bindings. ErrConflict reports a stale expected revision.
//
// CreateMonitorWithAssignments inserts a monitor together with its complete
// initial desired set in one transaction, so creation can never leave a
// different set behind. It allocates the monitor ID and returns the created
// set at revision one.
type ProbeAssignmentWriter interface {
	ReplaceWithBindings(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error)
	CreateMonitorWithAssignments(ctx context.Context, m *domain.Monitor, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error)
}

// ProbeAssignmentCapabilities reports the compiled checker inventory so a
// desired assignment can be rejected before commit instead of failing silently
// at publication. It answers hub-side facts only: whether a monitor type can
// execute remotely at all and whether its checker dials a configured outbound
// proxy. Remote probe advertisements are runtime evidence and are enforced at
// publication and activation, never guessed here.
type ProbeAssignmentCapabilities interface {
	RemoteCapable(monitorType string) bool
	ProxyCapable(monitorType string) bool
}
