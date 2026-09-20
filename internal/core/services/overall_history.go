package services

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ReconstructOverallHistory builds overall availability intervals from regional
// transitions, assignment/policy effective times, and freshness expirations.
// Added probes do not participate before their assignment interval. Late samples
// improve the reconstructed answer through ObservedAt; they do not pool raw
// heartbeats across probes.
func ReconstructOverallHistory(in domain.OverallHistoryInput) (domain.MonitorHealthHistory, error) {
	from, to := in.From.UTC(), in.To.UTC()
	if from.IsZero() || to.IsZero() || !from.Before(to) || in.FreshFor < 0 {
		return domain.MonitorHealthHistory{}, ErrInvalidHealthEvidence
	}
	out := domain.MonitorHealthHistory{MonitorID: in.MonitorID, From: from, To: to}
	times := map[int64]time.Time{from.UnixNano(): from}
	addTime := func(t time.Time) {
		t = t.UTC()
		if t.Before(from) || !t.Before(to) {
			return
		}
		times[t.UnixNano()] = t
	}
	for _, assignment := range in.Assignments {
		if assignment.ProbeID == "" || assignment.From.IsZero() {
			return domain.MonitorHealthHistory{}, fmt.Errorf("assignment interval: %w", ErrInvalidHealthEvidence)
		}
		if !assignment.To.IsZero() && !assignment.From.Before(assignment.To) {
			return domain.MonitorHealthHistory{}, fmt.Errorf("assignment interval order: %w", ErrInvalidHealthEvidence)
		}
		addTime(assignment.From)
		if !assignment.To.IsZero() {
			addTime(assignment.To)
		}
	}
	observationTimes, freshnessTimes := make(map[int64]bool), make(map[int64]bool)
	for _, obs := range in.Observations {
		observationTimes[obs.ObservedAt.UnixNano()] = true
		freshnessTimes[obs.ObservedAt.Add(in.FreshFor).UnixNano()] = in.FreshFor > 0
		if obs.ProbeID == "" {
			return domain.MonitorHealthHistory{}, fmt.Errorf("observation identity: %w", ErrInvalidHealthEvidence)
		}
		addTime(obs.ObservedAt)
		if in.FreshFor > 0 {
			addTime(obs.ObservedAt.UTC().Add(in.FreshFor))
		}
	}
	for _, gap := range in.Gaps {
		if gap.ProbeID == "" || gap.StreamID == "" || gap.FromSeq <= 0 || gap.ThroughSeq < gap.FromSeq || gap.From.IsZero() || gap.Through.Before(gap.From) {
			return domain.MonitorHealthHistory{}, fmt.Errorf("history gap: %w", ErrInvalidHealthEvidence)
		}
		addTime(gap.From)
	}
	ordered := make([]time.Time, 0, len(times)+1)
	for _, t := range times {
		ordered = append(ordered, t)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Before(ordered[j]) })
	ordered = append(ordered, to)

	var prev domain.MonitorHealthInterval
	var havePrev bool
	var prevIDs []string
	var prevRev int64
	observations := slices.Clone(in.Observations)
	slices.SortFunc(observations, func(a, b domain.RegionalObservation) int {
		if order := a.ObservedAt.Compare(b.ObservedAt); order != 0 {
			return order
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	latest := make(map[observationKey]domain.RegionalObservation)
	observationIndex := 0
	for i := 0; i < len(ordered)-1; i++ {
		start, end := ordered[i], ordered[i+1]
		if !start.Before(end) {
			continue
		}
		active, err := activeAssignments(in.Assignments, start)
		if err != nil {
			return domain.MonitorHealthHistory{}, err
		}
		if len(active) == 0 {
			if havePrev {
				out.Intervals = append(out.Intervals, prev)
				addIntervalDuration(&out.Durations, prev)
			}
			// Missing effective-time records cannot establish a healthy quorum.
			// Include that duration in coverage instead of dropping it silently.
			prev = domain.MonitorHealthInterval{
				From: start, To: end, Status: domain.StatusUnknown,
				Reason: "missing_assignment_history", Cause: domain.HealthHistoryCauseAdministrative,
				Policy: domain.HealthPolicyAnyDown,
			}
			havePrev, prevIDs, prevRev = true, nil, 0
			continue
		}
		policy, revision, err := activePolicy(active)
		if err != nil {
			return domain.MonitorHealthHistory{}, err
		}
		evidence := make([]domain.RegionalHealthEvidence, 0, len(active))
		for observationIndex < len(observations) && !observations[observationIndex].ObservedAt.After(start) {
			obs := observations[observationIndex]
			key := observationKey{probeID: obs.ProbeID, generation: obs.AssignmentGeneration}
			if previous, ok := latest[key]; !ok || laterHistoricalEvidence(previous, obs) {
				latest[key] = obs
			}
			observationIndex++
		}
		for _, assignment := range active {
			region := domain.RegionalHealthEvidence{
				ProbeID:  assignment.ProbeID,
				FreshFor: in.FreshFor,
				Paused:   in.Paused || assignment.Paused,
			}
			key := observationKey{probeID: assignment.ProbeID, generation: assignment.Generation}
			if sample, ok := latest[key]; !ok {
				region.UnknownReason = "missing_evidence"
			} else if sample.AssignmentGeneration != assignment.Generation {
				region.UnknownReason = "stale_generation"
			} else {
				region.Status = sample.Status
				region.ObservedAt = sample.ObservedAt.UTC()
				if observationHiddenByGap(sample, start, in.Gaps) {
					region.UnknownReason = "history_gap"
				}
			}
			evidence = append(evidence, region)
		}
		health, err := EvaluateMonitorHealth(start, policy, evidence)
		if err != nil {
			return domain.MonitorHealthHistory{}, err
		}
		ids := assignmentIDs(active)
		interval := domain.MonitorHealthInterval{
			From: start, To: end, Status: health.Status, Reason: health.Reason,
			Cause:  historyCause(start, active, prevIDs, prevRev, revision, observationTimes, freshnessTimes, havePrev),
			Policy: policy, PolicyRevision: revision, Counts: health.Counts,
		}
		if havePrev && prev.Status == interval.Status && prev.Reason == interval.Reason &&
			prev.Policy == interval.Policy && prev.PolicyRevision == interval.PolicyRevision && prev.Counts == interval.Counts {
			prev.To = end
			prevIDs, prevRev = ids, revision
			continue
		}
		if havePrev {
			out.Intervals = append(out.Intervals, prev)
			addIntervalDuration(&out.Durations, prev)
		}
		prev, havePrev, prevIDs, prevRev = interval, true, ids, revision
	}
	if havePrev {
		out.Intervals = append(out.Intervals, prev)
		addIntervalDuration(&out.Durations, prev)
	}
	return out, nil
}

type observationKey struct {
	probeID    string
	generation int64
}

func activeAssignments(assignments []domain.AssignmentInterval, at time.Time) ([]domain.AssignmentInterval, error) {
	var active []domain.AssignmentInterval
	seen := make(map[string]struct{}, len(assignments))
	for _, assignment := range assignments {
		from, to := assignment.From.UTC(), assignment.To.UTC()
		if at.Before(from) || (!to.IsZero() && !at.Before(to)) {
			continue
		}
		if _, dup := seen[assignment.ProbeID]; dup {
			return nil, fmt.Errorf("overlapping assignment for %s: %w", assignment.ProbeID, ErrInvalidHealthEvidence)
		}
		seen[assignment.ProbeID] = struct{}{}
		active = append(active, assignment)
	}
	return active, nil
}

func activePolicy(active []domain.AssignmentInterval) (domain.HealthPolicy, int64, error) {
	var policy domain.HealthPolicy
	var revision int64
	have := false
	for _, assignment := range active {
		if !have || assignment.Revision > revision {
			policy, revision, have = assignment.Policy, assignment.Revision, true
			continue
		}
		if assignment.Revision == revision && assignment.Policy != policy {
			return "", 0, fmt.Errorf("conflicting health policy: %w", ErrInvalidHealthEvidence)
		}
	}
	if !have || (policy != domain.HealthPolicyAnyDown && policy != domain.HealthPolicyAllDown) {
		return "", 0, ErrInvalidHealthEvidence
	}
	return policy, revision, nil
}

func laterHistoricalEvidence(previous, next domain.RegionalObservation) bool {
	if next.StreamID != "" && next.StreamID == previous.StreamID && next.Seq > 0 && previous.Seq > 0 {
		return next.Seq > previous.Seq || next.Seq == previous.Seq && next.ID > previous.ID
	}
	return next.ObservedAt.After(previous.ObservedAt) || next.ObservedAt.Equal(previous.ObservedAt) && next.ID > previous.ID
}

// Declared loss interrupts carry-forward until retained evidence is newer than
// the lost source range. A zero-length time range still represents lost events.
func observationHiddenByGap(sample domain.RegionalObservation, at time.Time, gaps []domain.RegionalHistoryGap) bool {
	for _, gap := range gaps {
		if gap.ProbeID != sample.ProbeID || at.Before(gap.From) {
			continue
		}
		if gap.StreamID == sample.StreamID {
			if sample.Seq <= gap.ThroughSeq {
				return true
			}
		} else if !sample.ObservedAt.After(gap.Through) {
			// A loss range in another stream cannot establish continuity from
			// older evidence. New-stream observations restore it in source time.
			return true
		}
	}
	return false
}

func assignmentIDs(active []domain.AssignmentInterval) []string {
	ids := make([]string, len(active))
	for i, assignment := range active {
		ids[i] = assignment.ProbeID
	}
	sort.Strings(ids)
	return ids
}

func historyCause(
	at time.Time,
	active []domain.AssignmentInterval,
	prevIDs []string,
	prevRev, revision int64,
	observationTimes, freshnessTimes map[int64]bool,
	havePrev bool,
) string {
	ids := assignmentIDs(active)
	if havePrev && !sameStrings(prevIDs, ids) {
		return domain.HealthHistoryCauseAssignment
	}
	if havePrev && prevRev != revision {
		return domain.HealthHistoryCausePolicy
	}
	if freshnessTimes[at.UnixNano()] {
		return domain.HealthHistoryCauseFreshness
	}
	if observationTimes[at.UnixNano()] {
		return domain.HealthHistoryCauseRegional
	}
	for _, assignment := range active {
		if assignment.From.UTC().Equal(at) {
			return domain.HealthHistoryCauseAssignment
		}
	}
	return domain.HealthHistoryCauseAdministrative
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func addIntervalDuration(d *domain.HealthDurations, interval domain.MonitorHealthInterval) {
	dur := interval.To.Sub(interval.From)
	if dur <= 0 {
		return
	}
	switch {
	case interval.Counts.Assigned > 0 && interval.Counts.Paused == interval.Counts.Assigned:
		d.Paused += dur
	case interval.Status == domain.StatusUp:
		d.Up += dur
	case interval.Status == domain.StatusDown:
		d.Down += dur
	case interval.Status == domain.StatusPending:
		d.Pending += dur
	case interval.Status == domain.StatusMaintenance:
		d.Maintenance += dur
	default:
		d.Unknown += dur
	}
}
