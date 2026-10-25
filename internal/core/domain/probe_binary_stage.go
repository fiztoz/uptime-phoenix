package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// Binary staging observations are read-only facts, never host or journal authority.
const (
	ProbeBinaryStageAbsent         = "absent"
	ProbeBinaryStageConflict       = "conflict"
	ProbeBinaryStageContentMatches = "content_matches"
	ProbeBinaryStageMarkerName     = ".phoenix-stage-v1"
	MaxProbeBinaryStageMarkerSize  = 2048
)

// Staging errors carry no paths, remote packets, credentials or SSH diagnostics.
var (
	ErrProbeBinaryStageInput      = errors.New("invalid probe binary staging inspection input")
	ErrProbeBinaryStageInspection = errors.New("probe binary staging inspection unavailable")
)

// ProbeBinaryStageIntent names a systemd artifact and a proposed marker tuple.
// OwnerID/AttemptID are labels, not proof of a current committed reservation.
type ProbeBinaryStageIntent struct {
	Identity  ProbeInstallIdentity
	OwnerID   string
	AttemptID string
}

// Valid accepts only the existing systemd layout and canonical public identity.
func (i ProbeBinaryStageIntent) Valid() bool {
	return i.Identity.Valid() && i.Identity.Mode == ProbeInstallSystemd &&
		ValidProbeOperationID(i.OwnerID) && ValidProbeOperationID(i.AttemptID)
}

// Directory derives the existing artifact-directory intent, never a caller path.
func (i ProbeBinaryStageIntent) Directory() string {
	if !i.Valid() {
		return ""
	}
	return i.Identity.Resources()[1].Reference
}

// Marker is a canonical secret-free byte contract for a future staging writer.
// No writer is provided here; matching these bytes grants no remote ownership.
func (i ProbeBinaryStageIntent) Marker() string {
	if !i.Valid() {
		return ""
	}
	r, b := i.Identity, i.Identity.Binary
	return strings.Join([]string{
		"phoenix-binary-stage-v1",
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
		"",
	}, "\n")
}

// MarkerSHA256 identifies the canonical marker bytes, not publisher authenticity.
func (i ProbeBinaryStageIntent) MarkerSHA256() string {
	marker := i.Marker()
	if marker == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(marker))
	return hex.EncodeToString(sum[:])
}

// ProbeBinaryStageObservation binds bounded read-only observations to an intent.
// ContentMatches does not prove a closed directory graph, ownership or a fence.
type ProbeBinaryStageObservation struct {
	Intent ProbeBinaryStageIntent
	State  string
}

// Valid checks shape only; callers must require exact intent equality as well.
func (o ProbeBinaryStageObservation) Valid() bool {
	return o.Intent.Valid() && (o.State == ProbeBinaryStageAbsent ||
		o.State == ProbeBinaryStageConflict || o.State == ProbeBinaryStageContentMatches)
}
