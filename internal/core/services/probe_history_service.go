package services

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeHistoryService recomputes historical evidence without running checks or
// notifications. The repository owns the atomic read/project/consume boundary.
type ProbeHistoryService struct {
	repository ports.ProbeHistoryWorkRepository
}

// HistoryFreshness supplies the same carry-forward rule as live health.
func (s *ProbeHistoryService) HistoryFreshness(monitor domain.Monitor) (time.Duration, error) {
	return RegionalFreshnessWindow(monitor.Interval, monitor.RetryInterval, monitor.Timeout)
}

// NewProbeHistoryService binds the transactional work queue.
func NewProbeHistoryService(repository ports.ProbeHistoryWorkRepository) *ProbeHistoryService {
	return &ProbeHistoryService{repository: repository}
}

// ProcessBatch processes bounded closed windows and resumable gap work.
func (s *ProbeHistoryService) ProcessBatch(ctx context.Context, now time.Time, limit int) (int, error) {
	if s.repository == nil || now.IsZero() || limit <= 0 || limit > 1000 {
		return 0, domain.ErrValidation
	}
	return s.repository.ProcessHistoryWork(ctx, now.UTC(), limit, s)
}

// ProjectHistory evaluates a single region or policy-derived overall interval.
// Regional sample statistics remain independent from duration-based coverage.
func (s *ProbeHistoryService) ProjectHistory(ctx context.Context, work domain.ProbeHistoryWork) (domain.ProbeHistoryProjection, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProbeHistoryProjection{}, err
	}
	b := work.Bucket
	in := work.Evidence
	width := historyResolution(b.Resolution)
	if b.MonitorID <= 0 || b.ProbeID == "" || width == 0 || in.MonitorID != b.MonitorID || !in.From.Equal(b.Bucket) || !in.To.Equal(in.From.Add(width)) || !in.From.Equal(in.From.UTC().Truncate(width)) {
		return domain.ProbeHistoryProjection{}, domain.ErrValidation
	}
	if b.Resolution == domain.DirtyResolutionOverall {
		history, err := ReconstructOverallHistory(in)
		return domain.ProbeHistoryProjection{Overall: history.Intervals}, err
	}
	if (b.Resolution == domain.DirtyResolution1h || b.Resolution == domain.DirtyResolution1d) && (len(in.Observations) != 0 || len(in.Gaps) != 0) {
		// Parent coverage comes only from committed children. Mixing raw
		// evidence into its assignment baseline would count coverage twice.
		return domain.ProbeHistoryProjection{}, domain.ErrValidation
	}
	assignments := make([]domain.AssignmentInterval, 0, len(in.Assignments))
	for _, a := range in.Assignments {
		if a.ProbeID == b.ProbeID {
			a.Policy = domain.HealthPolicyAnyDown
			assignments = append(assignments, a)
		}
	}
	in.Assignments = assignments
	history, err := ReconstructOverallHistory(in)
	if err != nil {
		return domain.ProbeHistoryProjection{}, err
	}
	rollup := &domain.RegionalHistoryRollup{MonitorID: b.MonitorID, ProbeID: b.ProbeID, Bucket: b.Bucket.UTC(), Resolution: b.Resolution}
	for _, interval := range history.Intervals {
		// Outside this probe's membership there is no regional denominator.
		// Overall history separately counts missing assignment evidence.
		if interval.Counts.Assigned > 0 {
			addIntervalDuration(&rollup.Durations, interval)
		}
	}
	if b.Resolution == domain.DirtyResolution1h || b.Resolution == domain.DirtyResolution1d {
		return mergeHistoryChildren(rollup, work.Children, in.Paused)
	}
	var pingSum float64
	for _, obs := range in.Observations {
		if obs.ProbeID != b.ProbeID || obs.ObservedAt.Before(in.From) || !obs.ObservedAt.Before(in.To) {
			continue
		}
		active, err := activeAssignments(assignments, obs.ObservedAt)
		if err != nil {
			return domain.ProbeHistoryProjection{}, err
		}
		if len(active) != 1 || active[0].Generation != obs.AssignmentGeneration {
			continue
		}
		rollup.TotalChecks++
		switch obs.Status {
		case domain.StatusUp:
			rollup.UpCount++
		case domain.StatusDown:
			rollup.DownCount++
		case domain.StatusPending:
			rollup.PendingCount++
		case domain.StatusMaintenance:
			rollup.MaintCount++
		default:
			rollup.UnknownCount++
		}
		if obs.Ping > 0 {
			rollup.PingCount++
			pingSum += float64(obs.Ping)
			if rollup.MinPing == 0 || obs.Ping < rollup.MinPing {
				rollup.MinPing = obs.Ping
			}
			if obs.Ping > rollup.MaxPing {
				rollup.MaxPing = obs.Ping
			}
		}
	}
	if rollup.PingCount > 0 {
		rollup.AvgPing = pingSum / float64(rollup.PingCount)
	}
	return domain.ProbeHistoryProjection{Regional: rollup}, nil
}

func mergeHistoryChildren(out *domain.RegionalHistoryRollup, children []domain.RegionalHistoryRollup, paused bool) (domain.ProbeHistoryProjection, error) {
	var pingSum float64
	unknownRemaining, pausedRemaining := out.Durations.Unknown, out.Durations.Paused
	seen := make(map[int64]bool, len(children))
	for _, child := range children {
		width := time.Minute
		resolution := domain.DirtyResolution1m
		if out.Resolution == domain.DirtyResolution1d {
			width = time.Hour
			resolution = domain.DirtyResolution1h
		}
		if child.MonitorID != out.MonitorID || child.ProbeID != out.ProbeID || child.Resolution != resolution || child.Bucket.Before(out.Bucket) || !child.Bucket.Before(out.Bucket.Add(historyResolution(out.Resolution))) || !child.Bucket.Equal(child.Bucket.UTC().Truncate(width)) || seen[child.Bucket.UnixNano()] {
			return domain.ProbeHistoryProjection{}, domain.ErrValidation
		}
		seen[child.Bucket.UnixNano()] = true
		d := child.Durations
		total := d.Up + d.Down + d.Pending + d.Unknown + d.Maintenance + d.Paused
		if d.Up < 0 || d.Down < 0 || d.Pending < 0 || d.Unknown < 0 || d.Maintenance < 0 || d.Paused < 0 || total > width {
			return domain.ProbeHistoryProjection{}, domain.ErrValidation
		}
		if paused {
			// The caller applies the same administrative pause policy as a
			// minute projection; sample counts still describe real observations.
			d = domain.HealthDurations{Paused: total}
		}
		remaining := total
		replaced := min(unknownRemaining, remaining)
		unknownRemaining -= replaced
		out.Durations.Unknown -= replaced
		remaining -= replaced
		replaced = min(pausedRemaining, remaining)
		pausedRemaining -= replaced
		out.Durations.Paused -= replaced
		remaining -= replaced
		if remaining != 0 {
			return domain.ProbeHistoryProjection{}, domain.ErrValidation
		}
		out.Durations.Up += d.Up
		out.Durations.Down += d.Down
		out.Durations.Pending += d.Pending
		out.Durations.Unknown += d.Unknown
		out.Durations.Maintenance += d.Maintenance
		out.Durations.Paused += d.Paused
		out.TotalChecks += child.TotalChecks
		out.UpCount += child.UpCount
		out.DownCount += child.DownCount
		out.PendingCount += child.PendingCount
		out.MaintCount += child.MaintCount
		out.UnknownCount += child.UnknownCount
		out.PingCount += child.PingCount
		pingSum += child.AvgPing * float64(child.PingCount)
		if child.MinPing > 0 && (out.MinPing == 0 || child.MinPing < out.MinPing) {
			out.MinPing = child.MinPing
		}
		out.MaxPing = max(out.MaxPing, child.MaxPing)
	}
	if out.PingCount > 0 {
		out.AvgPing = pingSum / float64(out.PingCount)
	}
	return domain.ProbeHistoryProjection{Regional: out}, nil
}

func historyResolution(resolution string) time.Duration {
	switch resolution {
	case domain.DirtyResolutionOverall, domain.DirtyResolution1m:
		return time.Minute
	case domain.DirtyResolution1h:
		return time.Hour
	case domain.DirtyResolution1d:
		return 24 * time.Hour
	default:
		return 0
	}
}
