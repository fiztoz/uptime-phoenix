package domain

import "time"

// Monitor represents a monitoring target configuration.
type Monitor struct {
	ID                  int64
	UserID              int64
	Name                string
	Description         string
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
	GroupID   *int64
	Weight    int // display order
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
