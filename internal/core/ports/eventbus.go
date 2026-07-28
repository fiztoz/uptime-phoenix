package ports

import "context"

// Event represents a domain event published on the bus.
type Event struct {
	Type    string
	Payload any
}

// EventBus defines the pub/sub interface for domain events.
type EventBus interface {
	Publish(ctx context.Context, event Event) error
	Subscribe(eventType string) <-chan Event
	Close()
}
