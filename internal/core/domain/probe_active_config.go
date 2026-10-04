package domain

import (
	"strings"
	"time"
)

// ProbeActiveConfig represents the durable active configuration pointer and receipt.
// It binds the trusted target, positive revision, exact hash, and application time.
type ProbeActiveConfig struct {
	ProbeConfigTarget
	Revision        int64
	SHA256          string
	AppliedAt       time.Time
	AssignmentCount int
}

// ValidProbeActiveConfig checks whether an active configuration state is well-formed.
func ValidProbeActiveConfig(c ProbeActiveConfig) bool {
	return ValidProbeConfigTarget(c.ProbeConfigTarget) &&
		c.Revision > 0 &&
		len(c.SHA256) == 64 && configHex(c.SHA256) &&
		!c.AppliedAt.IsZero() &&
		c.AssignmentCount >= 0
}

// NormalizeProbeActiveConfig normalizes timestamps to UTC microsecond precision.
func NormalizeProbeActiveConfig(c ProbeActiveConfig) ProbeActiveConfig {
	c.HubID = strings.TrimSpace(c.HubID)
	c.ProbeID = strings.TrimSpace(c.ProbeID)
	c.SHA256 = strings.ToLower(strings.TrimSpace(c.SHA256))
	c.AppliedAt = c.AppliedAt.UTC().Truncate(time.Microsecond)
	return c
}

// SameProbeActiveConfig compares two active configurations independently of time zones.
func SameProbeActiveConfig(a, b ProbeActiveConfig) bool {
	a, b = NormalizeProbeActiveConfig(a), NormalizeProbeActiveConfig(b)
	return a.ProbeConfigTarget == b.ProbeConfigTarget &&
		a.Revision == b.Revision &&
		a.SHA256 == b.SHA256 &&
		a.AppliedAt.Equal(b.AppliedAt) &&
		a.AssignmentCount == b.AssignmentCount
}
