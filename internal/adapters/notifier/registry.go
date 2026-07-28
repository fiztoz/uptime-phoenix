// Package notifier implements the ports.NotificationSender interface for all notification providers.
// Each provider is one file. This file is the auto-registration registry.
package notifier

import "github.com/fiztoz/uptime-phoenix/internal/core/ports"

var registry = map[string]ports.NotificationSender{}

// Register adds a notification sender to the registry. Called by each sender's init().
func Register(s ports.NotificationSender) {
	registry[s.Type()] = s
}

// Get retrieves a notification sender by type string. Returns false if not found.
func Get(t string) (ports.NotificationSender, bool) {
	s, ok := registry[t]
	return s, ok
}
