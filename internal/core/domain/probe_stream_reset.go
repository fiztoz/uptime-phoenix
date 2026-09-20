package domain

import "time"

// ProbeStreamResetIssue identifies one explicit hub administrative decision.
// Exact retries retain all identifiers; changing any of them is a conflict.
type ProbeStreamResetIssue struct {
	ResetID, HubID, ProbeID, PreviousStreamID, StreamID string
}

// ValidProbeStreamResetIssue validates identifiers without granting permission.
func ValidProbeStreamResetIssue(i ProbeStreamResetIssue) bool {
	return configUUID(i.ResetID) && configUUID(i.HubID) && configUUID(i.ProbeID) && configUUID(i.PreviousStreamID) && configUUID(i.StreamID) && i.PreviousStreamID != i.StreamID
}

// ProbeStreamResetOperation records hub progress separately from enrollment.
// Source is an operator-supplied local receipt; only ConfirmedAt proves that the
// new stream has actually passed authenticated runtime admission at the hub.
type ProbeStreamResetOperation struct {
	Plan                     ProbeStreamResetPlan
	State                    string // prepared, awaiting_peer, complete
	Source                   *EdgeStreamResetRecord
	ActivatedAt, ConfirmedAt *time.Time
}

// ProbeStreamResetPlan is immutable operator input issued by the hub before the
// stopped source changes epoch. It contains no credential or private key.
type ProbeStreamResetPlan struct {
	ResetID, HubID, ProbeID, EnrollmentID   string
	PreviousStreamID, StreamID, Fingerprint string
	CredentialVersion, CertificateVersion   int64
	HubCommittedSeq, ConnectionGeneration   int64
	PreparedAt                              time.Time
}

// ValidProbeStreamResetPlan checks scope and bounds without granting authority.
func ValidProbeStreamResetPlan(p ProbeStreamResetPlan) bool {
	return configUUID(p.ResetID) && configUUID(p.HubID) && configUUID(p.ProbeID) && configUUID(p.EnrollmentID) && configUUID(p.PreviousStreamID) && configUUID(p.StreamID) && p.PreviousStreamID != p.StreamID && ValidKeyHash(p.Fingerprint) && p.CredentialVersion > 0 && p.CertificateVersion > 0 && p.HubCommittedSeq >= 0 && p.ConnectionGeneration > 0 && !p.PreparedAt.IsZero() && p.PreparedAt.Nanosecond()%1000 == 0
}

// EdgeStreamResetRecord authenticates an explicit discontinuity. Source records
// only available evidence; it does not assert that absent work never happened.
// The immutable bootstrap files remain bound to InitialStreamID.
type EdgeStreamResetRecord struct {
	Plan                  ProbeStreamResetPlan
	InitialStreamID       string
	Source                EdgeIdentity
	State                 string
	ReservedAt, AppliedAt time.Time
	ArchiveBytes          int64
	ArchiveSHA256         string
}

// ValidEdgeStreamResetRecord checks each durable phase's complete metadata.
func ValidEdgeStreamResetRecord(r EdgeStreamResetRecord) bool {
	p := r.Plan
	if !ValidProbeStreamResetPlan(p) || !configUUID(r.InitialStreamID) || !ValidEdgeIdentity(r.Source) || r.Source.HubID != p.HubID || r.Source.ProbeID != p.ProbeID || r.Source.StreamID != p.PreviousStreamID || r.Source.ConnectionGeneration >= p.ConnectionGeneration || r.ReservedAt.IsZero() || r.ReservedAt.Nanosecond()%1000 != 0 {
		return false
	}
	switch r.State {
	case "prepared":
		return r.AppliedAt.IsZero() && r.ArchiveBytes == 0 && r.ArchiveSHA256 == ""
	case "archived":
		return r.AppliedAt.IsZero() && r.ArchiveBytes > 0 && ValidKeyHash(r.ArchiveSHA256)
	case "applied":
		return !r.AppliedAt.IsZero() && r.AppliedAt.Nanosecond()%1000 == 0 && r.ArchiveBytes > 0 && ValidKeyHash(r.ArchiveSHA256)
	default:
		return false
	}
}
