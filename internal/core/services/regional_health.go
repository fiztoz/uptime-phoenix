package services

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ErrInvalidHealthEvidence indicates an invalid policy, empty quorum, or malformed input.
var ErrInvalidHealthEvidence = errors.New("invalid regional health evidence")

// EvaluateRetry applies the existing local confirmation rule without persistence or I/O.
// Callers provide previous state from the same regional assignment. This operation
// never reads pooled heartbeat history and must not run again on replayed telemetry.
func EvaluateRetry(previous *domain.RetryState, rawStatus domain.Status, maxRetries int) domain.RetryEvaluation {
	state := domain.RetryState{Status: rawStatus}
	if rawStatus == domain.StatusDown {
		state.DownCount = 1
		if previous != nil && previous.DownCount > 0 {
			state.DownCount = previous.DownCount
			if state.DownCount < math.MaxInt {
				state.DownCount++
			}
		}
		if maxRetries > 0 && state.DownCount <= maxRetries {
			state.Status = domain.StatusPending
		}
	}
	return domain.RetryEvaluation{State: state, Important: previous == nil || previous.Status != state.Status}
}

// EvaluateObservation applies accepted maintenance then retry confirmation.
// Maintenance never reads another region's history; it only replaces this
// assignment's effective status and resets its consecutive-failure count.
func EvaluateObservation(previous *domain.RetryState, rawStatus domain.Status, inMaintenance bool, maxRetries int) domain.RetryEvaluation {
	if inMaintenance {
		rawStatus = domain.StatusMaintenance
	}
	return EvaluateRetry(previous, rawStatus, maxRetries)
}

// RegionalFreshnessWindow computes max(90, 2*max(interval,retry)+ceil(timeout)) seconds.
// Reject unrepresentable configuration rather than overflowing time.Duration.
func RegionalFreshnessWindow(intervalSeconds, retrySeconds int, timeoutSeconds float64) (time.Duration, error) {
	if intervalSeconds <= 0 || retrySeconds < 0 || timeoutSeconds < 0 || math.IsNaN(timeoutSeconds) || math.IsInf(timeoutSeconds, 0) {
		return 0, fmt.Errorf("freshness configuration: %w", ErrInvalidHealthEvidence)
	}
	seconds := max(90, 2*float64(max(intervalSeconds, retrySeconds))+math.Ceil(timeoutSeconds))
	if seconds > float64(math.MaxInt64/int64(time.Second)) {
		return 0, fmt.Errorf("freshness duration overflow: %w", ErrInvalidHealthEvidence)
	}
	return time.Duration(seconds) * time.Second, nil
}

// EvaluateMonitorHealth applies the ANY/ALL truth table to the complete assignment set.
// Evidence expires exactly at its freshness deadline. Future observations are unknown
// until their timestamp is reached; an explicit diagnostic can invalidate older data.
// Unknown and pending are projections only: callers must never treat them as recovery.
func EvaluateMonitorHealth(now time.Time, policy domain.HealthPolicy, evidence []domain.RegionalHealthEvidence) (domain.MonitorHealth, error) {
	if now.IsZero() || len(evidence) == 0 || (policy != domain.HealthPolicyAnyDown && policy != domain.HealthPolicyAllDown) {
		return domain.MonitorHealth{}, ErrInvalidHealthEvidence
	}
	now = now.UTC()
	counts := domain.ProbeHealthCounts{Assigned: len(evidence)}
	seen := make(map[string]struct{}, len(evidence))
	for _, region := range evidence {
		if region.ProbeID == "" {
			return domain.MonitorHealth{}, fmt.Errorf("missing probe identity: %w", ErrInvalidHealthEvidence)
		}
		if _, duplicate := seen[region.ProbeID]; duplicate {
			return domain.MonitorHealth{}, fmt.Errorf("duplicate probe identity: %w", ErrInvalidHealthEvidence)
		}
		seen[region.ProbeID] = struct{}{}
		if region.Status < domain.StatusDown || region.Status > domain.StatusUnknown || region.FreshFor < 0 {
			return domain.MonitorHealth{}, fmt.Errorf("invalid regional status or freshness: %w", ErrInvalidHealthEvidence)
		}
		if region.Paused {
			counts.Paused++
			continue
		}
		status := region.Status
		observedAt := region.ObservedAt.UTC()
		if region.UnknownReason != "" || observedAt.IsZero() || observedAt.After(now) || region.FreshFor == 0 || now.Sub(observedAt) >= region.FreshFor {
			status = domain.StatusUnknown
		}
		switch status {
		case domain.StatusUp:
			counts.Up++
		case domain.StatusDown:
			counts.Down++
		case domain.StatusPending:
			counts.Pending++
		case domain.StatusMaintenance:
			counts.Maintenance++
		case domain.StatusUnknown:
			counts.Unknown++
		}
	}
	result := domain.MonitorHealth{Status: domain.StatusUnknown, Counts: counts}
	active := counts.Assigned - counts.Paused - counts.Maintenance
	switch {
	case counts.Paused == counts.Assigned:
		result.Reason = "no_active_assignments"
	case active == 0:
		result.Status = domain.StatusMaintenance
	case policy == domain.HealthPolicyAnyDown && counts.Down > 0:
		result.Status = domain.StatusDown
	case policy == domain.HealthPolicyAllDown && counts.Up > 0:
		result.Status = domain.StatusUp
	case counts.Unknown > 0:
		result.Reason = "incomplete_evidence"
	case counts.Pending > 0:
		result.Status = domain.StatusPending
		result.Reason = "unconfirmed_checks"
	case counts.Up == active:
		result.Status = domain.StatusUp
	case counts.Down == active:
		result.Status = domain.StatusDown
	}
	return result, nil
}

// CalculateHealthCoverage evaluates duration-based uptime and evidence coverage.
// PENDING and paused time count as unknown; maintenance is excluded from both ratios.
func CalculateHealthCoverage(d domain.HealthDurations) (domain.HealthCoverage, error) {
	var total time.Duration
	for _, duration := range []time.Duration{d.Up, d.Down, d.Pending, d.Unknown, d.Maintenance, d.Paused} {
		if duration < 0 || duration > time.Duration(math.MaxInt64)-total {
			return domain.HealthCoverage{}, fmt.Errorf("invalid history duration: %w", ErrInvalidHealthEvidence)
		}
		total += duration
	}
	result := domain.HealthCoverage{Known: d.Up + d.Down, Unknown: d.Unknown + d.Pending + d.Paused, Maintenance: d.Maintenance}
	if result.Known > 0 {
		percent := 100 * float64(d.Up) / float64(result.Known)
		result.UptimePercent = &percent
	}
	if denominator := result.Known + result.Unknown; denominator > 0 {
		percent := 100 * float64(result.Known) / float64(denominator)
		result.CoveragePercent = &percent
	}
	return result, nil
}
