package services

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestEvaluateMonitorHealthPolicyTable(t *testing.T) {
	up, down, pending, unknown, maintenance := domain.StatusUp, domain.StatusDown, domain.StatusPending, domain.StatusUnknown, domain.StatusMaintenance
	tests := []struct {
		name             string
		evidence         []domain.Status
		anyDown, allDown domain.Status
	}{
		{"all up", []domain.Status{up, up}, up, up},
		{"regional disagreement", []domain.Status{down, up}, down, up},
		{"all down", []domain.Status{down, down}, down, down},
		{"down unknown", []domain.Status{down, unknown}, down, unknown},
		{"down pending", []domain.Status{down, pending}, down, pending},
		{"up unknown", []domain.Status{up, unknown}, unknown, up},
		{"up pending", []domain.Status{up, pending}, pending, up},
		{"unknown", []domain.Status{unknown, unknown}, unknown, unknown},
		{"pending", []domain.Status{pending, pending}, pending, pending},
		{"unknown pending", []domain.Status{unknown, pending}, unknown, unknown},
		{"all maintenance", []domain.Status{maintenance, maintenance}, maintenance, maintenance},
		{"maintenance with down", []domain.Status{maintenance, down}, down, down},
		{"maintenance with unknown", []domain.Status{maintenance, unknown}, unknown, unknown},
		{"unknown cannot override ANY down", []domain.Status{up, down, unknown, pending}, down, up},
	}
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := make([]domain.RegionalHealthEvidence, len(test.evidence))
			for i, status := range test.evidence {
				evidence[i] = domain.RegionalHealthEvidence{ProbeID: fmt.Sprint(i), Status: status, ObservedAt: now, FreshFor: 90 * time.Second}
			}
			original := append([]domain.RegionalHealthEvidence(nil), evidence...)
			for policy, want := range map[domain.HealthPolicy]domain.Status{domain.HealthPolicyAnyDown: test.anyDown, domain.HealthPolicyAllDown: test.allDown} {
				got, err := EvaluateMonitorHealth(now, policy, evidence)
				if err != nil || got.Status != want {
					t.Fatalf("%s: got %+v, %v; want %s", policy, got, err, want)
				}
				c := got.Counts
				if c.Assigned != len(evidence) || c.Assigned != c.Up+c.Down+c.Pending+c.Unknown+c.Maintenance+c.Paused {
					t.Fatalf("counts do not partition assignments: %+v", c)
				}
			}
			if !reflect.DeepEqual(evidence, original) {
				t.Fatal("evaluation mutated evidence")
			}
		})
	}
}

func TestEvaluateMonitorHealthFreshnessAndPause(t *testing.T) {
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		evidence domain.RegionalHealthEvidence
		want     domain.Status
		reason   string
	}{
		{"fresh before deadline", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now.Add(-90*time.Second + time.Nanosecond), FreshFor: 90 * time.Second}, domain.StatusUp, ""},
		{"expired at deadline", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now.Add(-90 * time.Second), FreshFor: 90 * time.Second}, domain.StatusUnknown, "incomplete_evidence"},
		{"future timestamp", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now.Add(time.Nanosecond), FreshFor: 90 * time.Second}, domain.StatusUnknown, "incomplete_evidence"},
		{"no first sample", domain.RegionalHealthEvidence{FreshFor: 90 * time.Second}, domain.StatusUnknown, "incomplete_evidence"},
		{"no freshness budget", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now}, domain.StatusUnknown, "incomplete_evidence"},
		{"invalidated by stream reset", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now, FreshFor: 90 * time.Second, UnknownReason: "stream_reset"}, domain.StatusUnknown, "incomplete_evidence"},
		{"paused", domain.RegionalHealthEvidence{Status: domain.StatusDown, Paused: true}, domain.StatusUnknown, "no_active_assignments"},
		{"local timezone instant", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now.In(time.FixedZone("UTC+7", 7*3600)), FreshFor: 90 * time.Second}, domain.StatusUp, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.evidence.ProbeID = "local"
			got, err := EvaluateMonitorHealth(now, domain.HealthPolicyAnyDown, []domain.RegionalHealthEvidence{test.evidence})
			if err != nil || got.Status != test.want || got.Reason != test.reason {
				t.Fatalf("got %+v, %v; want %s/%s", got, err, test.want, test.reason)
			}
		})
	}
	got, err := EvaluateMonitorHealth(now, domain.HealthPolicyAllDown, []domain.RegionalHealthEvidence{
		{ProbeID: "paused", Paused: true, Status: domain.StatusUp},
		{ProbeID: "active", Status: domain.StatusDown, ObservedAt: now, FreshFor: time.Minute},
	})
	if err != nil || got.Status != domain.StatusDown || got.Counts.Paused != 1 {
		t.Fatalf("paused counted as quorum: %+v %v", got, err)
	}
}

func TestEvaluateMonitorHealthRejectsInvalidInput(t *testing.T) {
	valid := domain.RegionalHealthEvidence{ProbeID: "local", Status: domain.StatusUp}
	for _, test := range []struct {
		now      time.Time
		policy   domain.HealthPolicy
		evidence []domain.RegionalHealthEvidence
	}{
		{time.Time{}, domain.HealthPolicyAnyDown, []domain.RegionalHealthEvidence{valid}},
		{time.Now(), "quorum", []domain.RegionalHealthEvidence{valid}},
		{time.Now(), domain.HealthPolicyAnyDown, nil},
		{time.Now(), domain.HealthPolicyAnyDown, []domain.RegionalHealthEvidence{valid, valid}},
		{time.Now(), domain.HealthPolicyAnyDown, []domain.RegionalHealthEvidence{{Status: domain.StatusUp}}},
		{time.Now(), domain.HealthPolicyAnyDown, []domain.RegionalHealthEvidence{{ProbeID: "local", Status: 99}}},
		{time.Now(), domain.HealthPolicyAnyDown, []domain.RegionalHealthEvidence{{ProbeID: "local", FreshFor: -1}}},
	} {
		if _, err := EvaluateMonitorHealth(test.now, test.policy, test.evidence); !errors.Is(err, ErrInvalidHealthEvidence) {
			t.Fatalf("got error %v", err)
		}
	}
}

func TestRegionalFreshnessWindow(t *testing.T) {
	for _, test := range []struct {
		interval, retry int
		timeout         float64
		want            time.Duration
	}{
		{1, 1, 1, 90 * time.Second}, {60, 10, 10.1, 131 * time.Second}, {10, 120, 0.1, 241 * time.Second},
	} {
		got, err := RegionalFreshnessWindow(test.interval, test.retry, test.timeout)
		if err != nil || got != test.want {
			t.Fatalf("got %v %v want %v", got, err, test.want)
		}
	}
	for _, test := range []struct {
		interval, retry int
		timeout         float64
	}{
		{0, 1, 1}, {1, -1, 1}, {1, 1, -1}, {1, 1, math.NaN()}, {1, 1, math.Inf(1)}, {math.MaxInt, 1, 1},
	} {
		if _, err := RegionalFreshnessWindow(test.interval, test.retry, test.timeout); !errors.Is(err, ErrInvalidHealthEvidence) {
			t.Fatalf("got error %v", err)
		}
	}
}

func TestEvaluateRetryIndependentRegions(t *testing.T) {
	// UP samples from the local assignment must never reset the remote failure count.
	var local, remote *domain.RetryState
	for i := 1; i <= 4; i++ {
		localNext := EvaluateRetry(local, domain.StatusUp, 2)
		remoteNext := EvaluateRetry(remote, domain.StatusDown, 2)
		local, remote = &localNext.State, &remoteNext.State
		want := domain.StatusPending
		if i > 2 {
			want = domain.StatusDown
		}
		if local.Status != domain.StatusUp || local.DownCount != 0 || remote.Status != want || remote.DownCount != i {
			t.Fatalf("iteration %d local=%+v remote=%+v", i, local, remote)
		}
	}
	for _, resetStatus := range []domain.Status{domain.StatusUp, domain.StatusMaintenance, domain.StatusPending} {
		reset := EvaluateRetry(remote, resetStatus, 2)
		if reset.State.DownCount != 0 || reset.State.Status != resetStatus || !reset.Important {
			t.Fatalf("reset: %+v", reset)
		}
		next := EvaluateRetry(&reset.State, domain.StatusDown, 2)
		if next.State.Status != domain.StatusPending || next.State.DownCount != 1 {
			t.Fatalf("new retry window: %+v", next)
		}
	}
	noRetry := EvaluateRetry(nil, domain.StatusDown, 0)
	if noRetry.State.Status != domain.StatusDown || !noRetry.Important {
		t.Fatalf("no retry: %+v", noRetry)
	}
	same := EvaluateRetry(&noRetry.State, domain.StatusDown, 0)
	if same.Important {
		t.Fatal("unchanged status marked important")
	}
	saturated := EvaluateRetry(&domain.RetryState{Status: domain.StatusDown, DownCount: math.MaxInt}, domain.StatusDown, 0)
	if saturated.State.DownCount != math.MaxInt || saturated.State.Status != domain.StatusDown {
		t.Fatalf("counter overflow: %+v", saturated)
	}
}

func TestCalculateHealthCoverage(t *testing.T) {
	got, err := CalculateHealthCoverage(domain.HealthDurations{Up: 30 * time.Second, Down: 10 * time.Second, Unknown: 5 * time.Second, Pending: 10 * time.Second, Paused: 5 * time.Second, Maintenance: 40 * time.Second})
	if err != nil || got.Known != 40*time.Second || got.Unknown != 20*time.Second || got.Maintenance != 40*time.Second || got.UptimePercent == nil || *got.UptimePercent != 75 || got.CoveragePercent == nil || math.Abs(*got.CoveragePercent-200.0/3) > 0.000001 {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, durations := range []domain.HealthDurations{{}, {Maintenance: time.Hour}, {Unknown: time.Hour}, {Paused: time.Hour}, {Pending: time.Hour}} {
		result, calcErr := CalculateHealthCoverage(durations)
		if calcErr != nil || result.UptimePercent != nil {
			t.Fatalf("no known time: %+v %v", result, calcErr)
		}
		if result.Unknown == 0 && result.CoveragePercent != nil {
			t.Fatalf("undefined coverage: %+v", result)
		}
		if result.Unknown > 0 && (result.CoveragePercent == nil || *result.CoveragePercent != 0) {
			t.Fatalf("unknown time coverage: %+v", result)
		}
	}
	for _, durations := range []domain.HealthDurations{{Up: -1}, {Up: time.Duration(math.MaxInt64), Down: 1}} {
		if _, calcErr := CalculateHealthCoverage(durations); !errors.Is(calcErr, ErrInvalidHealthEvidence) {
			t.Fatalf("invalid durations accepted: %v", calcErr)
		}
	}
}
