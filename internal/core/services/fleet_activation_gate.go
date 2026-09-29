package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Fleet activation errors. The HTTP layer maps ErrFleetNotAssignmentAware to a
// 409 (the request is well formed and would be accepted once the rollout
// completes) and ErrFleetReadinessUnavailable to a 503 (the hub cannot answer
// the readiness question at all, so it must not guess).
var (
	ErrFleetNotAssignmentAware   = errors.New("hub workers do not all enforce probe assignment ownership")
	ErrFleetReadinessUnavailable = errors.New("hub worker readiness is unavailable")
)

// maxNamedUnawareWorkers bounds how many offending identities one refusal names.
// The message reaches an API response and a log line; a large or badly rolled
// out fleet must not turn either into an unbounded string.
const maxNamedUnawareWorkers = 8

// FleetActivationGate refuses to hand a monitor to a remote probe while any live
// hub worker cannot be shown to enforce assignment ownership.
//
// This is verification matrix T34. Enforcement inside one process is not enough:
// a worker binary built before assignment ownership has no
// ExecutableByLocal filter and no local-execution predicate in its lease claim,
// so during a rolling upgrade it would keep checking a monitor that the fleet
// just made remote-only, writing local heartbeats that fight the regional
// evidence and can manufacture a false recovery. Such a worker cannot be patched
// retroactively, but it does claim monitor leases, and that makes it observable.
//
// The zero value is deliberately unusable for remote writes: with no readiness
// store attached, EnsureRemoteActivationAllowed fails closed rather than
// skipping the check. A gate that can be forgotten must fail loudly, never
// silently permit (AGENTS.md rule 7).
type FleetActivationGate struct {
	workers       ports.HubWorkerReadiness
	leaseLookback time.Duration
}

// NewFleetActivationGate binds the readiness store and the lease liveness window.
// leaseLookback must be at least the fleet's shard lease TTL: a shorter window
// lets a running unaware worker look dead and quietly defeats the gate.
func NewFleetActivationGate(workers ports.HubWorkerReadiness, leaseLookback time.Duration) FleetActivationGate {
	return FleetActivationGate{workers: workers, leaseLookback: leaseLookback}
}

// desiresRemoteExecution reports whether a desired set removes local execution.
// Any member other than the local probe means some probe outside this hub will
// run the monitor, which is the transition this gate guards. A local-only set
// keeps today's behavior and is never gated, so a default single-pod install
// with no probes configured cannot be blocked by fleet readiness.
func desiresRemoteExecution(probeIDs []string) bool {
	for _, id := range probeIDs {
		if id != domain.LocalProbeID {
			return true
		}
	}
	return false
}

// EnsureRemoteActivationAllowed authorizes a desired set that includes a remote
// probe member. It returns nil when every observable live worker attests the
// required assignment protocol, when the set is local-only, and never for any
// other reason.
//
// Scope: this is a point-in-time check at write time. It cannot stop an unaware
// worker that joins the fleet afterwards, and it cannot see a local-mode worker,
// which writes no lease. Both limits are documented on ports.HubWorkerReadiness.
func (g FleetActivationGate) EnsureRemoteActivationAllowed(ctx context.Context, probeIDs []string) error {
	if !desiresRemoteExecution(probeIDs) {
		return nil
	}
	if g.workers == nil || g.leaseLookback <= 0 {
		return fmt.Errorf("%w: remote assignment writes need a readiness store and a positive lease lookback",
			ErrFleetReadinessUnavailable)
	}
	unaware, err := g.workers.UnawareWorkers(ctx, ports.HubWorkerAssignmentProtocol, g.leaseLookback)
	if err != nil {
		// Fail closed. An unreadable readiness table is not evidence that the
		// fleet is safe; guessing here is exactly the false-success the gate
		// exists to prevent.
		return fmt.Errorf("%w: %w", ErrFleetReadinessUnavailable, err)
	}
	if len(unaware) == 0 {
		return nil
	}
	named, suffix := unaware, ""
	if len(named) > maxNamedUnawareWorkers {
		suffix = fmt.Sprintf(" (+%d more)", len(unaware)-maxNamedUnawareWorkers)
		named = named[:maxNamedUnawareWorkers]
	}
	return fmt.Errorf("%w: %s%s", ErrFleetNotAssignmentAware, strings.Join(named, ", "), suffix)
}
