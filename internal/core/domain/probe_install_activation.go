package domain

import (
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Activation states. Active means the probe was initialized, registered,
// started and enrolled; it is not a claim about configuration application.
const (
	ProbeActivationQueued          = "queued"
	ProbeActivationRunning         = "running"
	ProbeActivationActive          = "active"
	ProbeActivationCancelRequested = "cancel_requested"
	ProbeActivationFailed          = "failed"
	ProbeActivationCanceled        = "canceled"
	ProbeActivationInterrupted     = "interrupted"
	ProbeActivationLifetime        = 15 * time.Minute
	ProbeActivationDefaultPort     = 8443
)

// Activation step kinds, in order. probe_init and service_start are host
// actions authorized like the install host actions; registration and
// enrollment run on the hub through the existing probe admin service.
const (
	ProbeActivationProbeInit    = "probe_init"
	ProbeActivationRegistration = "registration"
	ProbeActivationServiceStart = "service_start"
	ProbeActivationEnrollment   = "enrollment"
)

// ProbeActivationIdentity is the immutable, nonsecret activation request for
// one resources_ready systemd install. Install is a copy of that install's
// identity; Endpoint is derived from its SSH host and the listen port.
type ProbeActivationIdentity struct {
	Install              ProbeInstallIdentity
	ProbeKey             string
	Name                 string
	Location             string
	ListenPort           int
	ClientKeyFingerprint string
	RequestedBy          int64
}

// Valid requires a root systemd install identity, a canonical probe key slug,
// a bounded display name, an unprivileged-safe port and the requesting actor.
func (a ProbeActivationIdentity) Valid() bool {
	return a.Install.Valid() && a.Install.Mode == ProbeInstallSystemd && a.Install.Host.User == "root" &&
		validProbeActivationKey(a.ProbeKey) && validProbeActivationText(a.Name, 1, 200) &&
		validProbeActivationText(a.Location, 0, 255) && a.ListenPort >= 1024 && a.ListenPort <= 65535 &&
		ValidSSHKeyFingerprint(a.ClientKeyFingerprint) && a.RequestedBy > 0 && a.Endpoint() != ""
}

// Endpoint is the probe's runtime WebSocket endpoint on the installed host.
func (a ProbeActivationIdentity) Endpoint() string {
	host, _, err := net.SplitHostPort(a.Install.Host.Address)
	if err != nil || host == "" || a.ListenPort < 1 || a.ListenPort > 65535 {
		return ""
	}
	return "wss://" + net.JoinHostPort(host, strconv.Itoa(a.ListenPort)) + "/ws/probe/v1"
}

// Steps derives the fixed, ordered activation step layout.
func (a ProbeActivationIdentity) Steps() []ProbeInstallResourceSpec {
	if !a.Valid() {
		return nil
	}
	install := a.Install.Resources()
	endpoint := a.Endpoint()
	return []ProbeInstallResourceSpec{
		{Ordinal: 1, Kind: ProbeActivationProbeInit, Reference: install[0].Reference, Owned: true},
		{Ordinal: 2, Kind: ProbeActivationRegistration, Reference: endpoint, Owned: false},
		{Ordinal: 3, Kind: ProbeActivationServiceStart, Reference: install[3].Reference, Owned: true},
		{Ordinal: 4, Kind: ProbeActivationEnrollment, Reference: endpoint, Owned: false},
	}
}

// ProbeActivationFacts are the public identity facts probe_init reports. The
// one-time enrollment token is never part of them and is never stored.
type ProbeActivationFacts struct {
	ProbeID                string
	StreamID               string
	CertificateFingerprint string
}

// Valid requires canonical identifiers and a SHA-256 certificate pin.
func (f ProbeActivationFacts) Valid() bool {
	return ValidHubID(f.ProbeID) && f.ProbeID != LocalProbeID && ValidHubID(f.StreamID) && ValidKeyHash(f.CertificateFingerprint)
}

// ProbeActivation is the durable activation receipt for one install.
type ProbeActivation struct {
	Identity   ProbeActivationIdentity
	OwnerID    string
	State      string
	ErrorCode  string
	Facts      ProbeActivationFacts
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeadlineAt time.Time
	Steps      []ProbeInstallResource
}

// Terminal reports an immutable outcome; active is the only success.
func (a ProbeActivation) Terminal() bool {
	switch a.State {
	case ProbeActivationActive, ProbeActivationFailed, ProbeActivationCanceled, ProbeActivationInterrupted:
		return true
	}
	return false
}

// Valid applies the install journal's causal step rules, and binds the
// probe_init facts: present exactly when probe_init is applied.
func (a ProbeActivation) Valid() bool {
	if !a.Identity.Valid() || !ValidProbeOperationID(a.OwnerID) || !installUTC(a.CreatedAt) || !installUTC(a.UpdatedAt) || !installUTC(a.DeadlineAt) ||
		!a.DeadlineAt.Equal(a.CreatedAt.Add(ProbeActivationLifetime)) || a.UpdatedAt.Before(a.CreatedAt) {
		return false
	}
	g, ok := validProbeStepGraph(a.Steps, a.Identity.Steps(), a.ErrorCode, a.CreatedAt, a.UpdatedAt, a.DeadlineAt)
	if !ok {
		return false
	}
	initApplied := a.Steps[0].State == ProbeInstallResourceApplied
	if initApplied != a.Facts.Valid() || (!initApplied && a.Facts != (ProbeActivationFacts{})) {
		return false
	}
	switch a.State {
	case ProbeActivationQueued:
		return a.ErrorCode == "" && g.allPlanned && a.UpdatedAt.Before(a.DeadlineAt)
	case ProbeActivationRunning:
		return a.ErrorCode == "" && !g.allApplied && !g.hasUnknown && a.UpdatedAt.Before(a.DeadlineAt)
	case ProbeActivationActive:
		return a.ErrorCode == "" && g.allApplied && a.UpdatedAt.Before(a.DeadlineAt)
	case ProbeActivationCancelRequested:
		return a.ErrorCode == "" && !g.allApplied && !g.hasUnknown && a.UpdatedAt.Before(a.DeadlineAt)
	case ProbeActivationFailed, ProbeActivationCanceled:
		return !g.hasInFlight && !g.hasUnknown && a.UpdatedAt.Before(a.DeadlineAt) &&
			(ProbeInstallOutcome{State: map[string]string{ProbeActivationFailed: ProbeInstallFailed, ProbeActivationCanceled: ProbeInstallCanceled}[a.State], ErrorCode: a.ErrorCode}).Valid()
	case ProbeActivationInterrupted:
		if g.hasInFlight {
			return false
		}
		switch a.ErrorCode {
		case "owner_interrupted":
			return a.UpdatedAt.Before(a.DeadlineAt)
		case "deadline_expired":
			return !a.UpdatedAt.Before(a.DeadlineAt)
		case "outcome_unknown":
			return g.hasUnknown && a.UpdatedAt.Before(a.DeadlineAt)
		}
	}
	return false
}

// validProbeActivationKey mirrors the registry's slug rule, including the
// reserved local key.
func validProbeActivationKey(key string) bool {
	if key == LocalProbeID || len(key) < 1 || len(key) > 63 || key[0] == '-' || key[len(key)-1] == '-' {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// validProbeActivationText mirrors the registry's rune limits and refuses
// control characters and surrounding whitespace.
func validProbeActivationText(s string, min, max int) bool {
	if n := utf8.RuneCountInString(s); !utf8.ValidString(s) || n < min || n > max || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
