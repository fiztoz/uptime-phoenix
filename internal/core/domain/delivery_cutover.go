package domain

import "time"

// DeliveryCutoverState describes a pure evidence proposal, never mode activation.
type DeliveryCutoverState string

const (
	DeliveryCutoverRegional         DeliveryCutoverState = "regional"
	DeliveryCutoverPending          DeliveryCutoverState = "pending"
	DeliveryCutoverEvidenceComplete DeliveryCutoverState = "evidence_complete"
)

// DeliveryCutoverReason is a bounded missing/mismatched-evidence diagnostic.
type DeliveryCutoverReason string

const (
	DeliveryCutoverNoAssignments          DeliveryCutoverReason = "no_assignments"
	DeliveryCutoverHistoricalSnapshot     DeliveryCutoverReason = "historical_snapshot"
	DeliveryCutoverSourceUnreported       DeliveryCutoverReason = "source_unreported"
	DeliveryCutoverApplicationUnsupported DeliveryCutoverReason = "mode_application_unsupported"
	DeliveryCutoverConfigUnreported       DeliveryCutoverReason = "config_unreported"
	DeliveryCutoverConfigUnverified       DeliveryCutoverReason = "config_unverified"
	// AssignmentMismatch includes a document with no entry for this monitor,
	// as well as a conflicting monitor/generation: neither covers this member.
	DeliveryCutoverAssignmentMismatch         DeliveryCutoverReason = "assignment_mismatch"
	DeliveryCutoverAssignmentRevisionMismatch DeliveryCutoverReason = "assignment_revision_mismatch"
	DeliveryCutoverDocumentModeMismatch       DeliveryCutoverReason = "document_mode_mismatch"
	DeliveryCutoverApplicationUnreported      DeliveryCutoverReason = "mode_application_unreported"
	DeliveryCutoverApplicationMismatch        DeliveryCutoverReason = "mode_application_mismatch"
)

// ProbeDeliveryModeApplication is separate from a config.applied receipt. The
// edge and protected hub-local runtime store source-local effective REGIONAL
// facts for exact bound documents; these are not authenticated cross-source
// reports or cutover authority.
// Optional authenticated regional-only reporting preserves these bindings, but
// supplies no aggregate application or cutover support. A future lifecycle must
// supply trusted evidence for the selected mode and accepted current stream.
// AssignmentRevision and ConfigRevision belong to different identity spaces.
// Hashes are internal identity only and never returned by the decision or HTTP.
type ProbeDeliveryModeApplication struct {
	ProbeConfigTarget
	MonitorID          int64
	AssignmentRevision int64
	Generation         int64
	ConfigRevision     int64
	SHA256             string
	StreamID           string
	AlertDelivery      AlertDelivery
}

// ValidProbeDeliveryModeApplication validates shape only, not authenticity,
// provenance, a committed barrier, lease ownership or runtime behavior.
func ValidProbeDeliveryModeApplication(a ProbeDeliveryModeApplication) bool {
	return ValidProbeConfigTarget(a.ProbeConfigTarget) && a.MonitorID > 0 &&
		a.AssignmentRevision > 0 && a.Generation > 0 && a.ConfigRevision > 0 &&
		len(a.SHA256) == 64 && configHex(a.SHA256) && configUUID(a.StreamID) &&
		ValidAlertDelivery(a.AlertDelivery)
}

// DeliveryCutoverSource contains already-authenticated, point-in-time evidence.
// AcknowledgedConfig must come from a coherent active pointer, matching durable
// receipt and validated exact retained document, never a latest prepared row.
// Assignment is this monitor's facts from that same document. StreamID is the
// caller's accepted current stream, not a stream selected by the attestation.
// ModeApplicationSupported is stronger than understanding optional desired mode:
// today's paging.aggregate.v1, source-local regional facts and the regional-only
// reporting capability must never populate it. Aggregate application negotiation
// has no producer yet.
// These inputs cannot establish current database authority on their own.
type DeliveryCutoverSource struct {
	ProbeID                  string
	StreamID                 string
	ModeApplicationSupported bool
	AcknowledgedConfig       *ProbeActiveConfig
	Assignment               *AcknowledgedAssignmentDelivery
	ModeApplication          *ProbeDeliveryModeApplication
}

// DeliveryCutoverInput is one complete assignment set selected at a captured
// instant. Historical evidence cannot complete a cutover proposal. Membership
// includes every assigned source, including paused or currently offline sources.
// HubID and source streams are trusted caller inputs, not source claims.
type DeliveryCutoverInput struct {
	Now         time.Time
	SnapshotAt  time.Time
	Current     bool
	HubID       string
	Assignments MonitorProbeAssignments
	Sources     []DeliveryCutoverSource
}

// DeliveryCutoverSourceDecision reveals only source identity and bounded codes.
type DeliveryCutoverSourceDecision struct {
	ProbeID string
	State   DeliveryCutoverState
	Reason  DeliveryCutoverReason
}

// DeliveryCutoverDecision is an evidence proposal, not readiness or permission.
// Even EvidenceComplete requires a future committed barrier and transactional
// assignment/source/owner fencing before any enqueue or final send decision.
// Both always retains regional paging; this object has no suppression/send flag.
type DeliveryCutoverDecision struct {
	State       DeliveryCutoverState
	DesiredMode AlertDelivery
	Reason      DeliveryCutoverReason
	Sources     []DeliveryCutoverSourceDecision
}
