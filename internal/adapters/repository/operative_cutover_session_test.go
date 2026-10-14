package repository_test

import (
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestOperativeCutoverSessionContract composes encrypted real source state,
// pinned TLS/WebSocket transport, hub preparation and the operative coordinator.
func TestOperativeCutoverSessionContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) {
					testAssignmentRevisionSession(t, newProbeRegistryFixture(t, engine), mode, true, false, true)
				})
			}
		})
	}
}
