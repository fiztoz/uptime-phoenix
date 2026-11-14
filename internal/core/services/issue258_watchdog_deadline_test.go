package services

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestIssue258WatchdogLossStartsAtHealthNotDisconnect documents the deliberate
// deadline: a frame 13 seconds before a cut leaves 77 seconds, not another 90.
func TestIssue258WatchdogLossStartsAtHealthNotDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		frameAge int
	}{
		{"cut_at_health", 0},
		{"reported_77_seconds", 13},
		{"cut_before_next_frame", 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := watchdogTimer(t, true)
			const lastHealth = 10
			watchdogHealth(t, w, lastHealth, true)
			cut := lastHealth + tc.frameAge
			if err := w.Disconnect(time.Duration(cut) * time.Second); err != nil {
				t.Fatal(err)
			}
			// Neither silence nor a degraded sample moves the health deadline.
			watchdogHealth(t, w, cut+1, false)
			watchdogState(t, w, lastHealth+89, false, domain.ProbeWatchdogSuspect, domain.ProbeWatchdogNoAction)
			watchdogState(t, w, lastHealth+90, false, domain.ProbeWatchdogLost, domain.ProbeWatchdogOpen)
		})
	}
}
