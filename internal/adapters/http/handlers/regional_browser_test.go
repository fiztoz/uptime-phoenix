package handlers

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
)

func TestM5BrowserHealthSharesHTTPContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/m5/health.json")
	if err != nil {
		t.Fatal(err)
	}
	var view MonitorHealthView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{false, true} {
		if missing {
			view.ProjectionVersion = 0
			view.KnownSeconds = .125
			view.UnknownSeconds = 3599.875
			view.Regions[0].ObservedAt = nil
			view.Regions[0].ReceivedAt = nil
			view.Regions[0].FreshUntil = nil
		}
		frame, err := json.Marshal(map[string]any{"type": "monitor.health", "payload": view})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := probe.DecodeBrowserEvent(frame); err != nil {
			t.Fatalf("HTTP health cannot be consumed as live event: %v %s", err, frame)
		}
		region := view.Regions[0]
		frame, err = json.Marshal(map[string]any{"type": "monitor.probe.status", "payload": map[string]any{"monitor_id": view.MonitorID, "probe_id": region.ProbeID, "status": region.Status, "connection_status": region.ConnectionStatus, "observed_at": region.ObservedAt, "received_at": region.ReceivedAt, "fresh_until": region.FreshUntil, "reason": region.Reason}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := probe.DecodeBrowserEvent(frame); err != nil {
			t.Fatalf("missing evidence cannot be consumed: %v %s", err, frame)
		}
	}
	// A monitor with no stored assignments has an honest empty read view.
	view.ProbeCounts = ProbeHealthCountsView{}
	view.Regions = []MonitorRegionView{}
	frame, err := json.Marshal(map[string]any{"type": "monitor.health", "payload": view})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.DecodeBrowserEvent(frame); err != nil {
		t.Fatalf("empty HTTP health cannot be consumed: %v", err)
	}
}
