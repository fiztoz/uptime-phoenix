package domain

import "time"

// Monitor represents a monitoring target configuration.
type Monitor struct {
	ID          int64
	UserID      int64
	Name        string
	Description string
	// Owner is free-text contact information for the team or person responsible
	// for the monitored service. It is display-only and has no authorization
	// meaning; UserID remains the creating Phoenix account.
	// When InheritGroupOwner is true, EffectiveOwner walks the group chain and
	// prefers a non-empty group Owner over this field.
	Owner string
	// InheritGroupOwner, when true and GroupID is set, makes the display contact
	// come from the monitor's group (and ancestors) rather than Owner alone.
	InheritGroupOwner   bool
	Type                string // "http", "tcp", "ping", "dns", ...
	Active              bool
	Interval            int // seconds between checks
	RetryInterval       int // seconds between retries
	MaxRetries          int
	Timeout             float64 // seconds
	Config              map[string]any
	AcceptedStatusCodes []string // for HTTP: ["200-299", "301"]
	ProxyID             *int64
	UpsideDown          bool // flip UP/DOWN
	ResendInterval      int  // minutes between repeated alerts
	PushToken           string
	// GroupID files this monitor under a MonitorGroup. nil means top-level
	// (not in any group). Replaces the old ParentID, which nested a monitor
	// under another *monitor*.
	GroupID *int64
	// Weight is the manual display order (lower first). Lists order by weight,
	// then name, then id. Schema default is 2000; Create treats 0 as unset → 2000.
	Weight    int
	TLSIgnore bool
	// CertExpiryNotify opts the monitor into certificate-expiry alerts at the
	// fixed 30/14/7 day thresholds. Default false keeps existing monitors quiet
	// until an operator explicitly enables the feature (Sprint C F2.1).
	CertExpiryNotify bool
	DockerHostID     *int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Target returns the primary connection target string for this monitor,
// derived from its Type and Config. The result is what gets displayed in
// notification messages, status-page cards, and the monitor list.
func (m *Monitor) Target() string {
	keys := map[string][]string{
		"http":      {"url"},
		"websocket": {"url"},
		"tcp":       {"hostname", "host"},
		"ping":      {"hostname", "host"},
		"dns":       {"hostname", "host"},
		"grpc":      {"hostname"},
		"mqtt":      {"broker", "url", "hostname", "host"},
		"snmp":      {"hostname", "host"},
		"database":  {"connection_string", "dsn", "connectionString", "hostname"},
	}
	for _, key := range keys[m.Type] {
		if v, ok := m.Config[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// maxOwnerAncestorWalk bounds the group ParentID walk used when resolving an
// inherited contact, so a cycle in stored group data cannot hang the request.
const maxOwnerAncestorWalk = 64

// EffectiveOwner returns the contact string to display for this monitor.
//
// When InheritGroupOwner is false, or the monitor is not in a group, this is
// simply Owner. When inherit is on, the monitor's group and each ancestor is
// walked (nearest first) for the first non-empty Owner; if the whole chain is
// empty, the monitor's own Owner is used as a fallback.
//
// groupsByID may be nil or incomplete — missing groups simply stop the walk.
// Authorization is not involved; this is display metadata only.
func (m *Monitor) EffectiveOwner(groupsByID map[int64]*MonitorGroup) string {
	if m == nil {
		return ""
	}
	if !m.InheritGroupOwner || m.GroupID == nil || groupsByID == nil {
		return m.Owner
	}
	seen := make(map[int64]bool, 8)
	gid := m.GroupID
	for depth := 0; gid != nil && depth < maxOwnerAncestorWalk; depth++ {
		id := *gid
		if seen[id] {
			break
		}
		seen[id] = true
		g := groupsByID[id]
		if g == nil {
			break
		}
		if g.Owner != "" {
			return g.Owner
		}
		gid = g.ParentID
	}
	return m.Owner
}
