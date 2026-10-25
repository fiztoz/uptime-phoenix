package domain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Host actions are the systemd resources other than artifact_directory, plus
// the activation's probe_init and service_start steps. Each is applied by the
// authorized host helper under its own one-shot DB admission.
const (
	ProbeHostActionDataDirectory = "data_directory"
	ProbeHostActionUnitFile      = "unit_file"
	ProbeHostActionService       = "service"
)

const probeHostActionSigningDomain = "phoenix-host-action-admission-v1\x00"

// ProbeHostActionIntent names one host-action attempt of a root install
// (every install resource but the binary stage, plus the activation and
// cleanup host steps). Payload is set only for image_cache. OwnerID/AttemptID
// are labels, not proof of a current committed reservation.
type ProbeHostActionIntent struct {
	Identity  ProbeInstallIdentity
	OwnerID   string
	AttemptID string
	Kind      string
	Payload   ProbeHostActionPayload
}

// Valid accepts a root install, a kind of its mode, and a payload exactly
// when the kind streams one (image_cache).
func (i ProbeHostActionIntent) Valid() bool {
	if !i.Identity.Valid() || i.Identity.Host.User != "root" ||
		!ValidProbeOperationID(i.OwnerID) || !ValidProbeOperationID(i.AttemptID) {
		return false
	}
	if i.Kind == ProbeHostActionImageCache {
		if !i.Payload.Valid() {
			return false
		}
	} else if i.Payload != (ProbeHostActionPayload{}) {
		return false
	}
	switch i.Kind {
	case ProbeHostActionDataDirectory, ProbeActivationProbeInit, ProbeActivationServiceStart, ProbeHostActionCleanup:
		return i.Identity.Mode == ProbeInstallSystemd || i.Identity.Mode == ProbeInstallDocker
	case ProbeHostActionUnitFile, ProbeHostActionService:
		return i.Identity.Mode == ProbeInstallSystemd
	case ProbeHostActionImageCache, ProbeHostActionContainer:
		return i.Identity.Mode == ProbeInstallDocker
	}
	return false
}

// Resource returns the identity-derived spec this action applies.
func (i ProbeHostActionIntent) Resource() (ProbeInstallResourceSpec, bool) {
	if !i.Valid() {
		return ProbeInstallResourceSpec{}, false
	}
	install := i.Identity.Resources()
	data, service, ok := ProbeInstallLayout(i.Identity)
	if !ok {
		return ProbeInstallResourceSpec{}, false
	}
	switch i.Kind {
	case ProbeActivationProbeInit:
		// The activation layout's host steps act on the installed paths.
		return ProbeInstallResourceSpec{Ordinal: 1, Kind: i.Kind, Reference: data.Reference, Owned: true}, true
	case ProbeActivationServiceStart:
		return ProbeInstallResourceSpec{Ordinal: 3, Kind: i.Kind, Reference: service.Reference, Owned: true}, true
	case ProbeHostActionCleanup:
		// Cleanup acts on the operation's whole resource family, which every
		// resource is named after.
		return ProbeInstallResourceSpec{Ordinal: 1, Kind: i.Kind, Reference: ProbeCleanupResourceName(i.Identity), Owned: true}, true
	}
	for _, spec := range install {
		if spec.Kind == i.Kind {
			return spec, true
		}
	}
	return ProbeInstallResourceSpec{}, false
}

// Marker is the canonical secret-free description the grant signs. For
// unit_file it also binds the exact derived unit content, and for container
// the exact derived `docker create` arguments, so a hub and helper deriving
// different templates refuse rather than install different resources. Docker
// identities list the image selection, and image_cache its payload.
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
	}
	if r.Mode == ProbeInstallDocker {
		img := r.Image
		lines = append(lines,
			"image_repository="+img.Pin.Repository,
			"version="+img.Pin.Version,
			"architecture="+img.Pin.Architecture,
			"image_index_sha256="+img.Pin.IndexSHA256,
			"image_index_size="+strconv.FormatInt(img.Pin.IndexSize, 10),
			"image_manifest_sha256="+img.ManifestSHA256,
			"image_config_sha256="+img.ConfigSHA256,
			"image_layer_count="+strconv.Itoa(img.LayerCount),
		)
	} else {
		lines = append(lines,
			"version="+b.Version,
			"architecture="+b.Architecture,
			"sha256="+b.SHA256,
			"size="+strconv.FormatInt(b.Size, 10),
		)
	}
	switch i.Kind {
	case ProbeHostActionUnitFile:
		sum := sha256.Sum256([]byte(ProbeSystemdUnit(r)))
		lines = append(lines, "unit_sha256="+hex.EncodeToString(sum[:]))
	case ProbeHostActionContainer:
		sum := sha256.Sum256([]byte(probeDockerArgsDigestInput(ProbeDockerCreateArgs(r))))
		lines = append(lines, "container_sha256="+hex.EncodeToString(sum[:]))
	case ProbeHostActionImageCache:
		lines = append(lines,
			"payload_sha256="+i.Payload.SHA256,
			"payload_size="+strconv.FormatInt(i.Payload.Size, 10),
			"payload_layout_index_sha256="+i.Payload.LayoutIndexSHA256,
		)
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
