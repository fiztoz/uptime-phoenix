package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type recoveryStatusReader struct {
	statuses map[int64]domain.Status
	err      error
}

func (r recoveryStatusReader) StatusForMonitors(context.Context, []int64, time.Time) (map[int64]domain.Status, error) {
	return r.statuses, r.err
}

func TestRegionalRecoveryRequiresFreshOverallUp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses map[int64]domain.Status
		err      error
		want     int
	}{
		{name: "up", statuses: map[int64]domain.Status{7: domain.StatusUp}, want: 1},
		{name: "down", statuses: map[int64]domain.Status{7: domain.StatusDown}},
		{name: "unknown", statuses: map[int64]domain.Status{7: domain.StatusUnknown}},
		{name: "pending", statuses: map[int64]domain.Status{7: domain.StatusPending}},
		{name: "maintenance", statuses: map[int64]domain.Status{7: domain.StatusMaintenance}},
		{name: "removed remote assignment", statuses: map[int64]domain.Status{}},
		{name: "failed read", err: errors.New("database unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &countingResolver{}
			r := regionalRecovery{overall: recoveryStatusReader{statuses: tc.statuses, err: tc.err}, resolver: resolver}
			r.resolve(t.Context(), []int64{7, 7, 0})
			if len(resolver.ids) != tc.want {
				t.Fatalf("recovery calls=%d, want %d", len(resolver.ids), tc.want)
			}
		})
	}
}
