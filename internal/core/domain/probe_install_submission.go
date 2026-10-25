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

// ProbeInstallSubmission is one admin request to install a probe over SSH,
// as a systemd unit (Mode "" or systemd, with a binary pin) or a Docker
// container (Mode docker, with an image pin). PrivateKey is a transient
// deployment key: it is parsed in memory for the duration of one operation
// and never stored. RequestedBy comes only from the authenticated context.
// The pin is the operator's explicit trust.
type ProbeInstallSubmission struct {
	OperationID        string
	Host               ProbeSSHHost
	HostKeyFingerprint string
	PrivateKey         []byte
	Mode               string
	Binary             ProbeBinaryPin
	Image              ProbeImagePin
	RequestedBy        int64
}

// InstallMode returns the effective mode: systemd unless docker is chosen.
func (s ProbeInstallSubmission) InstallMode() string {
	if s.Mode == ProbeInstallDocker {
		return ProbeInstallDocker
	}
	return ProbeInstallSystemd
}

// Valid checks shape only: a root SSH host, canonical identifiers, exactly
// the pin of the chosen mode and a bounded non-empty key. It proves nothing
// about the host or the key.
func (s ProbeInstallSubmission) Valid() bool {
	if !ValidProbeOperationID(s.OperationID) || !s.Host.Valid() || s.Host.User != "root" ||
		!ValidSSHKeyFingerprint(s.HostKeyFingerprint) || s.RequestedBy <= 0 ||
		len(s.PrivateKey) == 0 || len(s.PrivateKey) > MaxProbeInstallPrivateKeySize {
		return false
	}
	switch s.Mode {
	case "", ProbeInstallSystemd:
		return s.Binary.Valid() && s.Image == (ProbeImagePin{})
	case ProbeInstallDocker:
		return s.Image.Valid() && s.Binary == (ProbeBinaryPin{})
	}
	return false
}
