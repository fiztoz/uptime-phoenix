// Package eventbus provides adapters for the ports.EventBus interface.
package eventbus

import (
	"context"
	"sync"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// subscriberBuffer is how many events a single subscriber may fall behind by
// before its events start being dropped.
//
// This is a real backpressure policy, not a detail: a subscriber that exceeds it
// loses events permanently. It was 100 while the WS hub could stall for seconds
// on a synchronous O(monitors) recompute, which meant a cold start of a few
// hundred monitors quietly lost heartbeats. The hub no longer stalls (see
// ws.Hub.listen), and the drops that do happen are now counted rather than
// invisible — that observability matters more than the exact depth here.
const subscriberBuffer = 1024

// dropMetrics counts events discarded because a subscriber could not keep up.
//
// Narrow local interface so callers are not forced to implement the whole
// ports.MetricsExporter surface; the Prometheus adapter satisfies it
// structurally. nil is valid and records nothing.
type dropMetrics interface {
	IncBusEventDropped(eventType string)
}

// MemoryBus is an in-process EventBus implementation using Go channels.
// This is the Phase 1 default — zero external dependencies.
type MemoryBus struct {
	mu          sync.RWMutex
	subscribers map[string][]chan ports.Event
	closed      bool
	metrics     dropMetrics
}

// NewMemoryBus creates a new in-process EventBus.
func NewMemoryBus() *MemoryBus {
	return &MemoryBus{
		subscribers: make(map[string][]chan ports.Event),
	}
}

// SetDropMetrics attaches a counter for dropped events. Optional; nil-safe.
func (b *MemoryBus) SetDropMetrics(m dropMetrics) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics = m
}

// Publish sends an event to all subscribers of the event type.
// Non-blocking: slow subscribers are dropped.
//
// The drop is deliberate — one stalled subscriber must not block every publisher
// in the process — but it is no longer SILENT. An unobservable drop here is how
// heartbeats went missing under load with nothing in the logs to show for it.
func (b *MemoryBus) Publish(ctx context.Context, event ports.Event) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return nil
	}

	chs := b.subscribers[event.Type]
	for _, ch := range chs {
		select {
		case ch <- event:
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Slow consumer: drop, but count it.
			if b.metrics != nil {
				b.metrics.IncBusEventDropped(event.Type)
			}
		}
	}
	return nil
}

// Subscribe returns a channel that receives events of the given type.
func (b *MemoryBus) Subscribe(eventType string) <-chan ports.Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan ports.Event, subscriberBuffer)
	b.subscribers[eventType] = append(b.subscribers[eventType], ch)
	return ch
}

// Close closes all subscriber channels and marks the bus as closed.
func (b *MemoryBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true

	for _, chs := range b.subscribers {
		for _, ch := range chs {
			close(ch)
		}
	}
	b.subscribers = nil
}
