package notifier

import (
	"context"
	"net/http"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
)

// notifierHTTPClient is the client every HTTP notifier sends through. Its
// dials are checked by netguard against the outbound policy the caller put on
// ctx (the notification owner's trust; see domain.OutboundTrust): an
// admin-owned notification may reach private addresses, one owned by a
// non-admin may not unless an operator allowlists them with
// OUTBOUND_ALLOWED_CIDRS, and nobody reaches link-local or cloud metadata
// addresses. Tests may replace it.
var notifierHTTPClient = func(ctx context.Context) *http.Client { return netguard.ForContext(ctx).Client(0) }
