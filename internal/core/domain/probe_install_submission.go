package domain

import "errors"

// MaxProbeInstallPrivateKeySize bounds the transient deployment key.
const MaxProbeInstallPrivateKeySize = 64 << 10

// Installer admission sentinels. They never carry paths, keys or SSH output.
var (
	ErrProbeInstallBusy     = errors.New("probe install capacity exhausted")
	ErrProbeInstallClosed   = errors.New("probe install service closed")
	ErrProbeInstallArtifact = errors.New("pinned probe artifact rejected")
)

// ProbeInstallSubmission is one admin request to install a systemd probe over
// SSH. PrivateKey is a transient deployment key: it is parsed in memory for
// the duration of one operation and never stored. RequestedBy comes only from
// the authenticated context. The binary pin is the operator's explicit trust.
type ProbeInstallSubmission struct {
	OperationID        string
	Host               ProbeSSHHost
	HostKeyFingerprint string
	PrivateKey         []byte
	Binary             ProbeBinaryPin
	RequestedBy        int64
}

// Valid checks shape only: a root SSH host, canonical identifiers, a valid pin
// and a bounded non-empty key. It proves nothing about the host or the key.
func (s ProbeInstallSubmission) Valid() bool {
	return ValidProbeOperationID(s.OperationID) && s.Host.Valid() && s.Host.User == "root" &&
		ValidSSHKeyFingerprint(s.HostKeyFingerprint) && s.Binary.Valid() && s.RequestedBy > 0 &&
		len(s.PrivateKey) > 0 && len(s.PrivateKey) <= MaxProbeInstallPrivateKeySize
}
