package domain

import (
	"testing"
	"time"
)

func TestProbeWatchdogCustomTimingPreservesPositiveWireIntervals(t *testing.T) {
	for _, test := range []struct {
		loss    int32
		suspect time.Duration
	}{{90, 45 * time.Second}, {60, 45 * time.Second}, {45, 22500 * time.Millisecond}, {1, 500 * time.Millisecond}} {
		s := DefaultProbeWatchdogSettings()
		s.LostAfterSeconds = test.loss
		got := s.Timing()
		if ValidateProbeWatchdogSettings(s) != nil || got.SuspectAfter != test.suspect || got.LostAfter <= got.SuspectAfter || got.RecoverAfter != 30*time.Second {
			t.Fatalf("invalid custom timing: %+v", got)
		}
	}
}
