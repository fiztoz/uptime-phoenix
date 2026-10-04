package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const fleetTestProbeID = "11111111-1111-4111-8111-111111111111"

type fleetDiagnosticsRepo struct {
	ports.ProbeDiagnosticsRepository
	list    []domain.ProbeDiagnostics
	byID    map[string]domain.ProbeDiagnostics
	after   string
	limit   int
	listErr error
	getErr  error
}

func (r *fleetDiagnosticsRepo) ListProbeDiagnostics(_ context.Context, after string, limit int) ([]domain.ProbeDiagnostics, error) {
	r.after, r.limit = after, limit
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.list, nil
}

func (r *fleetDiagnosticsRepo) GetProbeDiagnostics(_ context.Context, probeID string) (*domain.ProbeDiagnostics, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	row, ok := r.byID[probeID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return &row, nil
}

func fleetBaseProbe() domain.Probe {
	return domain.Probe{ID: fleetTestProbeID, Key: "singapore", Name: "Singapore", Location: "SG", Kind: domain.ProbeKindRemote, Enabled: true, Revision: 9007199254740993}
}

// TestProbeDiagnosticsSummaryBoundaries pins the evidence-bound derivation:
// application needs revision AND digest proof, connection health is separate
// from execution readiness, and absence is always unreported.
func TestProbeDiagnosticsSummaryBoundaries(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	document := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)

	base := domain.ProbeDiagnostics{
		Registration: fleetBaseProbe(),
		Enrollment:   &domain.ProbeEnrollmentFacts{State: "active", PreparedAt: at.Add(-time.Hour)},
		Session:      &domain.ProbeSessionFacts{OwnerID: "owner", Generation: 4, LeaseUntil: at.Add(time.Minute), Connected: true},
		Runtime:      &domain.ProbeRuntimeFacts{OwnerID: "owner", Epoch: 2, LeaseUntil: at.Add(time.Minute)},
		Publication:  &domain.ProbeConfigPublicationFacts{Revision: 7, SHA256: document, EffectiveAt: at.Add(-time.Minute)},
		Applied:      &domain.ProbeConfigAppliedFacts{Revision: 7, SHA256: document, AppliedAt: at.Add(-time.Minute), AssignmentCount: 3},
	}

	t.Run("AppliedNeedsRevisionAndDigest", func(t *testing.T) {
		got := ProbeDiagnosticsSummary(at, base)
		if got.ConfigSyncStatus != ProbeConfigSyncApplied || got.DesiredConfigRevision != 7 || got.AppliedConfigRevision != 7 {
			t.Fatalf("matching receipt not applied: %+v", got)
		}
		// Equal revision counters alone must never report applied.
		conflicting := base
		conflicting.Applied = &domain.ProbeConfigAppliedFacts{Revision: 7, SHA256: other, AppliedAt: at}
		if got := ProbeDiagnosticsSummary(at, conflicting); got.ConfigSyncStatus != ProbeConfigSyncRejected {
			t.Fatalf("digest conflict reported %q, want %q", got.ConfigSyncStatus, ProbeConfigSyncRejected)
		}
		// A receipt behind the publication is pending work, not application.
		behind := base
		behind.Publication = &domain.ProbeConfigPublicationFacts{Revision: 8, SHA256: other, EffectiveAt: at}
		if got := ProbeDiagnosticsSummary(at, behind); got.ConfigSyncStatus != ProbeConfigSyncPending || got.DesiredConfigRevision != 8 || got.AppliedConfigRevision != 7 {
			t.Fatalf("lagging receipt: %+v", got)
		}
		missing := base
		missing.Applied = nil
		if got := ProbeDiagnosticsSummary(at, missing); got.ConfigSyncStatus != ProbeConfigSyncPending || got.AppliedConfigRevision != 0 {
			t.Fatalf("publication without receipt: %+v", got)
		}
		unpublished := base
		unpublished.Publication, unpublished.Applied = nil, nil
		if got := ProbeDiagnosticsSummary(at, unpublished); got.ConfigSyncStatus != "" || got.DesiredConfigRevision != 0 {
			t.Fatalf("unreported config work must stay empty: %+v", got)
		}
	})

	t.Run("ConnectionIsNotExecution", func(t *testing.T) {
		got := ProbeDiagnosticsSummary(at, base)
		if got.ConnectionStatus != ProbeConnectionOnline || got.ExecutionStatus != ProbeExecutionReady {
			t.Fatalf("live session and lease: %+v", got)
		}
		// Watchdog suspect degrades only the connection, never execution.
		suspect := base
		suspect.Watchdog = &domain.ProbeWatchdogFacts{Status: "suspect", UpdatedAt: at.Add(-time.Second)}
		got = ProbeDiagnosticsSummary(at, suspect)
		if got.ConnectionStatus != ProbeConnectionSuspect || got.ExecutionStatus != ProbeExecutionReady {
			t.Fatalf("watchdog conflated with execution: %+v", got)
		}
		// An expired connector lease is a disconnect even when Connected is set.
		dropped := base
		dropped.Session = &domain.ProbeSessionFacts{OwnerID: "owner", Generation: 4, LeaseUntil: at.Add(-time.Second), Connected: true}
		got = ProbeDiagnosticsSummary(at, dropped)
		if got.ConnectionStatus != ProbeConnectionDisconnected || got.ExecutionStatus != ProbeExecutionReady {
			t.Fatalf("expired session lease: %+v", got)
		}
		// A held but unconnected session is not online.
		dialing := base
		dialing.Session = &domain.ProbeSessionFacts{OwnerID: "owner", Generation: 5, LeaseUntil: at.Add(time.Minute), Connected: false}
		if got := ProbeDiagnosticsSummary(at, dialing); got.ConnectionStatus != ProbeConnectionDisconnected {
			t.Fatalf("unconnected session: %+v", got)
		}
		// A live lease exactly at the deadline is not held.
		boundary := base
		boundary.Runtime = &domain.ProbeRuntimeFacts{OwnerID: "owner", Epoch: 2, LeaseUntil: at}
		if got := ProbeDiagnosticsSummary(at, boundary); got.ExecutionStatus != ProbeExecutionDegraded {
			t.Fatalf("deadline boundary must expire: %+v", got)
		}
		never := base
		never.Session, never.Runtime = nil, nil
		never.Enrollment = nil
		got = ProbeDiagnosticsSummary(at, never)
		if got.ConnectionStatus != ProbeConnectionNeverConnected || got.ExecutionStatus != ProbeExecutionUnconfigured || got.EnrollmentState != ProbeEnrollmentUnconfigured {
			t.Fatalf("registration without runtime evidence: %+v", got)
		}
		// Enrolled but without a held owner lease is degraded, never ready.
		idle := base
		idle.Runtime = &domain.ProbeRuntimeFacts{OwnerID: "owner", Epoch: 2, LeaseUntil: at.Add(-time.Second)}
		if got := ProbeDiagnosticsSummary(at, idle); got.ExecutionStatus != ProbeExecutionDegraded {
			t.Fatalf("expired owner: %+v", got)
		}
		// An enrollment without any runtime lease row is degraded as well.
		neverOwned := base
		neverOwned.Runtime = nil
		if got := ProbeDiagnosticsSummary(at, neverOwned); got.ExecutionStatus != ProbeExecutionDegraded {
			t.Fatalf("enrollment without owner row: %+v", got)
		}
	})

	t.Run("EnrollmentAndPause", func(t *testing.T) {
		prepared := base
		prepared.Enrollment = &domain.ProbeEnrollmentFacts{State: "prepared", PreparedAt: at}
		if got := ProbeDiagnosticsSummary(at, prepared); got.EnrollmentState != ProbeEnrollmentPending {
			t.Fatalf("prepared enrollment: %+v", got)
		}
		paused := base
		paused.Registration.Enabled = false
		got := ProbeDiagnosticsSummary(at, paused)
		if got.ExecutionStatus != ProbeExecutionPaused || got.ConnectionStatus != ProbeConnectionOnline {
			t.Fatalf("pause is not a disconnect: %+v", got)
		}
	})

	t.Run("LocalProbeIsScheduler", func(t *testing.T) {
		local := base
		local.Registration.ID, local.Registration.Kind = domain.LocalProbeID, domain.ProbeKindLocal
		local.Session, local.Runtime, local.Enrollment, local.Watchdog = nil, nil, nil, nil
		got := ProbeDiagnosticsSummary(at, local)
		if got.ConnectionStatus != ProbeConnectionOnline || got.EnrollmentState != ProbeEnrollmentActive || got.ExecutionStatus != ProbeExecutionReady {
			t.Fatalf("local projection: %+v", got)
		}
		if got.ConfigSyncStatus != ProbeConfigSyncApplied {
			t.Fatalf("local config evidence ignored: %+v", got)
		}
	})

	t.Run("LastSeenIsSourceEvidence", func(t *testing.T) {
		empty := ProbeDiagnosticsSummary(at, domain.ProbeDiagnostics{Registration: fleetBaseProbe()})
		if empty.LastSeenAt != nil {
			t.Fatalf("invented last seen: %+v", empty)
		}
		withWatchdog := base
		withWatchdog.Watchdog = &domain.ProbeWatchdogFacts{Status: "healthy", UpdatedAt: at.Add(-2 * time.Minute)}
		if got := ProbeDiagnosticsSummary(at, withWatchdog); got.LastSeenAt == nil || !got.LastSeenAt.Equal(base.Applied.AppliedAt) {
			t.Fatalf("last seen must be the latest source report: %+v", got.LastSeenAt)
		}
	})
}

func TestProbeFleetServiceListAndDetail(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	page := func(n int) []domain.ProbeDiagnostics {
		rows := make([]domain.ProbeDiagnostics, 0, n)
		for i := 0; i < n; i++ {
			rows = append(rows, domain.ProbeDiagnostics{Registration: domain.Probe{ID: fleetTestProbeID, Kind: domain.ProbeKindRemote, Enabled: true, Revision: 9007199254740993}})
		}
		return rows
	}

	t.Run("PaginationDerivesNextCursor", func(t *testing.T) {
		repo := &fleetDiagnosticsRepo{list: page(3)}
		svc := NewProbeFleetService(repo)
		got, err := svc.List(t.Context(), "", 2, at)
		if err != nil || len(got.Items) != 2 || got.NextCursor == nil || *got.NextCursor != fleetTestProbeID {
			t.Fatalf("page with successor: %+v %v", got, err)
		}
		if repo.after != "" || repo.limit != 3 {
			t.Fatalf("store call not bounded: after=%q limit=%d", repo.after, repo.limit)
		}
		repo.list = page(2)
		got, err = svc.List(t.Context(), fleetTestProbeID, 2, at)
		if err != nil || len(got.Items) != 2 || got.NextCursor != nil {
			t.Fatalf("final page: %+v %v", got, err)
		}
		if repo.after != fleetTestProbeID {
			t.Fatalf("cursor not forwarded: %q", repo.after)
		}
	})

	t.Run("Validation", func(t *testing.T) {
		svc := NewProbeFleetService(&fleetDiagnosticsRepo{})
		for _, limit := range []int{0, -1, MaxProbeFleetPageLimit + 1} {
			if _, err := svc.List(t.Context(), "", limit, at); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("limit %d accepted: %v", limit, err)
			}
		}
		if _, err := svc.List(t.Context(), "not-a-probe-id", 10, at); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("malformed cursor accepted: %v", err)
		}
		if _, err := svc.List(t.Context(), "", 10, time.Time{}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("zero instant accepted: %v", err)
		}
		if _, err := svc.Detail(t.Context(), "not-a-probe-id", at); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("malformed probe id accepted: %v", err)
		}
		if _, err := svc.Detail(t.Context(), "9223372036854775807", at); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("overflow probe id accepted: %v", err)
		}
	})

	t.Run("NotFoundAndSummary", func(t *testing.T) {
		repo := &fleetDiagnosticsRepo{byID: map[string]domain.ProbeDiagnostics{
			domain.LocalProbeID: {Registration: domain.Probe{ID: domain.LocalProbeID, Kind: domain.ProbeKindLocal, Enabled: true}},
		}}
		svc := NewProbeFleetService(repo)
		if _, err := svc.Detail(t.Context(), fleetTestProbeID, at); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("absent probe: %v", err)
		}
		entry, err := svc.Detail(t.Context(), domain.LocalProbeID, at)
		if err != nil || entry.Summary.ConnectionStatus != ProbeConnectionOnline || entry.Summary.EnrollmentState != ProbeEnrollmentActive {
			t.Fatalf("local detail: %+v %v", entry, err)
		}
		summary, err := svc.DiagnosticSummary(t.Context(), domain.LocalProbeID, at)
		if err != nil || summary.ExecutionStatus != ProbeExecutionReady {
			t.Fatalf("shared vocabulary: %+v %v", summary, err)
		}
		if _, err = svc.DiagnosticSummary(t.Context(), fleetTestProbeID, at); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("summary for absent probe: %v", err)
		}
	})

	t.Run("LargeRevisionCountersSurvive", func(t *testing.T) {
		repo := &fleetDiagnosticsRepo{byID: map[string]domain.ProbeDiagnostics{fleetTestProbeID: {
			Registration: fleetBaseProbe(),
			Publication:  &domain.ProbeConfigPublicationFacts{Revision: 9007199254740993, SHA256: "d0"},
			Applied:      &domain.ProbeConfigAppliedFacts{Revision: 9007199254740993, SHA256: "d0", AppliedAt: at},
		}}}
		entry, err := NewProbeFleetService(repo).Detail(t.Context(), fleetTestProbeID, at)
		if err != nil || entry.Summary.DesiredConfigRevision != 9007199254740993 || entry.Summary.ConfigSyncStatus != ProbeConfigSyncApplied {
			t.Fatalf("2^53+ revision: %+v %v", entry.Summary, err)
		}
	})
}
