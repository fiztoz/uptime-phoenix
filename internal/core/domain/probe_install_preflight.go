package domain

import "errors"

// ProbeInstallPreflightMaxExisting caps the existing-install count a host
// helper reports; a larger count is reported as the cap.
const ProbeInstallPreflightMaxExisting = 64

// ProbeInstallHostReport is the authorized host helper's nonmutating install
// preflight. It is compatibility evidence gathered as the deployment key's
// root login, never an installation, enrollment or reachability receipt.
type ProbeInstallHostReport struct {
	Kernel    string // uname sysname
	Machine   string // uname machine
	Systemd   bool   // booted with systemd and systemctl present
	DockerCLI bool   // a docker CLI is present at a fixed path
	Root      bool   // the helper's effective uid is 0
	Authority bool   // the trusted stage-authority key loads under protection
	// ExistingInstalls counts Uptime Phoenix probe entries already on the
	// host (data directories, artifact directories and unit files), capped.
	ExistingInstalls int
}

// Valid checks shape only.
func (r ProbeInstallHostReport) Valid() bool {
	return probeInstallReportToken(r.Kernel) && probeInstallReportToken(r.Machine) &&
		r.ExistingInstalls >= 0 && r.ExistingInstalls <= ProbeInstallPreflightMaxExisting
}

func probeInstallReportToken(v string) bool {
	if len(v) < 1 || len(v) > 64 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Install preflight refusal codes. Each is a definite refusal reached before
// any durable write or host change; they are safe to show an operator.
const (
	ProbeInstallPreflightTrust        = "host_trust_failed"
	ProbeInstallPreflightDestination  = "host_destination_refused"
	ProbeInstallPreflightAuth         = "host_auth_failed"
	ProbeInstallPreflightUnreachable  = "host_unreachable"
	ProbeInstallPreflightHelper       = "host_helper_unavailable"
	ProbeInstallPreflightOS           = "host_unsupported_os"
	ProbeInstallPreflightArchitecture = "host_architecture_mismatch"
	ProbeInstallPreflightRoot         = "host_root_required"
	ProbeInstallPreflightSystemd      = "host_systemd_unavailable"
	ProbeInstallPreflightAuthority    = "host_authority_missing"
	ProbeInstallPreflightExisting     = "host_existing_install"
)

// ErrProbeInstallPreflight matches every *ProbeInstallPreflightError.
var ErrProbeInstallPreflight = errors.New("probe install host preflight refused")

// ProbeInstallPreflightError is a definite preflight refusal with one fixed
// code. It never carries host output, paths or credentials.
type ProbeInstallPreflightError struct{ Code string }

func (e *ProbeInstallPreflightError) Error() string {
	return "probe install host preflight refused: " + e.Code
}

// Is makes errors.Is(err, ErrProbeInstallPreflight) hold.
func (e *ProbeInstallPreflightError) Is(target error) bool { return target == ErrProbeInstallPreflight }

// ValidProbeInstallPreflightCode reports whether code is one fixed refusal.
func ValidProbeInstallPreflightCode(code string) bool {
	switch code {
	case ProbeInstallPreflightTrust, ProbeInstallPreflightDestination, ProbeInstallPreflightAuth,
		ProbeInstallPreflightUnreachable, ProbeInstallPreflightHelper, ProbeInstallPreflightOS,
		ProbeInstallPreflightArchitecture, ProbeInstallPreflightRoot, ProbeInstallPreflightSystemd,
		ProbeInstallPreflightAuthority, ProbeInstallPreflightExisting:
		return true
	}
	return false
}

// ProbeInstallHostArchitecture maps a uname machine to a pin architecture, or
// "" when no supported pin can run there.
func ProbeInstallHostArchitecture(machine string) string {
	switch machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return ""
}

// Refusal returns the first preflight refusal for installing identity on the
// reporting host, or "" when the host qualifies. Checks run in a fixed order:
// OS, architecture, root, runtime, authority, then existing installs, so the
// most fundamental mismatch is the one reported. Any existing Phoenix probe
// refuses: a second probe would contend for the same listener, and leftovers
// of an interrupted install are removed by an intentional cleanup, never
// silently adopted or overwritten.
func (r ProbeInstallHostReport) Refusal(identity ProbeInstallIdentity) string {
	if !r.Valid() || !identity.Valid() {
		return ProbeInstallPreflightHelper
	}
	switch {
	case r.Kernel != "Linux":
		return ProbeInstallPreflightOS
	case ProbeInstallHostArchitecture(r.Machine) != identity.Binary.Architecture:
		return ProbeInstallPreflightArchitecture
	case !r.Root:
		return ProbeInstallPreflightRoot
	case identity.Mode == ProbeInstallSystemd && !r.Systemd:
		return ProbeInstallPreflightSystemd
	case !r.Authority:
		return ProbeInstallPreflightAuthority
	case r.ExistingInstalls > 0:
		return ProbeInstallPreflightExisting
	}
	return ""
}
