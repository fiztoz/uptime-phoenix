package domain

import (
	"errors"
	"strings"
	"time"
)

// ErrProbeKeyMismatch indicates the configured key does not match the stored key hash.
var ErrProbeKeyMismatch = errors.New("probe secret key does not match installation authority")

// ErrProbeInstallationConflict indicates an installation identity conflict.
var ErrProbeInstallationConflict = errors.New("probe installation identity conflict")

// ProbeInstallation represents the singleton installation identity and key confirmation.
type ProbeInstallation struct {
	HubID          string
	KeyHash        string
	ProtocolFloor  int
	AuthorityEpoch int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ValidHubID checks whether value is a valid canonical 36-char UUID.
func ValidHubID(value string) bool {
	return configUUID(value)
}

// ValidKeyHash checks whether value is a valid 64-char lowercase hex string.
func ValidKeyHash(value string) bool {
	return len(value) == 64 && configHex(value)
}

// ValidProbeInstallation verifies all fields of a ProbeInstallation struct.
func ValidProbeInstallation(inst ProbeInstallation) bool {
	return ValidHubID(inst.HubID) &&
		ValidKeyHash(inst.KeyHash) &&
		inst.ProtocolFloor >= 1 &&
		inst.AuthorityEpoch >= 1 &&
		!inst.CreatedAt.IsZero() &&
		!inst.UpdatedAt.IsZero()
}

// NormalizeProbeInstallation normalizes timestamps to UTC microsecond precision.
func NormalizeProbeInstallation(inst ProbeInstallation) ProbeInstallation {
	inst.HubID = strings.TrimSpace(inst.HubID)
	inst.KeyHash = strings.ToLower(strings.TrimSpace(inst.KeyHash))
	inst.CreatedAt = inst.CreatedAt.UTC().Truncate(time.Microsecond)
	inst.UpdatedAt = inst.UpdatedAt.UTC().Truncate(time.Microsecond)
	return inst
}

// SameProbeInstallation compares two installation records independent of time zones.
func SameProbeInstallation(a, b ProbeInstallation) bool {
	a, b = NormalizeProbeInstallation(a), NormalizeProbeInstallation(b)
	return a.HubID == b.HubID &&
		a.KeyHash == b.KeyHash &&
		a.ProtocolFloor == b.ProtocolFloor &&
		a.AuthorityEpoch == b.AuthorityEpoch &&
		a.CreatedAt.Equal(b.CreatedAt) &&
		a.UpdatedAt.Equal(b.UpdatedAt)
}
