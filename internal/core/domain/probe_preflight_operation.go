package domain

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// Preflight states describe discovery, never installation or runtime activation.
const (
	ProbePreflightQueued          = "queued"
	ProbePreflightRunning         = "running"
	ProbePreflightCancelRequested = "cancel_requested"
	ProbePreflightPassed          = "preflight_passed"
	ProbePreflightFailed          = "failed"
	ProbePreflightCanceled        = "canceled"
	ProbePreflightInterrupted     = "interrupted"
)

// ProbePreflightLifetime is a hard database-clock authority bound. Jobs never
// renew, change owners or resume after expiry; no recoverable SSH key is stored.
const ProbePreflightLifetime = 90 * time.Second

// Admission errors distinguish capacity/shutdown from malformed or failed work.
var (
	ErrProbePreflightBusy   = errors.New("SSH preflight capacity exhausted")
	ErrProbePreflightClosed = errors.New("SSH preflight executor is stopped")
)

// ProbePreflightSubmission is write-only transient admission input. PrivateKey
// is borrowed for synchronous parsing only; neither it nor a server-side key
// path belongs in the durable identity or response. Callers clear their buffer.
type ProbePreflightSubmission struct {
	OperationID        string
	Host               ProbeSSHHost
	HostKeyFingerprint string
	RequestedBy        int64
	PrivateKey         []byte
}

// ProbePreflightIdentity is the complete immutable nonsecret request identity.
// Public-key fingerprints bind retries without hashing/storing a private key.
// RequestedBy is historical attribution, not a reusable authorization grant.
type ProbePreflightIdentity struct {
	OperationID          string
	Host                 ProbeSSHHost
	HostKeyFingerprint   string
	ClientKeyFingerprint string
	RequestedBy          int64
}

// ValidSSHKeyFingerprint accepts canonical OpenSSH SHA256 fingerprints only.
func ValidSSHKeyFingerprint(pin string) bool {
	if !strings.HasPrefix(pin, "SHA256:") {
		return false
	}
	value := strings.TrimPrefix(pin, "SHA256:")
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawStdEncoding.EncodeToString(decoded) == value
}

// Valid checks the frozen request without performing I/O.
func (r ProbePreflightIdentity) Valid() bool {
	return ValidProbeOperationID(r.OperationID) && r.Host.Valid() && r.RequestedBy > 0 &&
		ValidSSHKeyFingerprint(r.HostKeyFingerprint) && ValidSSHKeyFingerprint(r.ClientKeyFingerprint)
}

// ProbePreflightOutcome contains only fixed result/error fields. ErrorCode is
// an allowlisted machine code; raw remote/library errors never belong here.
type ProbePreflightOutcome struct {
	State     string
	ErrorCode string
	Result    *ProbeHostPreflight
}

// Valid checks a terminal outcome independently of a stored operation.
func (o ProbePreflightOutcome) Valid() bool {
	switch o.State {
	case ProbePreflightPassed:
		return o.ErrorCode == "" && o.Result != nil && o.Result.OS == "linux" &&
			(o.Result.Architecture == "amd64" || o.Result.Architecture == "arm64") &&
			(o.Result.Privilege == "root" || o.Result.Privilege == "sudo") && (o.Result.DockerCLI || o.Result.Systemd)
	case ProbePreflightCanceled:
		return o.Result == nil && o.ErrorCode == "canceled"
	case ProbePreflightInterrupted:
		return o.Result == nil && o.ErrorCode == "interrupted"
	case ProbePreflightFailed:
		if o.Result != nil {
			return false
		}
		switch o.ErrorCode {
		case "ssh_trust", "ssh_destination", "ssh_connect", "ssh_discovery", "unsupported_platform", "missing_privilege", "missing_runtime", "timeout", "internal":
			return true
		}
	}
	return false
}

// ProbePreflightOperation is one durable, immutable-request discovery receipt.
// OwnerID and DeadlineAt fence writes in the database, not from a cached copy.
// No private key, path to a server-side key, password, or terminal log exists.
type ProbePreflightOperation struct {
	Identity   ProbePreflightIdentity
	OwnerID    string
	State      string
	ErrorCode  string
	Result     *ProbeHostPreflight
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeadlineAt time.Time
}

// Terminal reports whether an operation may no longer execute or change outcome.
func (o ProbePreflightOperation) Terminal() bool {
	return o.State == ProbePreflightPassed || o.State == ProbePreflightFailed ||
		o.State == ProbePreflightCanceled || o.State == ProbePreflightInterrupted
}

// Valid checks a complete stored receipt, including its timestamp ordering.
func (o ProbePreflightOperation) Valid() bool {
	if !o.Identity.Valid() || !ValidProbeOperationID(o.OwnerID) || o.CreatedAt.IsZero() ||
		o.UpdatedAt.IsZero() || !o.DeadlineAt.Equal(o.CreatedAt.Add(ProbePreflightLifetime)) || o.UpdatedAt.Before(o.CreatedAt) {
		return false
	}
	if o.Terminal() {
		return (ProbePreflightOutcome{State: o.State, ErrorCode: o.ErrorCode, Result: o.Result}).Valid()
	}
	return (o.State == ProbePreflightQueued || o.State == ProbePreflightRunning || o.State == ProbePreflightCancelRequested) && o.ErrorCode == "" && o.Result == nil
}
