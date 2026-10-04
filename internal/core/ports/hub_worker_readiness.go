package ports

import (
	"context"
	"time"
)

// HubWorkerAssignmentProtocol is the assignment-ownership contract version a hub
// worker enforces when it executes monitor checks. Version 1 means the worker
// resolves every candidate monitor through
// MonitorProbeAssignmentRepository.ExecutableByLocal (and, when claiming leases,
// through the same predicate in SQL) so it never runs a monitor whose active
// assignment set has no local member.
//
// Bump this only when the meaning of "honors assignment ownership" changes. A
// worker declaring a lower protocol than the hub requires is treated exactly
// like a worker that never declared: remote activation is refused.
const HubWorkerAssignmentProtocol = 1

// HubWorkerReadiness records which hub workers execute checks and the probe
// assignment protocol each one enforces. It exists so remote activation can fail
// closed during a mixed-version rollout (verification matrix T34): an older
// worker binary cannot be changed retroactively, but it does claim monitor
// leases, so a live lease with no matching attestation is positive proof of an
// executor that would ignore assignment ownership.
//
// Detection boundary, stated honestly: only sharded workers are enumerable,
// because only they own rows in monitors.worker_id. A local-mode worker writes
// no lease and is therefore invisible here; that topology is a single process
// upgraded atomically, so it has no mixed-version window. This readiness check
// is also a point-in-time gate evaluated when a remote assignment is written —
// it cannot stop an unaware worker that joins the fleet afterwards.
type HubWorkerReadiness interface {
	// DeclareWorker records or refreshes this worker's attestation, valid until
	// now+ttl. It is idempotent and prunes attestations whose lease has expired.
	DeclareWorker(ctx context.Context, workerID string, protocol int, ttl time.Duration) error
	// UnawareWorkers returns the distinct worker identities that are live but
	// cannot be shown to enforce requiredProtocol: live monitor-lease holders
	// without a current attestation at that protocol, plus workers whose current
	// attestation declares an older protocol. leaseLookback bounds how old a
	// monitor lease may be and still count as live; it must be at least the
	// fleet's shard lease TTL or a running old worker can look dead.
	//
	// An empty result means every observable executor honors assignment
	// ownership. It never means "no workers exist".
	UnawareWorkers(ctx context.Context, requiredProtocol int, leaseLookback time.Duration) ([]string, error)
}
