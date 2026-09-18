package domain

import (
	"strings"
	"time"
)

// MaxProbeConfigBytes bounds the complete confidential configuration document.
const MaxProbeConfigBytes = 16 << 20

// ProbeConfigProtectionOverhead covers the version, random nonce and AEAD tag.
const ProbeConfigProtectionOverhead = 1 + 12 + 16

// ProbeConfigTarget is trusted installation/destination identity, not document input.
type ProbeConfigTarget struct {
	HubID   string
	ProbeID string
}

// ProbeConfigMetadata describes exact original bytes of a prepared snapshot.
// Preparation is not runtime validation, authorization or an activation receipt.
// Database metadata times have UTC microsecond precision; SHA256 binds all bytes.
type ProbeConfigMetadata struct {
	ProbeConfigTarget
	Revision      int64
	SchemaVersion int
	SHA256        string
	CreatedAt     time.Time
	EffectiveAt   time.Time
}

// ProtectedProbeConfig retains immutable confidential content as ciphertext only.
// Neither this object nor decrypted document bytes belong in HTTP views or logs.
type ProtectedProbeConfig struct {
	ProbeConfigMetadata
	ProtectedPayload []byte
	StoredAt         time.Time
}

// ValidProbeConfigTarget accepts the reserved local destination or a remote UUID.
func ValidProbeConfigTarget(target ProbeConfigTarget) bool {
	return configUUID(target.HubID) && (target.ProbeID == LocalProbeID || configUUID(target.ProbeID))
}

// ValidProbeConfigMetadata checks bounded storage/protection identity.
func ValidProbeConfigMetadata(m ProbeConfigMetadata) bool {
	return ValidProbeConfigTarget(m.ProbeConfigTarget) && m.Revision > 0 && m.SchemaVersion == 1 &&
		len(m.SHA256) == 64 && configHex(m.SHA256) && !m.CreatedAt.IsZero() && !m.EffectiveAt.IsZero()
}

// NormalizeProbeConfigMetadata makes metadata stable across both SQL engines.
func NormalizeProbeConfigMetadata(m ProbeConfigMetadata) ProbeConfigMetadata {
	m.CreatedAt = m.CreatedAt.UTC().Truncate(time.Microsecond)
	m.EffectiveAt = m.EffectiveAt.UTC().Truncate(time.Microsecond)
	return m
}

// SameProbeConfigMetadata compares immutable identity independently of time zones.
func SameProbeConfigMetadata(a, b ProbeConfigMetadata) bool {
	a, b = NormalizeProbeConfigMetadata(a), NormalizeProbeConfigMetadata(b)
	return a.ProbeConfigTarget == b.ProbeConfigTarget && a.Revision == b.Revision &&
		a.SchemaVersion == b.SchemaVersion && a.SHA256 == b.SHA256 &&
		a.CreatedAt.Equal(b.CreatedAt) && a.EffectiveAt.Equal(b.EffectiveAt)
}

func configUUID(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' &&
		len(strings.ReplaceAll(value, "-", "")) == 32 && configHex(strings.ReplaceAll(value, "-", "")) &&
		value != "00000000-0000-0000-0000-000000000000"
}

func configHex(value string) bool {
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
