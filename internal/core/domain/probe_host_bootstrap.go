package domain

import (
	"bytes"
	"errors"
)

// MaxProbeHostBootstrapPasswordSize bounds the transient SSH/sudo password.
const MaxProbeHostBootstrapPasswordSize = 1024

// Host bootstrap admission sentinels. They never carry a password, key, path
// or SSH output.
var (
	ErrProbeHostBootstrapBusy   = errors.New("probe host bootstrap capacity exhausted")
	ErrProbeHostBootstrapClosed = errors.New("probe host bootstrap service closed")
)

// ProbeHostBootstrapRequest is one admin request to prepare a host for the
// restricted-key M8 installer, authenticated ONCE with an SSH password.
//
// Password is the SSH login password of Host.User and, when that user is not
// root, the same password is fed to `sudo -S`. It and PrivateKey are transient:
// the service consumes them (clears both buffers once Bootstrap returns) and
// they are never stored, logged or returned. The deployment key's PUBLIC half
// becomes the restricted forced-command entry; the private half is used only
// to verify that entry works. HostKeyFingerprint is the operator-verified pin
// that must match before the password is sent. Helper pins the
// phoenix-probe-admin release file read from the operator artifact directory.
type ProbeHostBootstrapRequest struct {
	Host               ProbeSSHHost
	Password           []byte
	HostKeyFingerprint string
	PrivateKey         []byte
	Helper             ProbeBinaryPin
	RequestedBy        int64
}

// Valid checks shape only. A password is 1..1024 bytes with no NUL, CR or LF
// (sudo -S reads exactly one line). It proves nothing about the host.
func (r ProbeHostBootstrapRequest) Valid() bool {
	return r.Host.Valid() && ValidSSHKeyFingerprint(r.HostKeyFingerprint) && r.RequestedBy > 0 &&
		len(r.Password) > 0 && len(r.Password) <= MaxProbeHostBootstrapPasswordSize &&
		!bytes.ContainsAny(r.Password, "\x00\r\n") &&
		len(r.PrivateKey) > 0 && len(r.PrivateKey) <= MaxProbeInstallPrivateKeySize &&
		r.Helper.Valid()
}

// HelperReleaseName is the release file name of the pinned phoenix-probe-admin
// helper (scripts/release), not an input pathname. Empty for an invalid pin.
func HelperReleaseName(pin ProbeBinaryPin) string {
	if !pin.Valid() {
		return ""
	}
	return "phoenix-probe-admin_" + pin.Version + "_linux_" + pin.Architecture
}

// Per-file outcomes of one bootstrap. "unchanged" means the host already held
// exactly the wanted bytes, owner and mode; nothing was rewritten.
const (
	ProbeHostBootstrapInstalled = "installed"
	ProbeHostBootstrapAdded     = "added"
	ProbeHostBootstrapUnchanged = "unchanged"
)

// ProbeHostBootstrapResult reports what one bootstrap changed and that the
// restricted key was then verified by the host's own install preflight. It
// carries no address, fingerprint, key or host output.
type ProbeHostBootstrapResult struct {
	Helper         string // installed | unchanged
	StageAuthority string // installed | unchanged
	AuthorizedKey  string // added | unchanged
	// Verified is true only when the restricted deployment key logged in as
	// root and the fixed install-preflight reported root=1 and authority=1.
	Verified bool
}

// Valid checks that every outcome is one fixed value and verification held.
func (r ProbeHostBootstrapResult) Valid() bool {
	return (r.Helper == ProbeHostBootstrapInstalled || r.Helper == ProbeHostBootstrapUnchanged) &&
		(r.StageAuthority == ProbeHostBootstrapInstalled || r.StageAuthority == ProbeHostBootstrapUnchanged) &&
		(r.AuthorizedKey == ProbeHostBootstrapAdded || r.AuthorizedKey == ProbeHostBootstrapUnchanged) &&
		r.Verified
}

// Host bootstrap refusal codes. Codes before "writes" are reached before
// anything on the host changed; host_* conflict/layout codes are refused by
// the root script before it replaces anything; verify_* codes mean the files
// were written but the restricted key did not verify. Safe to show an operator.
const (
	ProbeHostBootstrapTrust           = ProbeInstallPreflightTrust
	ProbeHostBootstrapDestination     = ProbeInstallPreflightDestination
	ProbeHostBootstrapUnreachable     = ProbeInstallPreflightUnreachable
	ProbeHostBootstrapOS              = ProbeInstallPreflightOS
	ProbeHostBootstrapArchitecture    = ProbeInstallPreflightArchitecture
	ProbeHostBootstrapAuth            = "host_password_rejected"
	ProbeHostBootstrapPrivilege       = "host_privilege_failed"
	ProbeHostBootstrapUnsafeLayout    = "host_layout_unsafe"
	ProbeHostBootstrapAuthorityClash  = "host_authority_conflict"
	ProbeHostBootstrapKeyClash        = "host_authorized_key_conflict"
	ProbeHostBootstrapTransfer        = "helper_transfer_failed"
	ProbeHostBootstrapFailed          = "host_bootstrap_failed"
	ProbeHostBootstrapVerifyAuth      = "verify_auth_failed"
	ProbeHostBootstrapVerifyHelper    = "verify_helper_failed"
	ProbeHostBootstrapVerifyAuthority = "verify_authority_failed"
)

// ErrProbeHostBootstrap matches every *ProbeHostBootstrapError.
var ErrProbeHostBootstrap = errors.New("probe host bootstrap refused")

// ProbeHostBootstrapError is a definite bootstrap failure with one fixed code.
// It never carries host output, paths, the password or the key.
type ProbeHostBootstrapError struct{ Code string }

// Error renders only the fixed code.
func (e *ProbeHostBootstrapError) Error() string { return "probe host bootstrap refused: " + e.Code }

// Is makes errors.Is(err, ErrProbeHostBootstrap) hold.
func (e *ProbeHostBootstrapError) Is(target error) bool { return target == ErrProbeHostBootstrap }

// ValidProbeHostBootstrapCode reports whether code is one fixed refusal.
func ValidProbeHostBootstrapCode(code string) bool {
	switch code {
	case ProbeHostBootstrapTrust, ProbeHostBootstrapDestination, ProbeHostBootstrapUnreachable,
		ProbeHostBootstrapOS, ProbeHostBootstrapArchitecture, ProbeHostBootstrapAuth,
		ProbeHostBootstrapPrivilege, ProbeHostBootstrapUnsafeLayout, ProbeHostBootstrapAuthorityClash,
		ProbeHostBootstrapKeyClash, ProbeHostBootstrapTransfer, ProbeHostBootstrapFailed,
		ProbeHostBootstrapVerifyAuth, ProbeHostBootstrapVerifyHelper, ProbeHostBootstrapVerifyAuthority:
		return true
	}
	return false
}
