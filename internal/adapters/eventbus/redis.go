// Package eventbus provides adapters for the ports.EventBus interface.
package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// RedisBus is a Redis-backed EventBus implementation using Pub/Sub.
// This is the Phase 3 opt-in adapter, selected when REDIS_URL is set.
// It allows cross-pod event distribution for the API/worker split.
type RedisBus struct {
	client *redis.Client
	log    ports.Logger

	mu          sync.RWMutex
	subscribers map[string][]chan ports.Event
	closed      bool
	listeners   map[string]*redis.PubSub // one pubsub listener per event type
	metrics     dropMetrics
}

// SetDropMetrics attaches a counter for dropped events. Optional; nil-safe.
//
// Split API/worker mode runs on this bus, not MemoryBus, so a drop counter that
// only existed in memory.go would leave exactly the deployment mode that fans
// out across pods unmonitored.
func (b *RedisBus) SetDropMetrics(m dropMetrics) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics = m
}

// NewRedisBus creates a new Redis-backed EventBus from a DSN (e.g. "redis://localhost:6379/0").
// It pings the server on creation.
func NewRedisBus(ctx context.Context, dsn string, log ports.Logger) (*RedisBus, error) {
	opts, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	bus := &RedisBus{
		client:      client,
		log:         log,
		subscribers: make(map[string][]chan ports.Event),
		listeners:   make(map[string]*redis.PubSub),
	}

	return bus, nil
}

// Publish marshals the event to JSON and publishes to the Redis channel for its type.
func (b *RedisBus) Publish(ctx context.Context, event ports.Event) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return nil
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	channel := b.channelFor(event.Type)
	if err := b.client.Publish(ctx, channel, data).Err(); err != nil {
		b.log.Warn("redis publish failed", "type", event.Type, "error", err)
		// best-effort, do not fail caller
		return nil
	}
	return nil
}

// Subscribe returns a channel that receives events of the given type.
// It starts a background listener goroutine (one per type) that receives from Redis
// and fans out to all local subscribers.
func (b *RedisBus) Subscribe(eventType string) <-chan ports.Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan ports.Event, subscriberBuffer)
	b.subscribers[eventType] = append(b.subscribers[eventType], ch)

	// Start listener if not already running for this type
	if _, ok := b.listeners[eventType]; !ok {
		ps := b.client.Subscribe(context.Background(), b.channelFor(eventType))
		b.listeners[eventType] = ps

		go b.listen(eventType, ps)
	}

	return ch
}

func (b *RedisBus) channelFor(eventType string) string {
	return "phoenix:" + eventType
}

func (b *RedisBus) listen(eventType string, ps *redis.PubSub) {
	defer func() { _ = ps.Close() }()

	ch := ps.Channel()
	for msg := range ch {
		if b.closed {
			return
		}

		var event ports.Event
		if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
			b.log.Warn("redis event unmarshal failed", "type", eventType, "error", err)
			continue
		}

		b.mu.RLock()
		chs := b.subscribers[eventType]
		m := b.metrics
		b.mu.RUnlock()

		for _, subCh := range chs {
			select {
			case subCh <- event:
			default:
				// Slow consumer: drop, but count it. Same policy as MemoryBus.
				if m != nil {
					m.IncBusEventDropped(eventType)
				}
			}
		}
	}
}

// Close shuts down the Redis client and all subscriber channels.
func (b *RedisBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true

	for _, ps := range b.listeners {
		_ = ps.Close()
	}

	for _, chs := range b.subscribers {
		for _, ch := range chs {
			close(ch)
		}
	}

	b.subscribers = nil
	b.listeners = nil

	_ = b.client.Close()
}
