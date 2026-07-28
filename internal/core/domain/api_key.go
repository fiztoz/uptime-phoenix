package domain

import "time"

// APIKey represents an API key for programmatic access.
type APIKey struct {
	ID         int64
	UserID     int64
	Name       string
	KeyHash    string // SHA-256 of the actual key
	Active     bool
	ExpiresAt  *time.Time
	Scopes     []string // e.g. ["read", "write", "metrics"]
	LastUsedAt *time.Time
	CreatedAt  time.Time
}
