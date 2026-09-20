package domain

import "time"

// MaxEdgeCertificateBytes bounds local PEM before authenticated encryption.
const MaxEdgeCertificateBytes = 16 << 10

// ProbeCertificateCommand is a closed certificate prepare/activate input. The
// source retains only its digest and nonsecret identity alongside protected PEM.
type ProbeCertificateCommand struct {
	CommandID, ProbeID, Kind string
	CreatedAt, ExpiresAt     time.Time
	PayloadHash              [32]byte
	RotationID               string
	CertificateVersion       int64
	ValidForDays             int
	ExpectedFingerprint      string
}

// ValidProbeCertificateCommand validates immutable input, not session authority.
func ValidProbeCertificateCommand(c ProbeCertificateCommand) bool {
	if !configUUID(c.CommandID) || !configUUID(c.ProbeID) || !configUUID(c.RotationID) || c.CertificateVersion <= 1 || c.PayloadHash == [32]byte{} || c.CreatedAt.IsZero() || !c.ExpiresAt.After(c.CreatedAt) || c.ExpiresAt.Sub(c.CreatedAt) > 7*24*time.Hour || c.CreatedAt.Nanosecond()%1000 != 0 || c.ExpiresAt.Nanosecond()%1000 != 0 {
		return false
	}
	switch c.Kind {
	case "certificate.prepare":
		return c.ValidForDays >= 1 && c.ValidForDays <= 3650 && c.ExpectedFingerprint == ""
	case "certificate.activate":
		return c.ValidForDays == 0 && ValidKeyHash(c.ExpectedFingerprint)
	default:
		return false
	}
}

// EdgeCertificateMetadata binds local protected material to its original scope.
// Fingerprint describes the candidate, never the immutable bootstrap anchor.
type EdgeCertificateMetadata struct {
	HubID, ProbeID, StreamID, RotationID string
	Version                              int64
	Fingerprint                          string
	CreatedAt, NotBefore, NotAfter       time.Time
}

// ValidEdgeCertificateMetadata checks bounded, canonical authenticated scope.
func ValidEdgeCertificateMetadata(m EdgeCertificateMetadata) bool {
	return configUUID(m.HubID) && configUUID(m.ProbeID) && configUUID(m.StreamID) && configUUID(m.RotationID) && m.Version > 1 && ValidKeyHash(m.Fingerprint) && !m.CreatedAt.IsZero() && !m.NotBefore.IsZero() && m.NotAfter.After(m.NotBefore) && m.NotAfter.Sub(m.NotBefore) <= 3650*24*time.Hour+5*time.Minute && m.CreatedAt.Nanosecond()%1000 == 0 && m.NotBefore.Nanosecond() == 0 && m.NotAfter.Nanosecond() == 0
}

// ProtectedEdgeCertificate contains confidential local material. Never marshal
// or log it, or return it through a transport or operator response.
type ProtectedEdgeCertificate struct {
	EdgeCertificateMetadata
	ProtectedPEM []byte
}

// EdgeCertificateState selects durable active material. Certificate is nil only
// for active version one, which uses the validated immutable bootstrap files.
type EdgeCertificateState struct {
	ActiveVersion, HighestVersion int64
	Certificate                   *ProtectedEdgeCertificate
}
