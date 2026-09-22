package probe

import (
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// PullCheckerCapabilities derives the exact "checker.<type>.v1" hello
// advertisement from the caller-supplied candidate types (the installed
// pull-checker inventory) and verified host capabilities. The lookup comes
// from the composition root, so advertisement reflects the real build, not a
// hardcoded inventory.
//
// Docker requires a validated probe-local resource map. Ping requires the unprivileged
// ICMP socket the ping checker actually opens. Every other pull checker is
// compiled into the build and has no environmental constraint to probe.
func PullCheckerCapabilities(candidates []string, lookup func(string) (ports.Checker, bool), icmpAvailable bool, resources ...*LocalResourceBindings) []string {
	if lookup == nil {
		return nil
	}
	advertised := make([]string, 0, len(candidates))
	for _, kind := range candidates {
		switch kind {
		case "docker":
			if len(resources) != 1 || len(resources[0].Inventory()) == 0 {
				continue
			}
		case "ping":
			if !icmpAvailable {
				continue
			}
		}
		c, ok := lookup(kind)
		if !ok || c == nil || c.Type() != kind {
			continue
		}
		advertised = append(advertised, "checker."+kind+".v1")
	}
	return advertised
}
