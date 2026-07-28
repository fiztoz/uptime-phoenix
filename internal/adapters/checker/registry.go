// Package checker implements the ports.Checker interface for all monitor types.
// Each type is one file. This file is the auto-registration registry.
package checker

import "github.com/fiztoz/uptime-phoenix/internal/core/ports"

var registry = map[string]ports.Checker{}

// Register adds a checker to the registry. Called by each checker's init().
func Register(c ports.Checker) {
	registry[c.Type()] = c
}

// Get retrieves a checker by type string. Returns false if not found.
func Get(t string) (ports.Checker, bool) {
	c, ok := registry[t]
	return c, ok
}
