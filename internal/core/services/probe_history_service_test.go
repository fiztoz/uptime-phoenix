package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func historyWorkFixture() domain.ProbeHistoryWork {
	at := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	return domain.ProbeHistoryWork{Bucket: domain.DirtyBucket{MonitorID: 1, ProbeID: "a", Resolution: domain.DirtyResolution1m, Bucket: at}, Evidence: domain.OverallHistoryInput{MonitorID: 1, From: at, To: at.Add(time.Minute), FreshFor: time.Minute, Assignments: []domain.AssignmentInterval{{ProbeID: "a", Generation: 1, Revision: 1, Policy: domain.HealthPolicyAnyDown, From: at.Add(-time.Hour)}}}}
}

func historyObservation(w domain.ProbeHistoryWork, seq, seconds int64, status domain.Status) domain.RegionalObservation {
	return domain.RegionalObservation{ID: seq, MonitorID: 1, ProbeID: "a", StreamID: "stream-a", Seq: seq, AssignmentGeneration: 1, ObservedAt: w.Evidence.From.Add(time.Duration(seconds) * time.Second), Status: status, Ping: 10}
}

func projectRegionalHistory(t *testing.T, w domain.ProbeHistoryWork) *domain.RegionalHistoryRollup {
	t.Helper()
	result, err := NewProbeHistoryService(nil).ProjectHistory(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	if result.Regional == nil || len(result.Overall) != 0 {
		t.Fatal("incorrect regional projection", result)
	}
	return result.Regional
}

func TestProbeHistoryDeclaredGapReducesCoverageWithoutSamples(t *testing.T) {
	w := historyWorkFixture()
	w.Evidence.Observations = []domain.RegionalObservation{historyObservation(w, 1, 0, domain.StatusUp), historyObservation(w, 4, 40, domain.StatusDown)}
	w.Evidence.Gaps = []domain.RegionalHistoryGap{{ProbeID: "a", StreamID: "stream-a", FromSeq: 2, ThroughSeq: 3, From: w.Evidence.From.Add(10 * time.Second), Through: w.Evidence.From.Add(20 * time.Second)}}
	r := projectRegionalHistory(t, w)
	if r.TotalChecks != 2 || r.UpCount != 1 || r.DownCount != 1 || r.UnknownCount != 0 || r.PingCount != 2 || r.AvgPing != 10 {
		t.Fatalf("gap fabricated samples or latency: %+v", r)
	}
	if r.Durations.Up != 10*time.Second || r.Durations.Unknown != 30*time.Second || r.Durations.Down != 20*time.Second {
		t.Fatalf("gap did not interrupt carry-forward: %+v", r.Durations)
	}
	// Single-source-time loss remains a real gap, not an empty interval.
	w.Evidence.Gaps[0].Through = w.Evidence.Gaps[0].From
	if got := projectRegionalHistory(t, w).Durations; got != r.Durations {
		t.Fatalf("zero-time source gap ignored: %+v", got)
	}
}

func TestProbeHistoryBackwardClockDoesNotResurrectEarlierSequence(t *testing.T) {
	w := historyWorkFixture()
	w.Evidence.Observations = []domain.RegionalObservation{historyObservation(w, 2, 10, domain.StatusDown), historyObservation(w, 1, 20, domain.StatusUp)}
	r := projectRegionalHistory(t, w)
	if r.Durations.Unknown != 10*time.Second || r.Durations.Down != 50*time.Second || r.Durations.Up != 0 {
		t.Fatalf("older sequence regained authority after clock regression: %+v", r.Durations)
	}
	// A retained sequence after a gap restores continuity even when its clock
	// is earlier than the lost sample's final timestamp.
	w.Evidence.Observations = append(w.Evidence.Observations, historyObservation(w, 4, 30, domain.StatusUp))
	w.Evidence.Gaps = []domain.RegionalHistoryGap{{ProbeID: "a", StreamID: "stream-a", FromSeq: 3, ThroughSeq: 3, From: w.Evidence.From.Add(25 * time.Second), Through: w.Evidence.From.Add(40 * time.Second)}}
	r = projectRegionalHistory(t, w)
	if r.Durations.Unknown != 15*time.Second || r.Durations.Down != 15*time.Second || r.Durations.Up != 30*time.Second {
		t.Fatalf("newer retained evidence did not restore gap: %+v", r.Durations)
	}
}

func TestProbeHistoryAssignmentBoundariesExcludeRetiredSamples(t *testing.T) {
	w := historyWorkFixture()
	w.Evidence.Assignments = []domain.AssignmentInterval{{ProbeID: "a", Generation: 2, Revision: 2, Policy: domain.HealthPolicyAnyDown, From: w.Evidence.From.Add(20 * time.Second), To: w.Evidence.From.Add(50 * time.Second)}}
	old := historyObservation(w, 1, 20, domain.StatusDown)
	current := historyObservation(w, 2, 30, domain.StatusUp)
	current.AssignmentGeneration = 2
	w.Evidence.Observations = []domain.RegionalObservation{old, current}
	r := projectRegionalHistory(t, w)
	if r.TotalChecks != 1 || r.DownCount != 0 || r.Durations.Unknown != 10*time.Second || r.Durations.Up != 20*time.Second {
		t.Fatalf("retired or unassigned time polluted region: %+v", r)
	}
}

func TestProbeHistoryOverallKeepsIndependentRegions(t *testing.T) {
	w := historyWorkFixture()
	w.Bucket.Resolution = domain.DirtyResolutionOverall
	b := w.Evidence.Assignments[0]
	b.ProbeID = "b"
	w.Evidence.Assignments = append(w.Evidence.Assignments, b)
	w.Evidence.Observations = []domain.RegionalObservation{historyObservation(w, 1, 0, domain.StatusUp)}
	bSample := historyObservation(w, 1, 0, domain.StatusDown)
	bSample.ProbeID = "b"
	bSample.StreamID = "stream-b"
	w.Evidence.Observations = append(w.Evidence.Observations, bSample)
	for n := int64(1); n < 60; n++ {
		w.Evidence.Observations = append(w.Evidence.Observations, historyObservation(w, n+1, n, domain.StatusUp))
	}
	result, err := NewProbeHistoryService(nil).ProjectHistory(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	if result.Regional != nil || len(result.Overall) != 1 || result.Overall[0].Status != domain.StatusDown || result.Overall[0].Counts.Down != 1 {
		t.Fatalf("fast region outweighed failing region: %+v", result)
	}
	for i := range w.Evidence.Assignments {
		w.Evidence.Assignments[i].Policy = domain.HealthPolicyAllDown
	}
	result, err = NewProbeHistoryService(nil).ProjectHistory(t.Context(), w)
	if err != nil || len(result.Overall) != 1 || result.Overall[0].Status != domain.StatusUp {
		t.Fatalf("all_down policy lost: %+v %v", result, err)
	}
}

type historyWorkRepo struct {
	at        time.Time
	limit     int
	projected bool
}

func (r *historyWorkRepo) ProcessHistoryWork(ctx context.Context, at time.Time, limit int, p ports.ProbeHistoryProjector) (int, error) {
	r.at, r.limit = at, limit
	_, err := p.ProjectHistory(ctx, historyWorkFixture())
	r.projected = err == nil
	return 1, err
}

func TestProbeHistoryServiceNormalizesUTCAndBoundsBatch(t *testing.T) {
	r := &historyWorkRepo{}
	s := NewProbeHistoryService(r)
	at := time.Date(2026, 9, 20, 7, 0, 0, 0, time.FixedZone("UTC+7", 7*3600))
	if n, err := s.ProcessBatch(t.Context(), at, 10); err != nil || n != 1 || !r.projected || r.at.Location() != time.UTC || r.limit != 10 {
		t.Fatalf("invalid work boundary: %+v %d %v", r, n, err)
	}
	if _, err := s.ProcessBatch(t.Context(), at, 1001); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}

func TestProbeHistoryParentWeightsLatencyAndPreservesMissingCoverage(t *testing.T) {
	w := historyWorkFixture()
	w.Bucket.Resolution = domain.DirtyResolution1h
	w.Evidence.To = w.Evidence.From.Add(time.Hour)
	w.Children = []domain.RegionalHistoryRollup{
		{MonitorID: 1, ProbeID: "a", Resolution: domain.DirtyResolution1m, Bucket: w.Bucket.Bucket, TotalChecks: 1, UpCount: 1, PingCount: 1, AvgPing: 100, MinPing: 100, MaxPing: 100, Durations: domain.HealthDurations{Up: time.Minute}},
		{MonitorID: 1, ProbeID: "a", Resolution: domain.DirtyResolution1m, Bucket: w.Bucket.Bucket.Add(time.Minute), TotalChecks: 3, DownCount: 3, PingCount: 3, AvgPing: 20, MinPing: 10, MaxPing: 30, Durations: domain.HealthDurations{Down: time.Minute}},
	}
	r := projectRegionalHistory(t, w)
	if r.TotalChecks != 4 || r.AvgPing != 40 || r.PingCount != 4 || r.MinPing != 10 || r.MaxPing != 100 || r.Durations.Up != time.Minute || r.Durations.Down != time.Minute || r.Durations.Unknown != 58*time.Minute {
		t.Fatalf("invalid hierarchy projection: %+v", r)
	}
	w.Evidence.Observations = []domain.RegionalObservation{historyObservation(w, 1, 0, domain.StatusUp)}
	if _, err := NewProbeHistoryService(nil).ProjectHistory(t.Context(), w); err == nil {
		t.Fatal("parent accepted mixed raw and child evidence")
	}
	w.Evidence.Observations = nil
	w.Children = append(w.Children, w.Children[0])
	if _, err := NewProbeHistoryService(nil).ProjectHistory(t.Context(), w); err == nil {
		t.Fatal("parent accepted duplicate child")
	}
}

func TestProbeHistorySkewedSampleCannotRestoreDeclaredLoss(t *testing.T) {
	w := historyWorkFixture()
	w.Evidence.Gaps = []domain.RegionalHistoryGap{{ProbeID: "a", StreamID: "stream-a", FromSeq: 2, ThroughSeq: 3, From: w.Evidence.From.Add(10 * time.Second), Through: w.Evidence.From.Add(20 * time.Second)}}
	w.Evidence.Observations = []domain.RegionalObservation{historyObservation(w, 1, 0, domain.StatusUp), historyObservation(w, 2, 50, domain.StatusUp)}
	r := projectRegionalHistory(t, w)
	if r.Durations.Up != 10*time.Second || r.Durations.Unknown != 50*time.Second {
		t.Fatalf("skew restored declared lost sequence: %+v", r.Durations)
	}
}
