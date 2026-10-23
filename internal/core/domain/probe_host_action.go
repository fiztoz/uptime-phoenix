package domain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Host actions are the systemd resources other than artifact_directory. Each is
// applied by the authorized host helper under its own one-shot DB admission.
const (
	ProbeHostActionDataDirectory = "data_directory"
	ProbeHostActionUnitFile      = "unit_file"
	ProbeHostActionService       = "service"
)

const probeHostActionSigningDomain = "phoenix-host-action-admission-v1\x00"

// ProbeHostActionIntent names one non-artifact systemd resource attempt.
// OwnerID/AttemptID are labels, not proof of a current committed reservation.
type ProbeHostActionIntent struct {
	Identity  ProbeInstallIdentity
	OwnerID   string
	AttemptID string
	Kind      string
}

// Valid accepts only the root systemd layout and the three host-action kinds.
func (i ProbeHostActionIntent) Valid() bool {
	if !i.Identity.Valid() || i.Identity.Mode != ProbeInstallSystemd || i.Identity.Host.User != "root" ||
		!ValidProbeOperationID(i.OwnerID) || !ValidProbeOperationID(i.AttemptID) {
		return false
	}
	switch i.Kind {
	case ProbeHostActionDataDirectory, ProbeHostActionUnitFile, ProbeHostActionService:
		return true
	}
	return false
}

// Resource returns the identity-derived spec this action applies.
func (i ProbeHostActionIntent) Resource() (ProbeInstallResourceSpec, bool) {
	if !i.Valid() {
		return ProbeInstallResourceSpec{}, false
	}
	for _, spec := range i.Identity.Resources() {
		if spec.Kind == i.Kind {
			return spec, true
		}
	}
	return ProbeInstallResourceSpec{}, false
}

// Marker is the canonical secret-free description the grant signs. For
// unit_file it also binds the exact derived unit content, so a hub and helper
// deriving different templates refuse rather than install different bytes.
func (i ProbeHostActionIntent) Marker() string {
	spec, ok := i.Resource()
	if !ok {
		return ""
	}
	r, b := i.Identity, i.Identity.Binary
	lines := []string{
		"phoenix-host-action-v1",
		"kind=" + i.Kind,
		"reference=" + spec.Reference,
		"operation_id=" + r.OperationID,
		"owner_id=" + i.OwnerID,
		"attempt_id=" + i.AttemptID,
		"ssh_address=" + r.Host.Address,
		"ssh_user=" + r.Host.User,
		"host_key=" + r.HostKeyFingerprint,
		"client_key=" + r.ClientKeyFingerprint,
		"requested_by=" + strconv.FormatInt(r.RequestedBy, 10),
		"layout_version=" + strconv.Itoa(r.LayoutVersion),
		"mode=" + r.Mode,
		"version=" + b.Version,
		"architecture=" + b.Architecture,
		"sha256=" + b.SHA256,
		"size=" + strconv.FormatInt(b.Size, 10),
	}
	if i.Kind == ProbeHostActionUnitFile {
		sum := sha256.Sum256([]byte(ProbeSystemdUnit(r)))
		lines = append(lines, "unit_sha256="+hex.EncodeToString(sum[:]))
	}
	return strings.Join(append(lines, ""), "\n")
}

// MarkerSHA256 identifies the canonical marker bytes.
func (i ProbeHostActionIntent) MarkerSHA256() string {
	marker := i.Marker()
	if marker == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(marker))
	return hex.EncodeToString(sum[:])
}

// ProbeHostActionAdmissionRequest names one host-action attempt and the
// helper's challenge. Its labels carry no proof of a stored reservation.
type ProbeHostActionAdmissionRequest struct {
	Intent    ProbeHostActionIntent
	Challenge ProbeBinaryStageChallenge
}

// Valid binds the whole intent through its marker.
func (r ProbeHostActionAdmissionRequest) Valid() bool {
	return r.Intent.Valid() && r.Challenge.Valid() && r.Challenge.MarkerSHA256 == r.Intent.MarkerSHA256()
}

// SigningMessage is domain-separated from binary staging grants, so neither
// kind of grant can ever verify as the other.
func (r ProbeHostActionAdmissionRequest) SigningMessage() []byte {
	if !r.Valid() {
		return nil
	}
	key, _ := hex.DecodeString(r.Challenge.AuthorityKeyID)
	marker, _ := hex.DecodeString(r.Challenge.MarkerSHA256)
	message := make([]byte, 0, len(probeHostActionSigningDomain)+96)
	message = append(message, probeHostActionSigningDomain...)
	message = append(message, key...)
	message = append(message, r.Challenge.Nonce[:]...)
	return append(message, marker...)
}

// ProbeHostActionAdmission is an acknowledged, irreversible DB consumption.
type ProbeHostActionAdmission struct {
	Request    ProbeHostActionAdmissionRequest
	AdmittedAt time.Time
}

// Valid checks the request and the UTC microsecond storage contract.
func (a ProbeHostActionAdmission) Valid() bool {
	return a.Request.Valid() && installUTC(a.AdmittedAt)
}

// ProbeHostActionGrant is short-lived authorization material. Never log,
// persist or serialize it in public views.
type ProbeHostActionGrant struct {
	Request   ProbeHostActionAdmissionRequest
	Signature [64]byte
}

// ValidFor verifies the exact message under the configured public authority.
func (g ProbeHostActionGrant) ValidFor(publicKey [32]byte) bool {
	return publicKey != ([32]byte{}) && g.Request.Valid() &&
		g.Request.Challenge.AuthorityKeyID == ProbeBinaryStageAuthorityKeyID(publicKey) &&
		ed25519.Verify(ed25519.PublicKey(publicKey[:]), g.Request.SigningMessage(), g.Signature[:])
}

// ProbeSystemdUnit derives the complete unit file for a valid systemd
// identity, or "" otherwise. It is pure data: the hub signs its hash and the
// helper writes exactly these bytes. The probe runs as root with every
// capability except CAP_NET_RAW (ICMP) dropped, a read-only system, and write
// access only to its own data directory. It is not started until enrolled.
func ProbeSystemdUnit(r ProbeInstallIdentity) string {
	if !r.Valid() || r.Mode != ProbeInstallSystemd {
		return ""
	}
	specs := r.Resources()
	data, artifact := specs[0].Reference, specs[1].Reference
	return strings.Join([]string{
		"# Managed by Uptime Phoenix (phoenix-systemd-unit-v1). Do not edit.",
		"# operation_id=" + r.OperationID,
		"[Unit]",
		"Description=Uptime Phoenix probe " + r.OperationID,
		"Wants=network-online.target",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + artifact + "/" + r.Binary.ReleaseName() + " run --data-dir " + data,
		"Restart=on-failure",
		"RestartSec=10s",
		"UMask=0077",
		"NoNewPrivileges=yes",
		"CapabilityBoundingSet=CAP_NET_RAW",
		"AmbientCapabilities=",
		"ProtectSystem=strict",
		"ReadWritePaths=" + data,
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectControlGroups=yes",
		"ProtectClock=yes",
		"ProtectHostname=yes",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK",
		"RestrictNamespaces=yes",
		"RestrictRealtime=yes",
		"RestrictSUIDSGID=yes",
		"LockPersonality=yes",
		"MemoryDenyWriteExecute=yes",
		"SystemCallArchitectures=native",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}
