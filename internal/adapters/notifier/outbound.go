package notifier

import (
	"net/http"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
)

// notifierHTTPClient is the client every HTTP notifier sends through. Its
// dials are checked by netguard, so a notification URL cannot reach loopback,
// private, link-local or metadata addresses unless an operator allowlists
// them with OUTBOUND_ALLOWED_CIDRS. Tests may replace it.
var notifierHTTPClient = func() *http.Client { return netguard.Default().Client(0) }
