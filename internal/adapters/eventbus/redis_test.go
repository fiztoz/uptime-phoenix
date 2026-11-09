package eventbus

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/logger"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// newTestRedisBus connects to PHOENIX_TEST_REDIS_URL (a scratch Redis; the
// tests only use channels under a per-test event type). Without it the tests
// skip: MemoryBus covers the in-process path.
func newTestRedisBus(t *testing.T) *RedisBus {
	t.Helper()
	url := os.Getenv("PHOENIX_TEST_REDIS_URL")
	if url == "" {
		t.Skip("PHOENIX_TEST_REDIS_URL not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	b, err := NewRedisBus(ctx, url, logger.New("error"))
	if err != nil {
		t.Fatalf("redis bus: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

func uniqueEventType(t *testing.T) string {
	return "test." + t.Name() + "." + strconv.FormatInt(time.Now().UnixNano(), 36)
}

type countingDrops struct{ n atomic.Int64 }

func (c *countingDrops) IncBusEventDropped(string) { c.n.Add(1) }

// receiveWhilePublishing republishes until the subscriber sees an event:
// Redis SUBSCRIBE is asynchronous, so a single publish can race it.
func receiveWhilePublishing(t *testing.T, pub *RedisBus, ch <-chan ports.Event, ev ports.Event) ports.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := pub.Publish(t.Context(), ev); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatal("subscriber channel closed")
			}
			return got
		case <-tick.C:
		case <-deadline:
			t.Fatal("no event delivered across Redis within 10s")
		}
	}
}

// Split API/worker mode depends on one pod's Publish reaching another pod's
// Subscribe. Two buses on the same Redis stand in for the two pods.
func TestRedisBusCrossInstanceDelivery(t *testing.T) {
	publisher := newTestRedisBus(t)
	subscriber := newTestRedisBus(t)
	typ := uniqueEventType(t)
	ch := subscriber.Subscribe(typ)

	got := receiveWhilePublishing(t, publisher, ch, ports.Event{Type: typ, Payload: map[string]any{"monitor_id": 42, "status": "up"}})
	if got.Type != typ {
		t.Fatalf("type = %q, want %q", got.Type, typ)
	}
	// The payload crosses as JSON: numbers come back as float64 and typed
	// structs as maps. Consumers (ws.monitorIDForEvent) must accept that shape.
	m, ok := got.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type %T, want map[string]any after the Redis round trip", got.Payload)
	}
	if m["monitor_id"] != float64(42) || m["status"] != "up" {
		t.Fatalf("payload = %v", m)
	}
}

// A subscriber that never drains loses events once its buffer is full; the
// loss must be counted, like MemoryBus, not silent.
func TestRedisBusCountsDropsForSlowSubscriber(t *testing.T) {
	publisher := newTestRedisBus(t)
	subscriber := newTestRedisBus(t)
	drops := &countingDrops{}
	subscriber.SetDropMetrics(drops)
	typ := uniqueEventType(t)
	ch := subscriber.Subscribe(typ)
	_ = receiveWhilePublishing(t, publisher, ch, ports.Event{Type: typ, Payload: "warmup"})

	for i := 0; i < subscriberBuffer+200; i++ {
		if err := publisher.Publish(t.Context(), ports.Event{Type: typ, Payload: i}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for drops.n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if drops.n.Load() == 0 {
		t.Fatalf("no drops counted after overfilling a %d-event buffer (queued %d)", subscriberBuffer, len(ch))
	}
}

// Close while the listener is still receiving must not race on the bus state
// (run under -race) and must close every subscriber channel exactly once.
func TestRedisBusCloseWhileReceiving(t *testing.T) {
	publisher := newTestRedisBus(t)
	subscriber := newTestRedisBus(t)
	typ := uniqueEventType(t)
	ch := subscriber.Subscribe(typ)
	_ = receiveWhilePublishing(t, publisher, ch, ports.Event{Type: typ, Payload: "warmup"})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = publisher.Publish(context.Background(), ports.Event{Type: typ, Payload: "flood"})
			}
		}
	}()
	go func() {
		for range ch { //nolint:revive // drain until Close closes the channel
		}
	}()
	time.Sleep(200 * time.Millisecond)
	subscriber.Close()
	subscriber.Close() // idempotent
	close(stop)
	wg.Wait()
	if err := subscriber.Publish(context.Background(), ports.Event{Type: typ}); err != nil {
		t.Fatalf("publish after close = %v, want nil (closed bus is a no-op)", err)
	}
}
