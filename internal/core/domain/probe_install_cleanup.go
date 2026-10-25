package domain

import (
	"errors"
	"time"
)

// ProbeHostActionCleanup removes every host resource one systemd install
// operation created: the unit (stopped, disabled, file removed), the artifact
// directory and the data directory. It is a host action authorized like the
// install actions, under its own one-shot cleanup admission.
const ProbeHostActionCleanup = "cleanup"

// Cleanup states. Cleaned means the helper verified every identity-derived
// resource absent; it says nothing about the hub's probe registration.
const (
	ProbeCleanupQueued      = "queued"
	ProbeCleanupRunning     = "running"
	ProbeCleanupCleaned     = "cleaned"
	ProbeCleanupFailed      = "failed"
	ProbeCleanupInterrupted = "interrupted"
	ProbeCleanupLifetime    = 5 * time.Minute
)

// ErrProbeCleanupIneligible refuses a cleanup whose install still has, or may
// still get, installer or activation work, or that was already cleaned.
var ErrProbeCleanupIneligible = errors.New("probe install is not eligible for cleanup")

// ProbeCleanupIdentity is the immutable, nonsecret cleanup request for one
// install operation. Install is a copy of that operation's identity; the
// client fingerprint is the cleanup's own transient deployment key.
type ProbeCleanupIdentity struct {
	CleanupID            string
	Install              ProbeInstallIdentity
	ClientKeyFingerprint string
	RequestedBy          int64
}

// Valid requires a root systemd install identity and the requesting actor.
func (c ProbeCleanupIdentity) Valid() bool {
	return ValidProbeOperationID(c.CleanupID) && c.Install.Valid() && c.Install.Mode == ProbeInstallSystemd &&
		c.Install.Host.User == "root" && ValidSSHKeyFingerprint(c.ClientKeyFingerprint) && c.RequestedBy > 0
}

// Intent is the cleanup host-action intent for one reserved attempt.
func (c ProbeCleanupIdentity) Intent(ownerID, attemptID string) ProbeHostActionIntent {
	return ProbeHostActionIntent{Identity: c.Install, OwnerID: ownerID, AttemptID: attemptID, Kind: ProbeHostActionCleanup}
}

// ProbeCleanupResourceName is the single path component every resource of an
// install is named after (the data and artifact directories, and the unit
// file with a ".service" suffix).
func ProbeCleanupResourceName(install ProbeInstallIdentity) string {
	if !install.Valid() {
		return ""
	}
	return "uptime-phoenix-probe-" + install.OperationID
}

// ProbeCleanup is the durable cleanup receipt. It is a single step: AttemptID
// is reserved by the queued-to-running transition and the step's outcome
// settles the cleanup.
type ProbeCleanup struct {
	Identity   ProbeCleanupIdentity
	OwnerID    string
	State      string
	ErrorCode  string
	AttemptID  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeadlineAt time.Time
}

// Terminal reports an immutable outcome; cleaned is the only success.
func (c ProbeCleanup) Terminal() bool {
	switch c.State {
	case ProbeCleanupCleaned, ProbeCleanupFailed, ProbeCleanupInterrupted:
		return true
	}
	return false
}

// Valid enforces the fixed state/error/attempt table and the DB-clock
// deadline. failed is definite (nothing reached the host helper);
// interrupted with outcome_unknown means resources may be partly removed.
func (c ProbeCleanup) Valid() bool {
	if !c.Identity.Valid() || !ValidProbeOperationID(c.OwnerID) || !installUTC(c.CreatedAt) || !installUTC(c.UpdatedAt) ||
		!installUTC(c.DeadlineAt) || !c.DeadlineAt.Equal(c.CreatedAt.Add(ProbeCleanupLifetime)) || c.UpdatedAt.Before(c.CreatedAt) {
		return false
	}
	attempt := c.AttemptID != ""
	if attempt && !ValidProbeOperationID(c.AttemptID) {
		return false
	}
	live := c.UpdatedAt.Before(c.DeadlineAt)
	switch c.State {
	case ProbeCleanupQueued:
		return c.ErrorCode == "" && !attempt && live
	case ProbeCleanupRunning, ProbeCleanupCleaned:
		return c.ErrorCode == "" && attempt && live
	case ProbeCleanupFailed:
		return c.ErrorCode == "not_started" && attempt && live
	case ProbeCleanupInterrupted:
		switch c.ErrorCode {
		case "owner_interrupted":
			return !attempt && live
		case "deadline_expired":
			return !live
		case "outcome_unknown":
			return attempt && live
		}
	}
	return false
}

// ProbeInstallCleanupEligible reports whether an install's own state leaves
// no installer work that could still create a resource.
func ProbeInstallCleanupEligible(state string) bool {
	switch state {
	case ProbeInstallResourcesReady, ProbeInstallFailed, ProbeInstallCanceled, ProbeInstallInterrupted:
		return true
	}
	return false
}
