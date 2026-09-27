package services

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Diagnostic vocabulary (protocol section 7). Empty string always means
// unreported: null on the wire, never online, applied, ready or failed.
// The reserved values "revoked" (connection and execution) and "failed"
// (enrollment) require the durable operation evidence of a later slice and
// are deliberately not synthesized here.
const (
	ProbeEnrollmentUnconfigured = "unconfigured"
	ProbeEnrollmentPending      = "pending"
	ProbeEnrollmentActive       = "active"

	ProbeConnectionNeverConnected = "never_connected"
	ProbeConnectionOnline         = "online"
	ProbeConnectionSuspect        = "suspect"
	ProbeConnectionDisconnected   = "disconnected"

	ProbeExecutionUnconfigured = "unconfigured"
	ProbeExecutionReady        = "ready"
	ProbeExecutionDegraded     = "degraded"
	ProbeExecutionPaused       = "paused"

	ProbeConfigSyncPending  = "pending"
	ProbeConfigSyncApplied  = "applied"
	ProbeConfigSyncRejected = "rejected"
)

// ProbeDiagnosticSummary is the derived, bounded diagnostic vocabulary for one
// probe. Revision fields are zero when that evidence source is unreported.
// Connection health, execution readiness and config synchronization are
// derived independently; none of them reports target availability, which
// belongs to monitor health.
type ProbeDiagnosticSummary struct {
	EnrollmentState       string
	ConnectionStatus      string
	ExecutionStatus       string
	ConfigSyncStatus      string
	DesiredConfigRevision int64
	AppliedConfigRevision int64
	LastSeenAt            *time.Time
}

// ProbeDiagnosticsSummary derives one summary from stored nonsecret evidence.
// at is the current UTC instant; lease liveness is observational display state
// and never lease authority.
//
// The rules are deliberately evidence-bound:
//   - enrollment: unconfigured without a stored connection, then prepared is
//     pending and active is active;
//   - connection: never_connected without any session row, suspect only while a
//     live connected session's watchdog reports suspect/lost, online only for a
//     live connected session, otherwise disconnected. A paused registration
//     keeps its stored connection truth; it is not a disconnect;
//   - execution: paused for a disabled registration, unconfigured without
//     enrollment, ready only while the runtime lease is held, otherwise
//     degraded. The local row is the hub scheduler itself and is ready while
//     enabled;
//   - config sync: unreported without publication, applied only when the
//     receipt's revision AND SHA256 match the publication (equal revision
//     counters alone never prove application, and the hash binds the embedded
//     assignment generations), rejected for conflicting receipt evidence at the
//     newest revision, otherwise pending;
//   - last_seen is the latest source-reported time (watchdog checkpoint or
//     application receipt), never a hub-side guess.
func ProbeDiagnosticsSummary(at time.Time, d domain.ProbeDiagnostics) ProbeDiagnosticSummary {
	at = at.UTC()
	var summary ProbeDiagnosticSummary
	if d.Publication != nil {
		summary.DesiredConfigRevision = d.Publication.Revision
	}
	if d.Applied != nil {
		summary.AppliedConfigRevision = d.Applied.Revision
	}
	summary.ConfigSyncStatus = probeConfigSyncStatus(d)
	summary.LastSeenAt = probeLastSeenAt(d)

	local := d.Registration.ID == domain.LocalProbeID || d.Registration.Kind == domain.ProbeKindLocal
	if local {
		// The hub scheduler owns local execution: it is enrolled in itself,
		// connected to itself, and ready while enabled. There is no remote
		// session or runtime lease for the reserved local row.
		summary.EnrollmentState = ProbeEnrollmentActive
		summary.ConnectionStatus = ProbeConnectionOnline
		if d.Registration.Enabled {
			summary.ExecutionStatus = ProbeExecutionReady
		} else {
			summary.ExecutionStatus = ProbeExecutionPaused
		}
		return summary
	}

	switch {
	case d.Enrollment == nil:
		summary.EnrollmentState = ProbeEnrollmentUnconfigured
	case d.Enrollment.State == "active":
		summary.EnrollmentState = ProbeEnrollmentActive
	default:
		summary.EnrollmentState = ProbeEnrollmentPending
	}

	summary.ConnectionStatus = probeConnectionStatus(at, d)
	if !d.Registration.Enabled {
		summary.ExecutionStatus = ProbeExecutionPaused
	} else if d.Enrollment == nil {
		summary.ExecutionStatus = ProbeExecutionUnconfigured
	} else if d.Runtime != nil && leaseHeld(at, d.Runtime.LeaseUntil) {
		summary.ExecutionStatus = ProbeExecutionReady
	} else {
		summary.ExecutionStatus = ProbeExecutionDegraded
	}
	return summary
}

// probeConnectionStatus keeps transport health independent of execution
// readiness and target availability. Watchdog suspect/lost degrades a live
// connection to suspect; it never affects execution status.
func probeConnectionStatus(at time.Time, d domain.ProbeDiagnostics) string {
	if d.Session == nil {
		return ProbeConnectionNeverConnected
	}
	if !leaseHeld(at, d.Session.LeaseUntil) || !d.Session.Connected {
		return ProbeConnectionDisconnected
	}
	if d.Watchdog != nil && (d.Watchdog.Status == "suspect" || d.Watchdog.Status == "lost") {
		return ProbeConnectionSuspect
	}
	return ProbeConnectionOnline
}

// leaseHeld reports observational liveness: a held lease must have a deadline
// strictly in the future. A zero deadline is unheld.
func leaseHeld(at time.Time, until time.Time) bool {
	return !until.IsZero() && until.After(at)
}

// probeConfigSyncStatus proves application from exact document evidence.
func probeConfigSyncStatus(d domain.ProbeDiagnostics) string {
	if d.Publication == nil {
		return ""
	}
	if d.Applied == nil {
		return ProbeConfigSyncPending
	}
	if d.Applied.Revision == d.Publication.Revision {
		if d.Applied.SHA256 == d.Publication.SHA256 {
			return ProbeConfigSyncApplied
		}
		return ProbeConfigSyncRejected
	}
	return ProbeConfigSyncPending
}

func probeLastSeenAt(d domain.ProbeDiagnostics) *time.Time {
	var last time.Time
	if d.Watchdog != nil && d.Watchdog.UpdatedAt.After(last) {
		last = d.Watchdog.UpdatedAt
	}
	if d.Applied != nil && d.Applied.AppliedAt.After(last) {
		last = d.Applied.AppliedAt
	}
	if last.IsZero() {
		return nil
	}
	last = last.UTC()
	return &last
}

// ProbeFleetEntry is one probe's administrative read model: registration
// metadata, derived summary and the raw safe facts a detail view may map. It
// never carries credential bytes, key hashes, stream identities or
// configuration payloads; digests stay server-side and are never mapped to
// wire fields.
type ProbeFleetEntry struct {
	Registration domain.Probe
	Summary      ProbeDiagnosticSummary
	Diagnostics  domain.ProbeDiagnostics
}

// ProbeFleetPage is one administrative list page. NextCursor is nil on the
// last page and otherwise the ID after which the next page continues.
type ProbeFleetPage struct {
	Items      []ProbeFleetEntry
	NextCursor *string
}

// ProbeFleetService composes administrative fleet reads from the safe
// diagnostics port. Authorization (admin session or write-scope API key) is
// enforced by middleware before this service runs; it never widens visibility
// for non-admin callers on its own.
type ProbeFleetService struct {
	diagnostics ports.ProbeDiagnosticsRepository
}

// NewProbeFleetService binds the safe fleet read port.
func NewProbeFleetService(diagnostics ports.ProbeDiagnosticsRepository) *ProbeFleetService {
	return &ProbeFleetService{diagnostics: diagnostics}
}

// MaxProbeFleetPageLimit bounds one administrative list request.
const MaxProbeFleetPageLimit = 100

// DefaultProbeFleetPageLimit is the page size when the caller omits limit.
const DefaultProbeFleetPageLimit = 50

// List returns one page of fleet entries ordered by probe ID. limit must be
// within [1, MaxProbeFleetPageLimit]; cursor is an exclusive probe ID or empty.
func (s *ProbeFleetService) List(ctx context.Context, cursor string, limit int, now time.Time) (*ProbeFleetPage, error) {
	if s == nil || s.diagnostics == nil {
		return nil, domain.ErrInternal
	}
	if now.IsZero() {
		return nil, domain.ErrValidation
	}
	if limit < 1 || limit > MaxProbeFleetPageLimit {
		return nil, domain.ErrValidation
	}
	if cursor != "" && !validProbeFleetID(cursor) {
		return nil, domain.ErrValidation
	}
	// One extra row proves whether another page exists without a count query.
	rows, err := s.diagnostics.ListProbeDiagnostics(ctx, cursor, limit+1)
	if err != nil {
		return nil, err
	}
	page := &ProbeFleetPage{Items: make([]ProbeFleetEntry, 0, len(rows))}
	for index, row := range rows {
		if index == limit {
			cursor := page.Items[len(page.Items)-1].Registration.ID
			page.NextCursor = &cursor
			break
		}
		page.Items = append(page.Items, newProbeFleetEntry(now, row))
	}
	return page, nil
}

// Detail returns one probe's complete administrative read model.
func (s *ProbeFleetService) Detail(ctx context.Context, probeID string, now time.Time) (*ProbeFleetEntry, error) {
	if s == nil || s.diagnostics == nil {
		return nil, domain.ErrInternal
	}
	if now.IsZero() {
		return nil, domain.ErrValidation
	}
	if !validProbeFleetID(probeID) {
		return nil, domain.ErrValidation
	}
	row, err := s.diagnostics.GetProbeDiagnostics(ctx, probeID)
	if err != nil {
		return nil, err
	}
	entry := newProbeFleetEntry(now, *row)
	return &entry, nil
}

// DiagnosticSummary exposes the derived summary for one probe so scoped
// monitor reads share exactly the same diagnostic vocabulary as fleet reads.
// A missing probe is unreported evidence: the summary is empty, not an error.
func (s *ProbeFleetService) DiagnosticSummary(ctx context.Context, probeID string, now time.Time) (ProbeDiagnosticSummary, error) {
	if s == nil || s.diagnostics == nil {
		return ProbeDiagnosticSummary{}, domain.ErrInternal
	}
	if now.IsZero() {
		return ProbeDiagnosticSummary{}, domain.ErrValidation
	}
	if !validProbeFleetID(probeID) {
		return ProbeDiagnosticSummary{}, domain.ErrValidation
	}
	row, err := s.diagnostics.GetProbeDiagnostics(ctx, probeID)
	if err != nil {
		return ProbeDiagnosticSummary{}, err
	}
	return ProbeDiagnosticsSummary(now, *row), nil
}

func newProbeFleetEntry(now time.Time, row domain.ProbeDiagnostics) ProbeFleetEntry {
	return ProbeFleetEntry{
		Registration: row.Registration,
		Summary:      ProbeDiagnosticsSummary(now, row),
		Diagnostics:  row,
	}
}

func validProbeFleetID(probeID string) bool {
	return probeID == domain.LocalProbeID || domain.ValidHubID(probeID)
}
