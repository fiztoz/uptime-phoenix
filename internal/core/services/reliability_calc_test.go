package services

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

var relBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(h float64) time.Time { return relBase.Add(time.Duration(h * float64(time.Hour))) }

func hb(id int64, h float64, s domain.Status) *domain.Heartbeat {
	return &domain.Heartbeat{ID: id, MonitorID: 1, Status: s, Time: at(h), Important: true}
}

func statusPtr(s domain.Status) *domain.Status { return &s }

// window [0h, 10h)
func win() (time.Time, time.Time) { return at(0), at(10) }

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if d := got - want; d > 0.5 || d < -0.5 {
		t.Fatalf("got %.3f, want %.3f", got, want)
	}
}

func TestReliability_NoDataLeadingNil_IsInsufficientNotFullyUp(t *testing.T) {
	from, to := win()
	m := ComputeReliability(ReliabilityInput{From: from, To: to, Leading: nil})
	if m.AvailabilityPercent != nil {
		t.Fatalf("availability should be nil for no data, got %v", *m.AvailabilityPercent)
	}
	if m.Qualification != QualificationInsufficient {
		t.Fatalf("qualification = %s; want insufficient_data", m.Qualification)
	}
	approx(t, m.UnknownSeconds, 10*3600)
}

func TestReliability_LeadingUpWithoutObservations_IsInsufficient(t *testing.T) {
	from, to := win()
	zero := float64(0)
	m := ComputeReliability(ReliabilityInput{
		From:               from,
		To:                 to,
		Leading:            statusPtr(domain.StatusUp),
		ObservationSeconds: &zero,
	})
	if m.AvailabilityPercent != nil {
		t.Fatalf("availability should be nil without observations, got %v", *m.AvailabilityPercent)
	}
	if m.Qualification != QualificationInsufficient {
		t.Fatalf("qualification = %s; want insufficient_data", m.Qualification)
	}
}

func TestReliability_LeadingUpNoTransitions_FullAvailability(t *testing.T) {
	from, to := win()
	m := ComputeReliability(ReliabilityInput{From: from, To: to, Leading: statusPtr(domain.StatusUp)})
	if m.AvailabilityPercent == nil || *m.AvailabilityPercent != 100 {
		t.Fatalf("availability = %v; want 100", m.AvailabilityPercent)
	}
	approx(t, m.CoveragePercent, 100)
	if m.Qualification != QualificationQualified {
		t.Fatalf("qualification = %s; want qualified", m.Qualification)
	}
	if m.OutageCount != 0 {
		t.Fatalf("outage count = %d; want 0", m.OutageCount)
	}
}

func TestReliability_MaintenanceOnly_EligibleZeroInsufficient(t *testing.T) {
	from, to := win()
	m := ComputeReliability(ReliabilityInput{From: from, To: to, Leading: statusPtr(domain.StatusMaintenance)})
	if m.AvailabilityPercent != nil {
		t.Fatalf("availability should be nil under full maintenance, got %v", *m.AvailabilityPercent)
	}
	approx(t, m.MaintSeconds, 10*3600)
	approx(t, m.EligibleSeconds, 0)
	if m.Qualification != QualificationInsufficient {
		t.Fatalf("qualification = %s; want insufficient", m.Qualification)
	}
}

func TestReliability_RangeStartsWhileDown_CountsDowntimeNotOutage(t *testing.T) {
	from, to := win()
	m := ComputeReliability(ReliabilityInput{
		From: from, To: to,
		Leading:              statusPtr(domain.StatusDown),
		LeadingConfirmedDown: true,
	})
	approx(t, m.DownSeconds, 10*3600)
	if m.AvailabilityPercent == nil || *m.AvailabilityPercent != 0 {
		t.Fatalf("availability = %v; want 0", m.AvailabilityPercent)
	}
	if m.OutageCount != 0 {
		t.Fatalf("outage count = %d; want 0 (carried from before range)", m.OutageCount)
	}
}

func TestReliability_OngoingDown_ClippedAtTo(t *testing.T) {
	from, to := win()
	m := ComputeReliability(ReliabilityInput{
		From: from, To: to,
		Leading:     statusPtr(domain.StatusUp),
		Transitions: []*domain.Heartbeat{hb(1, 8, domain.StatusDown)},
	})
	approx(t, m.UpSeconds, 8*3600)
	approx(t, m.DownSeconds, 2*3600)
	approx(t, *m.AvailabilityPercent, 80)
	if m.OutageCount != 1 {
		t.Fatalf("outage count = %d; want 1", m.OutageCount)
	}
	if m.FlapCount != 1 {
		t.Fatalf("flap count = %d; want 1 (down transition only, no recovery)", m.FlapCount)
	}
}

func TestReliability_TransientPending_NotAnOutage(t *testing.T) {
	from, to := win()
	m := ComputeReliability(ReliabilityInput{
		From: from, To: to,
		Leading: statusPtr(domain.StatusUp),
		Transitions: []*domain.Heartbeat{
			hb(1, 4, domain.StatusPending),
			hb(2, 5, domain.StatusUp),
		},
	})
	if m.OutageCount != 0 {
		t.Fatalf("outage count = %d; want 0 for transient pending", m.OutageCount)
	}
	approx(t, m.UpSeconds, 9*3600)
	approx(t, m.UnknownSeconds, 1*3600)
	if m.AvailabilityPercent == nil || *m.AvailabilityPercent != 100 {
		t.Fatalf("availability = %v; want 100 (blip is unknown, not down)", m.AvailabilityPercent)
	}
}

func TestReliability_ConfirmedRetrySequence_BackdatesDowntime(t *testing.T) {
	from, to := win()
	m := ComputeReliability(ReliabilityInput{
		From: from, To: to,
		Leading: statusPtr(domain.StatusUp),
		Transitions: []*domain.Heartbeat{
			hb(1, 4, domain.StatusPending), // retry window begins
			hb(2, 5, domain.StatusDown),    // confirmed
			hb(3, 6, domain.StatusUp),      // recovery
		},
	})
	// Downtime backdated to the first PENDING at 4h, not the confirmed DOWN at 5h.
	approx(t, m.DownSeconds, 2*3600)
	approx(t, m.UpSeconds, 8*3600)
	approx(t, *m.AvailabilityPercent, 80)
	if m.OutageCount != 1 {
		t.Fatalf("outage count = %d; want 1", m.OutageCount)
	}
	if m.FlapCount != 2 {
		t.Fatalf("flap count = %d; want 2 (down + recovery)", m.FlapCount)
	}
}

func TestReliability_MaintenanceExcludedFromDenominator(t *testing.T) {
	from, to := win()
	m := ComputeReliability(ReliabilityInput{
		From: from, To: to,
		Leading: statusPtr(domain.StatusUp),
		Transitions: []*domain.Heartbeat{
			hb(1, 4, domain.StatusMaintenance),
			hb(2, 6, domain.StatusUp),
		},
	})
	approx(t, m.UpSeconds, 8*3600)
	approx(t, m.MaintSeconds, 2*3600)
	approx(t, m.EligibleSeconds, 8*3600)
	if *m.AvailabilityPercent != 100 {
		t.Fatalf("availability = %v; want 100 (maintenance not counted against)", *m.AvailabilityPercent)
	}
}

func TestReliability_LowCoverageNewMonitor_InsufficientDespite100(t *testing.T) {
	from, to := win()
	// Leading unknown, first observation only at 9h: 90% of the window is unknown.
	m := ComputeReliability(ReliabilityInput{
		From: from, To: to,
		Leading:     nil,
		Transitions: []*domain.Heartbeat{hb(1, 9, domain.StatusUp)},
	})
	if m.AvailabilityPercent == nil || *m.AvailabilityPercent != 100 {
		t.Fatalf("availability = %v; want 100 over known time", m.AvailabilityPercent)
	}
	approx(t, m.CoveragePercent, 10)
	if m.Qualification != QualificationInsufficient {
		t.Fatalf("qualification = %s; want insufficient_data (new monitor must not top the ranking)", m.Qualification)
	}
}

func TestReliability_EqualTimestamps_DeterministicByID(t *testing.T) {
	from, to := win()
	// Two transitions at the same instant; ID ordering makes DOWN the survivor.
	m1 := ComputeReliability(ReliabilityInput{
		From: from, To: to,
		Leading: statusPtr(domain.StatusUp),
		Transitions: []*domain.Heartbeat{
			hb(1, 5, domain.StatusUp),
			hb(2, 5, domain.StatusDown),
		},
	})
	m2 := ComputeReliability(ReliabilityInput{
		From: from, To: to,
		Leading: statusPtr(domain.StatusUp),
		Transitions: []*domain.Heartbeat{
			hb(2, 5, domain.StatusDown),
			hb(1, 5, domain.StatusUp),
		},
	})
	if m1.DownSeconds != m2.DownSeconds {
		t.Fatalf("non-deterministic tie-break: %.0f vs %.0f", m1.DownSeconds, m2.DownSeconds)
	}
	approx(t, m1.DownSeconds, 5*3600)
}
