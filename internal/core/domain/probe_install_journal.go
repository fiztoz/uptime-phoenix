package domain

import "time"

// Installation journal states describe intent/resource reports, never installed success.
const (
	ProbeInstallQueued             = "queued"
	ProbeInstallRunning            = "running"
	ProbeInstallResourcesReady     = "resources_ready"
	ProbeInstallCancelRequested    = "cancel_requested"
	ProbeInstallFailed             = "failed"
	ProbeInstallCanceled           = "canceled"
	ProbeInstallInterrupted        = "interrupted"
	ProbeInstallSystemd            = "systemd"
	ProbeInstallDocker             = "docker"
	ProbeInstallLayoutVersion      = 1
	ProbeInstallLifetime           = 15 * time.Minute
	ProbeInstallResourcePlanned    = "planned"
	ProbeInstallResourceInFlight   = "in_flight"
	ProbeInstallResourceApplied    = "applied"
	ProbeInstallResourceNotStarted = "not_started"
	ProbeInstallResourceUnknown    = "unknown"
)

// ProbeInstallIdentity is a requested, nonsecret initial-install tuple.
// Artifact fields are intent, not a journal-generated integrity/runtime attestation.
type ProbeInstallIdentity struct {
	OperationID          string
	Host                 ProbeSSHHost
	HostKeyFingerprint   string
	ClientKeyFingerprint string
	RequestedBy          int64
	LayoutVersion        int
	Mode                 string
	Binary               ProbeBinaryPin
	Image                ProbeImageSelection
}

// Valid requires one complete artifact alternative and no hidden unused fields.
func (r ProbeInstallIdentity) Valid() bool {
	host := ProbePreflightIdentity{OperationID: r.OperationID, Host: r.Host, HostKeyFingerprint: r.HostKeyFingerprint, ClientKeyFingerprint: r.ClientKeyFingerprint, RequestedBy: r.RequestedBy}
	if !host.Valid() || r.LayoutVersion != ProbeInstallLayoutVersion {
		return false
	}
	switch r.Mode {
	case ProbeInstallSystemd:
		return r.Binary.Valid() && r.Image == (ProbeImageSelection{})
	case ProbeInstallDocker:
		return r.Image.Valid() && r.Binary == (ProbeBinaryPin{})
	}
	return false
}

// ProbeInstallResourceSpec is one deterministic operation-scoped resource intent.
// Owned is a requested isolation property, not proof of remote creation/ownership.
type ProbeInstallResourceSpec struct {
	Ordinal   int
	Kind      string
	Reference string
	Owned     bool
}

// Resources derives the complete immutable layout; invalid requests have no graph.
func (r ProbeInstallIdentity) Resources() []ProbeInstallResourceSpec {
	if !r.Valid() {
		return nil
	}
	base := "uptime-phoenix-probe-" + r.OperationID
	data := ProbeInstallResourceSpec{Kind: "data_directory", Reference: "/var/lib/" + base, Owned: true}
	var out []ProbeInstallResourceSpec
	if r.Mode == ProbeInstallSystemd {
		out = []ProbeInstallResourceSpec{data, {Kind: "artifact_directory", Reference: "/opt/" + base, Owned: true}, {Kind: "unit_file", Reference: "/etc/systemd/system/" + base + ".service", Owned: true}, {Kind: "service", Reference: base + ".service", Owned: true}}
	} else {
		out = []ProbeInstallResourceSpec{{Kind: "image_cache", Reference: r.Image.Reference(), Owned: false}, data, {Kind: "container", Reference: base, Owned: true}}
	}
	for i := range out {
		out[i].Ordinal = i + 1
	}
	return out
}

// ProbeInstallResourceOutcome is a joined action report, not arbitrary remote output.
type ProbeInstallResourceOutcome struct{ State, ErrorCode string }

// Valid permits only reports an executor may submit; expiry is recorded by storage.
func (o ProbeInstallResourceOutcome) Valid() bool {
	return o.State == ProbeInstallResourceApplied && o.ErrorCode == "" ||
		o.State == ProbeInstallResourceNotStarted && o.ErrorCode == "not_started" ||
		o.State == ProbeInstallResourceUnknown && o.ErrorCode == "outcome_unknown"
}

// ProbeInstallResource records one immutable action attempt in the resource graph.
type ProbeInstallResource struct {
	Spec       ProbeInstallResourceSpec
	State      string
	AttemptID  string
	ErrorCode  string
	StartedAt  time.Time
	FinishedAt time.Time
}

// ProbeInstallOutcome has no success/installed outcome until authenticated proof exists.
type ProbeInstallOutcome struct{ State, ErrorCode string }

// Valid restricts public owner outcomes; cold expiry/unknown are storage transitions.
func (o ProbeInstallOutcome) Valid() bool {
	if o.State == ProbeInstallCanceled {
		return o.ErrorCode == "canceled"
	}
	if o.State == ProbeInstallInterrupted {
		return o.ErrorCode == "owner_interrupted"
	}
	if o.State != ProbeInstallFailed {
		return false
	}
	switch o.ErrorCode {
	case "unsupported_host", "artifact_rejected", "resource_conflict", "execution_failed", "permission_denied", "runtime_unverified", "not_started", "internal":
		return true
	}
	return false
}

// ProbeInstallOperation is a durable, secret-free request and complete resource graph.
// Current database authority, not this snapshot, fences later journal mutations.
type ProbeInstallOperation struct {
	Identity   ProbeInstallIdentity
	OwnerID    string
	State      string
	ErrorCode  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeadlineAt time.Time
	Resources  []ProbeInstallResource
}

// Terminal reports an immutable unsuccessful outcome, never installation success.
func (o ProbeInstallOperation) Terminal() bool {
	return o.State == ProbeInstallFailed || o.State == ProbeInstallCanceled || o.State == ProbeInstallInterrupted
}

func installUTC(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC && t.Nanosecond()%1000 == 0
}

// Valid refuses malformed, incomplete, reordered or causally impossible stored graphs.
func (o ProbeInstallOperation) Valid() bool {
	if !o.Identity.Valid() || !ValidProbeOperationID(o.OwnerID) || !installUTC(o.CreatedAt) || !installUTC(o.UpdatedAt) || !installUTC(o.DeadlineAt) ||
		!o.DeadlineAt.Equal(o.CreatedAt.Add(ProbeInstallLifetime)) || o.UpdatedAt.Before(o.CreatedAt) {
		return false
	}
	specs := o.Identity.Resources()
	if len(o.Resources) != len(specs) {
		return false
	}
	stopped, allApplied, allPlanned, hasInFlight, hasUnknown := false, true, true, false, false
	attempts := make(map[string]bool, len(o.Resources))
	var previousFinish time.Time
	for i, r := range o.Resources {
		if r.Spec != specs[i] {
			return false
		}
		if r.State == ProbeInstallResourceApplied {
			if stopped {
				return false
			}
		} else {
			allApplied = false
			if r.State != ProbeInstallResourcePlanned && stopped {
				return false
			}
			stopped = true
		}
		if r.State != ProbeInstallResourcePlanned {
			allPlanned = false
		}
		if r.State == ProbeInstallResourcePlanned {
			if r.AttemptID != "" || r.ErrorCode != "" || !r.StartedAt.IsZero() || !r.FinishedAt.IsZero() {
				return false
			}
			continue
		}
		if !ValidProbeOperationID(r.AttemptID) || !installUTC(r.StartedAt) || r.StartedAt.Before(o.CreatedAt) || !r.StartedAt.Before(o.DeadlineAt) || r.StartedAt.After(o.UpdatedAt) {
			return false
		}
		if attempts[r.AttemptID] || (!previousFinish.IsZero() && r.StartedAt.Before(previousFinish)) {
			return false
		}
		attempts[r.AttemptID] = true
		if r.State == ProbeInstallResourceInFlight {
			if r.ErrorCode != "" || !r.FinishedAt.IsZero() {
				return false
			}
			hasInFlight = true
			continue
		}
		if !installUTC(r.FinishedAt) || r.FinishedAt.Before(r.StartedAt) || r.FinishedAt.After(o.UpdatedAt) {
			return false
		}
		switch r.State {
		case ProbeInstallResourceApplied:
			if r.ErrorCode != "" || !r.FinishedAt.Before(o.DeadlineAt) {
				return false
			}
			previousFinish = r.FinishedAt
		case ProbeInstallResourceNotStarted:
			if r.ErrorCode != "not_started" || !r.FinishedAt.Before(o.DeadlineAt) {
				return false
			}
		case ProbeInstallResourceUnknown:
			hasUnknown = true
			if (r.ErrorCode != "outcome_unknown" && r.ErrorCode != "deadline_expired" && r.ErrorCode != "owner_interrupted") || r.ErrorCode != o.ErrorCode {
				return false
			}
			if r.ErrorCode != "deadline_expired" && !r.FinishedAt.Before(o.DeadlineAt) {
				return false
			}
		default:
			return false
		}
	}
	switch o.State {
	case ProbeInstallQueued:
		return o.ErrorCode == "" && allPlanned && o.UpdatedAt.Before(o.DeadlineAt)
	case ProbeInstallRunning:
		return o.ErrorCode == "" && !allApplied && !hasUnknown && o.UpdatedAt.Before(o.DeadlineAt)
	case ProbeInstallResourcesReady:
		return o.ErrorCode == "" && allApplied && o.UpdatedAt.Before(o.DeadlineAt)
	case ProbeInstallCancelRequested:
		return o.ErrorCode == "" && !hasUnknown && o.UpdatedAt.Before(o.DeadlineAt)
	case ProbeInstallFailed, ProbeInstallCanceled:
		return !hasInFlight && !hasUnknown && o.UpdatedAt.Before(o.DeadlineAt) && (ProbeInstallOutcome{State: o.State, ErrorCode: o.ErrorCode}).Valid()
	case ProbeInstallInterrupted:
		if hasInFlight {
			return false
		}
		switch o.ErrorCode {
		case "owner_interrupted":
			return o.UpdatedAt.Before(o.DeadlineAt)
		case "deadline_expired":
			return !o.UpdatedAt.Before(o.DeadlineAt)
		case "outcome_unknown":
			return hasUnknown && o.UpdatedAt.Before(o.DeadlineAt)
		}
	}
	return false
}
