package services

import (
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Reliability qualification constants. These are named product constants, not
// magic numbers buried in SQL or UI code, so the "is this row trustworthy?"
// decision is testable in exactly one place (see INSIGHTS-PAGE-REVIEW-2.md §6).
const (
	// MinCoverageToQualify is the fraction of the eligible (non-maintenance)
	// window that must be backed by trustworthy observations before a monitor is
	// allowed to occupy a strongest/weakest ranking position. Below it the row is
	// reported as insufficient_data and sorted outside the ranked positions.
	MinCoverageToQualify = 0.80

	// MinObservationSamples prevents a tiny number of checks from qualifying a
	// row merely because one unusually long monitor interval covers the window.
	MinObservationSamples = 3

	// reliabilityFlapWindow is the trailing window over which "currently unstable"
	// (flapping) is measured, unless the selected period is shorter. Confirmed
	// UP<->DOWN transitions inside it are counted.
	reliabilityFlapWindow = 24 * time.Hour
)

// ReliabilityQualification is the trust state of a computed reliability row.
type ReliabilityQualification string

const (
	// QualificationQualified means coverage cleared MinCoverageToQualify and the
	// row may be ranked.
	QualificationQualified ReliabilityQualification = "qualified"
	// QualificationInsufficient means there is not enough trustworthy observation
	// of the window to rank the monitor. It is never a fabricated 100%.
	QualificationInsufficient ReliabilityQualification = "insufficient_data"
)

// ReliabilityInput is the pure input to the interval calculator for one monitor
// over one UTC window. It carries only domain data — no repository, no clock.
type ReliabilityInput struct {
	// From and To bound the window. Callers MUST pass UTC (rule 6). To is
	// exclusive; an ongoing state is clipped at To.
	From time.Time
	To   time.Time

	// Leading is the effective status entering the window, taken from the last
	// transition strictly before From. nil means "unknown before the first
	// in-range observation" — the segment [From, firstTransition) is then counted
	// as unknown rather than assumed UP, so a brand-new monitor cannot masquerade
	// as 100% available.
	Leading *domain.Status

	// LeadingConfirmedDown is true when the Leading run was already a CONFIRMED
	// outage before From (its originating sequence reached DOWN). It lets the
	// calculator attribute pre-range downtime correctly without counting a new
	// outage transition inside the window.
	LeadingConfirmedDown bool

	// Transitions are the important (status-changed) heartbeats inside [From, To],
	// ascending by time. Between two transitions the effective status is constant
	// and equal to the earlier transition's status.
	Transitions []*domain.Heartbeat

	// ObservationSeconds is an observation-based coverage estimate supplied by
	// the rollup read model. nil means the caller has no rollup coverage data;
	// non-nil zero means the selected window has no trustworthy observations.
	// It is intentionally separate from status intervals: a last-known UP state
	// must not make an installation outage look fully observed.
	ObservationSeconds *float64
	// ObservationCount is the number of checks represented by the same rollup
	// read model. It participates in the named qualification rule.
	ObservationCount int
}

// ReliabilityMetrics is the computed, wire-agnostic result for one monitor.
type ReliabilityMetrics struct {
	UpSeconds       float64
	DownSeconds     float64
	MaintSeconds    float64
	UnknownSeconds  float64
	EligibleSeconds float64 // total window minus maintenance
	KnownSeconds    float64 // up + confirmed down (the availability denominator)

	// AvailabilityPercent is nil when KnownSeconds is zero: no trustworthy
	// observation is evidence for neither 0% nor 100% (matches statuspage_service).
	AvailabilityPercent *float64
	CoveragePercent     float64

	// OutageCount counts confirmed-DOWN runs whose entry transition falls at or
	// after From. A run already active at From (carried from Leading) contributes
	// its in-range downtime but is not counted as a new outage.
	OutageCount int
	// FlapCount counts confirmed UP<->DOWN transitions inside the flap window.
	FlapCount int

	Qualification ReliabilityQualification
}

// segment is an internal [start,end) slice of the window with one effective
// status. observed=false marks a synthesised unknown gap segment.
type segment struct {
	start    time.Time
	end      time.Time
	status   domain.Status
	observed bool
}

// ComputeReliability turns an ordered transition timeline into time-weighted
// reliability metrics. It is pure and deterministic: same input, same output.
//
// The calculation is deliberately NOT the check-count ratio used by
// AggregateService.GetUptimePercent, which is comparable only against the same
// monitor. Ranking compares monitors with different check intervals against each
// other, so availability here is duration-weighted (see INSIGHTS-PAGE-REVIEW.md
// Concern 1).
func ComputeReliability(in ReliabilityInput) ReliabilityMetrics {
	from := in.From.UTC()
	to := in.To.UTC()
	m := ReliabilityMetrics{Qualification: QualificationInsufficient}
	if !to.After(from) {
		return m
	}

	segments := buildSegments(from, to, in.Leading, in.Transitions)
	total := to.Sub(from).Seconds()

	// A "failure run" is a maximal contiguous stretch of non-UP, non-MAINT
	// segments (PENDING/DOWN). Runs let us backdate a confirmed outage to its
	// first PENDING observation and keep a transient PENDING blip out of the
	// confirmed-down total.
	flapStart := to.Add(-reliabilityFlapWindow)
	if flapStart.Before(from) {
		flapStart = from
	}

	i := 0
	for i < len(segments) {
		seg := segments[i]
		switch {
		case seg.status == domain.StatusUp:
			m.UpSeconds += seg.end.Sub(seg.start).Seconds()
			i++
		case seg.status == domain.StatusMaintenance:
			m.MaintSeconds += seg.end.Sub(seg.start).Seconds()
			i++
		case !seg.observed:
			// Synthesised unknown gap.
			m.UnknownSeconds += seg.end.Sub(seg.start).Seconds()
			i++
		default:
			// Start of a failure run (PENDING/DOWN observed segments).
			j := i
			hasDown := false
			for j < len(segments) &&
				segments[j].observed &&
				segments[j].status != domain.StatusUp &&
				segments[j].status != domain.StatusMaintenance {
				if segments[j].status == domain.StatusDown {
					hasDown = true
				}
				j++
			}
			runStart := segments[i].start
			runEnd := segments[j-1].end
			dur := runEnd.Sub(runStart).Seconds()

			leadingFailure := in.Leading != nil &&
				(*in.Leading == domain.StatusDown || *in.Leading == domain.StatusPending)
			carriedFromLeading := runStart.Equal(from) && leadingFailure
			confirmed := hasDown || (carriedFromLeading && in.LeadingConfirmedDown)

			if confirmed {
				m.DownSeconds += dur
				// Count a new outage only when the run BEGAN inside the window.
				if !carriedFromLeading {
					m.OutageCount++
					if !runStart.Before(flapStart) {
						m.FlapCount++ // UP -> DOWN transition in flap window
					}
				}
				// Recovery (DOWN -> UP) counts as a flap only when the run actually
				// ended before To (an ongoing outage has not recovered).
				if runEnd.Before(to) && !runEnd.Before(flapStart) {
					m.FlapCount++
				}
			} else {
				// Transient PENDING that never confirmed: not UP, not a confirmed
				// outage. Treat as unknown so it neither credits availability nor
				// penalises the monitor for a blip that recovered.
				m.UnknownSeconds += dur
			}
			i = j
		}
	}

	m.EligibleSeconds = total - m.MaintSeconds
	if m.EligibleSeconds < 0 {
		m.EligibleSeconds = 0
	}
	m.KnownSeconds = m.UpSeconds + m.DownSeconds

	if m.KnownSeconds > 0 {
		avail := m.UpSeconds / m.KnownSeconds * 100
		m.AvailabilityPercent = &avail
	}
	if m.EligibleSeconds > 0 {
		m.CoveragePercent = m.KnownSeconds / m.EligibleSeconds * 100
		if in.ObservationSeconds != nil {
			observed := *in.ObservationSeconds
			if observed < 0 {
				observed = 0
			}
			if observed > total {
				observed = total
			}
			observedCoverage := observed / m.EligibleSeconds * 100
			if observedCoverage < m.CoveragePercent {
				m.CoveragePercent = observedCoverage
			}
			if observed == 0 {
				m.KnownSeconds = 0
				m.AvailabilityPercent = nil
			}
		}
	}

	if m.EligibleSeconds > 0 &&
		m.CoveragePercent >= MinCoverageToQualify*100 &&
		(in.ObservationSeconds == nil || in.ObservationCount >= MinObservationSamples) {
		m.Qualification = QualificationQualified
	}
	return m
}

// buildSegments turns Leading + ordered transitions into a contiguous [from,to)
// segment list. The status of each transition holds until the next transition
// (or To). The leading segment [from, firstTransition) uses Leading, or is a
// synthesised unknown gap when Leading is nil.
func buildSegments(from, to time.Time, leading *domain.Status, transitions []*domain.Heartbeat) []segment {
	// Defensive copy + clamp + sort; callers should pass ascending in-range rows
	// but we do not trust that.
	pts := make([]*domain.Heartbeat, 0, len(transitions))
	for _, hb := range transitions {
		if hb == nil {
			continue
		}
		t := hb.Time.UTC()
		if t.Before(from) || !t.Before(to) {
			continue
		}
		pts = append(pts, hb)
	}
	sort.SliceStable(pts, func(a, b int) bool {
		ta, tb := pts[a].Time.UTC(), pts[b].Time.UTC()
		if ta.Equal(tb) {
			return pts[a].ID < pts[b].ID // deterministic tie-break (rule 8)
		}
		return ta.Before(tb)
	})

	segs := make([]segment, 0, len(pts)+1)

	// Leading segment.
	if len(pts) == 0 {
		if leading != nil {
			segs = append(segs, segment{start: from, end: to, status: *leading, observed: true})
		} else {
			segs = append(segs, segment{start: from, end: to, status: domain.StatusDown, observed: false})
		}
		return segs
	}

	firstT := pts[0].Time.UTC()
	if firstT.After(from) {
		if leading != nil {
			segs = append(segs, segment{start: from, end: firstT, status: *leading, observed: true})
		} else {
			segs = append(segs, segment{start: from, end: firstT, status: domain.StatusDown, observed: false})
		}
	}

	for idx, hb := range pts {
		end := to
		if idx+1 < len(pts) {
			end = pts[idx+1].Time.UTC()
		}
		start := hb.Time.UTC()
		if !end.After(start) {
			continue // zero/negative width (duplicate timestamps already ID-ordered)
		}
		segs = append(segs, segment{start: start, end: end, status: hb.Status, observed: true})
	}
	return segs
}
