package domain

import "time"

// AggregatePagingAction is an availability-only decision, never provider authorization.
type AggregatePagingAction string

const (
	AggregatePagingHold       AggregatePagingAction = "hold"
	AggregatePagingOpen       AggregatePagingAction = "open"
	AggregatePagingRecover    AggregatePagingAction = "recover"
	AggregatePagingAdminClose AggregatePagingAction = "admin_close"
)

// AggregatePagingIncident captures the configuration that opened an aggregate incident.
// Identity, deduplication, delivery intents and fencing belong to future durable storage.
type AggregatePagingIncident struct {
	AssignmentRevision int64
	Policy             HealthPolicy
	DeliveryMode       AlertDelivery
}

// AggregatePagingInput is an already-selected complete current assignment snapshot.
// The caller must select accepted generations and streams before evaluation. Revision
// changes include membership, policy and delivery mode changes. PolicyEffectiveAt is
// their latest effective boundary, not merely the last change of the ANY/ALL policy.
// Current snapshots must be evaluated at SnapshotAt == Now; a caller must reconstruct
// a current snapshot rather than relabel a cached or historical projection as current.
// Evidence observed on or before the boundary cannot establish a paging transition.
// A false Current marks historical reconstruction and permits no incident transition.
type AggregatePagingInput struct {
	Now                time.Time
	SnapshotAt         time.Time
	Current            bool
	Policy             HealthPolicy
	DeliveryMode       AlertDelivery
	AssignmentRevision int64
	PolicyEffectiveAt  time.Time
	Evidence           []RegionalHealthEvidence
	OpenIncident       *AggregatePagingIncident
}

// AggregatePagingDecision is a pure proposal; callers must never send from it directly.
// Health is evaluated after invalidating evidence from before the configuration boundary.
// AdministrativeReason is populated only for an administrative closure.
type AggregatePagingDecision struct {
	Health               MonitorHealth
	Action               AggregatePagingAction
	AdministrativeReason string
}
