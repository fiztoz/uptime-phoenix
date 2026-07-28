package domain

// Proxy represents an outbound proxy configuration a monitor's checks can be
// routed through (Uptime Kuma-style proxy support). Password is stored
// plaintext because it must be usable to dial the proxy at check time — it
// is never safe to serialize this struct directly over HTTP (see the
// Wire-Shape Discipline rule in AGENTS.md); handlers must project it to a
// ProxyView that omits Password.
type Proxy struct {
	ID     int64
	UserID int64
	// Protocol is one of "http", "https", "socks5" (socks4 is rejected at
	// validation time — see internal/core/services/proxy_service.go).
	Protocol  string
	Host      string
	Port      int
	Auth      bool
	Username  string
	Password  string // plaintext proxy credential — NEVER expose via API
	Active    bool
	IsDefault bool
}
