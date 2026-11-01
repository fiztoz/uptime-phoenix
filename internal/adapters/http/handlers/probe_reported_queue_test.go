package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestProbeViewReportedQueue pins #157 on the wire: the probe-reported queue
// is exposed only as reported_* fields with the hub time it was accepted.
// Null means never reported, zero is an empty queue, and the hub-tracked
// queue_bytes/oldest_queued_at stay null because the hub still does not
// verify a probe's backlog itself.
func TestProbeViewReportedQueue(t *testing.T) {
	reportedAt := time.Date(2026, 10, 28, 9, 40, 15, 0, time.UTC)
	oldest := time.Date(2026, 10, 28, 9, 35, 0, 123456000, time.UTC)
	for _, tc := range []struct {
		name   string
		queue  *domain.ProbeReportedQueue
		bytes  any
		oldest any
		at     any
	}{
		{"NeverReported", nil, nil, nil, nil},
		{"Empty", &domain.ProbeReportedQueue{ReportedAt: reportedAt}, float64(0), nil, "2026-10-28T09:40:15Z"},
		{"Queued", &domain.ProbeReportedQueue{ProbeQueueReport: domain.ProbeQueueReport{QueueBytes: 4096, OldestQueuedAt: &oldest}, ReportedAt: reportedAt},
			float64(4096), "2026-10-28T09:35:00.123456Z", "2026-10-28T09:40:15Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote, _ := fleetFixtureEntries()
			remote.Summary.ReportedQueue = tc.queue
			for name, view := range map[string]any{"list": toProbeView(remote), "detail": toProbeDetailView(remote)} {
				data, err := json.Marshal(view)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				for field, want := range map[string]any{
					"reported_queue_bytes": tc.bytes, "reported_oldest_queued_at": tc.oldest, "queue_reported_at": tc.at,
					"queue_bytes": nil, "oldest_queued_at": nil,
				} {
					value, present := got[field]
					if !present || value != want {
						t.Fatalf("%s %s = %#v (present %v), want %#v", name, field, value, present, want)
					}
				}
			}
		})
	}
}
