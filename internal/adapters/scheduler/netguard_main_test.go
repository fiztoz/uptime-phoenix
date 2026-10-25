package scheduler

import (
	"os"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
)

// TestMain lets this package's real-check tests reach their loopback
// fixtures, the same way an operator allowlists internal targets with
// OUTBOUND_ALLOWED_CIDRS.
func TestMain(m *testing.M) {
	netguard.AllowLoopbackForTests()
	os.Exit(m.Run())
}
