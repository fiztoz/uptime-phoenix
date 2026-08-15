package ws

import (
	"testing"
)

// phoenix_ws_connections_active is what the API HPA reads. The gauge was
// registered and the hub already knew len(clients), but nothing ever called
// SetWSConnectionsActive, so the scrape stayed at 0 with live sockets.
func TestHub_ActiveConnectionCountExportedOnAddRemove(t *testing.T) {
	h := newEventPathHarness(t, 0)

	var got []float64
	h.hub.SetDropMetrics(recordingConnMetrics{onSet: func(n float64) {
		got = append(got, n)
	}})

	a := NewClient("a", h.adminID)
	b := NewClient("b", h.adminID)
	h.hub.AddClient(a)
	h.hub.AddClient(b)
	h.hub.RemoveClient(a)
	h.hub.RemoveClient(b)

	want := []float64{0, 1, 2, 1, 0} // attach publishes current (0), then each mutation
	if len(got) != len(want) {
		t.Fatalf("published counts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("published counts = %v, want %v", got, want)
		}
	}
}

// countingDropMetrics must keep compiling without SetWSConnectionsActive —
// drop-only fakes must not be forced onto the full exporter surface.
func TestHub_DropOnlyMetricsStillAttach(t *testing.T) {
	h := newEventPathHarness(t, 0)
	h.hub.SetDropMetrics(countingDropMetrics{})
	h.hub.AddClient(NewClient("solo", h.adminID))
	if n := h.hub.ActiveConnections(); n != 1 {
		t.Fatalf("ActiveConnections = %d, want 1", n)
	}
}

type recordingConnMetrics struct {
	onSet func(float64)
}

func (recordingConnMetrics) IncWSFrameDropped() {}

func (r recordingConnMetrics) SetWSConnectionsActive(count float64) {
	if r.onSet != nil {
		r.onSet(count)
	}
}
