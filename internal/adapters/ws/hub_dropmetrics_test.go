package ws

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Drop observability, kept in its own file because it exercises API that did not
// exist before the R3.6 fix — the sibling hub_eventpath_test.go is deliberately
// compilable against the OLD hub so its failure there can be demonstrated.

// A frame dropped because a client's send buffer is full must be COUNTED.
//
// Dropping is the right policy — one slow browser tab must not stall fan-out for
// everyone — but before this the drop was invisible: no log line, no metric. A
// backlogged install that was quietly losing UI events looked exactly like an
// idle one, which is why the load harness's "delivered events/sec" overstated
// what clients actually received.
func TestHub_DroppedClientFramesAreCounted(t *testing.T) {
	h := newEventPathHarness(t, 1)

	var dropped atomic.Int64
	h.hub.SetDropMetrics(countingDropMetrics{n: &dropped})

	// Register a client and never drain it, so its send buffer fills.
	client := NewClient("slow-client", h.adminID)
	h.hub.AddClient(client)

	ctx := context.Background()
	monitorID := h.monitorIDs[0]
	for i := 0; i < 2000; i++ {
		if err := h.bus.Publish(ctx, ports.Event{
			Type: EventHeartbeat,
			Payload: &domain.Heartbeat{
				MonitorID: monitorID, Status: domain.StatusUp, Time: time.Now().UTC(),
			},
		}); err != nil {
			t.Fatalf("publish heartbeat: %v", err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if dropped.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("client send buffer overflowed but no drop was counted (dropped=%d)", dropped.Load())
}

type countingDropMetrics struct{ n *atomic.Int64 }

func (c countingDropMetrics) IncWSFrameDropped() { c.n.Add(1) }
