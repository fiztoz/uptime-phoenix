package services

import (
	"math"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func watchdogTimer(t *testing.T, armed bool) *ProbeWatchdogTimer {
	t.Helper()
	w, err := NewProbeWatchdogTimer(domain.DefaultProbeWatchdogTiming(), 0, armed)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func watchdogHealth(t *testing.T, w *ProbeWatchdogTimer, seconds int, healthy bool) {
	t.Helper()
	if err := w.Health(time.Duration(seconds)*time.Second, healthy); err != nil {
		t.Fatal(err)
	}
}
func watchdogState(t *testing.T, w *ProbeWatchdogTimer, seconds int, open bool, status domain.ProbeWatchdogStatus, action domain.ProbeWatchdogAction) {
	t.Helper()
	got, err := w.Evaluate(time.Duration(seconds)*time.Second, open)
	if err != nil || got.Status != status || got.Action != action {
		t.Fatalf("at %ds open=%v: %+v %v; want %s/%s", seconds, open, got, err, status, action)
	}
}

func TestProbeWatchdogStartupArmingAndExactBoundaries(t *testing.T) {
	w := watchdogTimer(t, false)
	watchdogHealth(t, w, 100, true)
	watchdogState(t, w, 1000, false, domain.ProbeWatchdogUnarmed, domain.ProbeWatchdogNoAction)
	if err := w.Arm(1000 * time.Second); err != nil {
		t.Fatal(err)
	}
	watchdogState(t, w, 1044, false, domain.ProbeWatchdogStarting, domain.ProbeWatchdogNoAction)
	watchdogState(t, w, 1045, false, domain.ProbeWatchdogSuspect, domain.ProbeWatchdogNoAction)
	if err := w.Arm(1080 * time.Second); err != nil {
		t.Fatal(err)
	}
	watchdogState(t, w, 1089, false, domain.ProbeWatchdogSuspect, domain.ProbeWatchdogNoAction)
	watchdogState(t, w, 1090, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
	// A failed persistence attempt repeats the same proposal. Once an incident
	// is durable, both firing and acked states pass open=true and cannot reopen.
	watchdogState(t, w, 1100, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
	watchdogState(t, w, 1100, true, domain.ProbeWatchdogLost, domain.ProbeWatchdogNoAction)
}

func TestProbeWatchdogApplicationFailureCannotRenewDeadline(t *testing.T) {
	w := watchdogTimer(t, true)
	watchdogHealth(t, w, 10, true)
	for seconds := 25; seconds <= 85; seconds += 15 {
		watchdogHealth(t, w, seconds, false)
	}
	watchdogState(t, w, 99, false, domain.ProbeWatchdogSuspect, domain.ProbeWatchdogNoAction)
	watchdogState(t, w, 100, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
}

func TestProbeWatchdogRecoveryRequiresSpanningHealthySamples(t *testing.T) {
	w := watchdogTimer(t, true)
	watchdogState(t, w, 90, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
	watchdogHealth(t, w, 100, true)
	watchdogState(t, w, 130, true, domain.ProbeWatchdogRecovering, domain.ProbeWatchdogNoAction)
	watchdogHealth(t, w, 130, true)
	watchdogState(t, w, 130, true, domain.ProbeWatchdogHealthy, domain.ProbeWatchdogResolve)
	// Successful resolution leaves no new lifecycle work while health remains valid.
	watchdogState(t, w, 131, false, domain.ProbeWatchdogHealthy, domain.ProbeWatchdogNoAction)
}

func TestProbeWatchdogRecoveryInterruptedByHealthGapOrDisconnect(t *testing.T) {
	for _, mode := range []string{"degraded", "disconnect", "silence"} {
		t.Run(mode, func(t *testing.T) {
			w := watchdogTimer(t, true)
			watchdogHealth(t, w, 10, true)
			switch mode {
			case "degraded":
				watchdogHealth(t, w, 25, false)
			case "disconnect":
				if err := w.Disconnect(25 * time.Second); err != nil {
					t.Fatal(err)
				}
			case "silence":
				watchdogState(t, w, 55, true, domain.ProbeWatchdogLost, domain.ProbeWatchdogNoAction)
			}
			watchdogHealth(t, w, 60, true)
			watchdogState(t, w, 60, true, domain.ProbeWatchdogRecovering, domain.ProbeWatchdogNoAction)
			watchdogHealth(t, w, 75, true)
			watchdogHealth(t, w, 89, true)
			watchdogState(t, w, 89, true, domain.ProbeWatchdogRecovering, domain.ProbeWatchdogNoAction)
			watchdogHealth(t, w, 90, true)
			watchdogState(t, w, 90, true, domain.ProbeWatchdogHealthy, domain.ProbeWatchdogResolve)
		})
	}
}

func TestProbeWatchdogRestartRetainsOpenIncidentWithoutImmediateRecovery(t *testing.T) {
	// Storage supplies armed=true and the existing incident. No previous process
	// elapsed values or wall-clock timestamps are reused as a local duration.
	w := watchdogTimer(t, true)
	watchdogState(t, w, 0, true, domain.ProbeWatchdogLost, domain.ProbeWatchdogNoAction)
	watchdogHealth(t, w, 1, true)
	watchdogHealth(t, w, 16, true)
	watchdogState(t, w, 30, true, domain.ProbeWatchdogRecovering, domain.ProbeWatchdogNoAction)
	watchdogHealth(t, w, 31, true)
	watchdogState(t, w, 31, true, domain.ProbeWatchdogHealthy, domain.ProbeWatchdogResolve)
}

func TestProbeWatchdogDelayedTickCannotEraseObservedOutage(t *testing.T) {
	w := watchdogTimer(t, true)
	watchdogHealth(t, w, 0, true)
	// A fresh sample arrives after the loss threshold but before a delayed timer
	// callback. Latch the missed loss so a DB stall cannot erase the whole outage.
	watchdogHealth(t, w, 100, true)
	watchdogState(t, w, 100, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
	watchdogState(t, w, 100, true, domain.ProbeWatchdogRecovering, domain.ProbeWatchdogNoAction)
	watchdogHealth(t, w, 115, true)
	watchdogHealth(t, w, 130, true)
	watchdogState(t, w, 130, true, domain.ProbeWatchdogHealthy, domain.ProbeWatchdogResolve)
}

func TestProbeWatchdogRejectsClockRegressionAndOverflow(t *testing.T) {
	w := watchdogTimer(t, true)
	watchdogHealth(t, w, 10, true)
	if err := w.Health(9*time.Second, true); err == nil {
		t.Fatal("regressing elapsed time accepted")
	}
	if _, err := w.Evaluate(-time.Second, false); err == nil {
		t.Fatal("negative elapsed time accepted")
	}
	watchdogState(t, w, 100, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
	last := time.Duration(math.MaxInt64)
	huge, err := NewProbeWatchdogTimer(domain.DefaultProbeWatchdogTiming(), last-90*time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := huge.Evaluate(last, false)
	if err != nil || got.Action != domain.ProbeWatchdogOpen {
		t.Fatal("duration overflow", got, err)
	}
}

func TestProbeWatchdogHandoffPreservesMeasuredLossWithoutWallClock(t *testing.T) {
	w := watchdogTimer(t, true)
	checkpoint, err := w.Checkpoint(85 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	next, err := RestoreProbeWatchdogTimer(domain.DefaultProbeWatchdogTiming(), 0, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	watchdogState(t, next, 4, false, domain.ProbeWatchdogSuspect, domain.ProbeWatchdogNoAction)
	watchdogState(t, next, 5, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
	// Handoff after a missed loss and a returning healthy sample must not erase
	// the still-uncommitted open proposal just because current age became zero.
	returning := watchdogTimer(t, true)
	watchdogHealth(t, returning, 100, true)
	checkpoint, err = returning.Checkpoint(100 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	next, err = RestoreProbeWatchdogTimer(domain.DefaultProbeWatchdogTiming(), 0, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	watchdogState(t, next, 0, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
}

func TestProbeWatchdogHandoffCannotInheritRecoveryStreak(t *testing.T) {
	w := watchdogTimer(t, true)
	watchdogHealth(t, w, 0, true)
	watchdogHealth(t, w, 20, true)
	checkpoint, err := w.Checkpoint(20 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	next, err := RestoreProbeWatchdogTimer(domain.DefaultProbeWatchdogTiming(), 0, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	watchdogHealth(t, next, 10, true)
	watchdogState(t, next, 10, true, domain.ProbeWatchdogRecovering, domain.ProbeWatchdogNoAction)
	watchdogHealth(t, next, 25, true)
	watchdogHealth(t, next, 40, true)
	watchdogState(t, next, 40, true, domain.ProbeWatchdogHealthy, domain.ProbeWatchdogResolve)
}

func TestProbeWatchdogInvalidCheckpointCannotArmUnknownInstallation(t *testing.T) {
	for _, checkpoint := range []domain.ProbeWatchdogCheckpoint{
		{Armed: false, LossElapsed: time.Second}, {Armed: false, PendingLoss: true}, {Armed: true, LossElapsed: -time.Second},
	} {
		if _, err := RestoreProbeWatchdogTimer(domain.DefaultProbeWatchdogTiming(), 0, checkpoint); err == nil {
			t.Fatal("invalid checkpoint accepted", checkpoint)
		}
	}
	timing := domain.DefaultProbeWatchdogTiming()
	timing.LostAfter = timing.SuspectAfter
	if _, err := NewProbeWatchdogTimer(timing, 0, true); err == nil {
		t.Fatal("loss must follow the suspect threshold")
	}
}
