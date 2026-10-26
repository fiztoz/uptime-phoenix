package notifier

import (
	"os"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
)

// TestMain lets this package's tests reach their httptest servers on
// loopback, the same way an operator allowlists internal targets with
// OUTBOUND_ALLOWED_CIDRS. Tests that assert blocking install their own policy.
func TestMain(m *testing.M) {
	netguard.AllowLoopbackForTests()
	os.Exit(m.Run())
}
