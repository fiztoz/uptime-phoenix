package domain

import "time"

// ProbeDiagnostics is the complete nonsecret runtime read model for one
// registered probe. It composes the six durable evidence sources an
// administrative read may inspect: registration, enrollment, connection
// watchdog, runtime lease, config publication and applied receipts.
//
// It deliberately excludes every protected secret: the protected runtime
// credential, key hashes, stream/enrollment identities and configuration
// payloads are never part of this model. Endpoint and certificate pin are
// network selection metadata, not authentication material; surfaces that must
// hide them (scoped monitor reads) simply do not map them.
//
// Every pointer is nil when that evidence source has never reported. Absence
// is unreported, never online, applied, ready or failed. Reads never create,
// extend or validate any of these rows, and none of these values authorizes a
// write: lease ownership must be re-proven inside the writing transaction.
type ProbeDiagnostics struct {
	Registration Probe
	Enrollment   *ProbeEnrollmentFacts
	Session      *ProbeSessionFacts
	Runtime      *ProbeRuntimeFacts
	Watchdog     *ProbeWatchdogFacts
	Publication  *ProbeConfigPublicationFacts
	Applied      *ProbeConfigAppliedFacts
}

// ProbeEnrollmentFacts is nonsecret enrollment evidence for one probe. State
// is the stored connection state ("prepared" or "active"); credential and
// certificate versions are monotonic counters, never the material they guard.
type ProbeEnrollmentFacts struct {
	Endpoint            string
	TLSPin              string
	CredentialVersion   int64
	CertificateVersion  int64
	CertificateNotAfter *time.Time
	State               string
	PreparedAt          time.Time
	ActivatedAt         *time.Time
}

// ProbeSessionFacts is the connector (transport) session evidence. Connected
// reports the last committed transport state; a lease that is not currently
// held is never proof of a live connection.
type ProbeSessionFacts struct {
	OwnerID    string
	Generation int64
	LeaseUntil time.Time
	Connected  bool
}

// ProbeRuntimeFacts is the runtime (execution) lease evidence: the durable
// owner, its fencing epoch and the current deadline. This is execution
// ownership only; it says nothing about transport connection health or target
// availability.
type ProbeRuntimeFacts struct {
	OwnerID    string
	Epoch      int64
	LeaseUntil time.Time
}

// ProbeWatchdogFacts is the connection watchdog checkpoint committed by the
// source process. Status is the stored bounded code
// (unarmed/starting/healthy/suspect/lost/recovering); IncidentOpen reports
// whether a source incident identity is currently attached. The incident
// identity itself is not disclosed here.
type ProbeWatchdogFacts struct {
	Status         string
	Version        int64
	ConfigRevision int64
	Armed          bool
	PendingLoss    bool
	IncidentOpen   bool
	UpdatedAt      time.Time
}

// ProbeConfigPublicationFacts is the latest prepared configuration snapshot:
// the published desired document for this probe. Preparation is not
// application; SHA256 binds the exact original document bytes and is used to
// prove matches internally, never disclosed as document content.
type ProbeConfigPublicationFacts struct {
	Revision        int64
	SchemaVersion   int
	SHA256          string
	SourceCreatedAt time.Time
	EffectiveAt     time.Time
	StoredAt        time.Time
}

// ProbeConfigAppliedFacts is the durable application receipt recorded under a
// connector lease. Matching SHA256 with the publication proves the exact
// source document — and therefore its embedded assignment generations — was
// applied; equal revision counters alone prove nothing.
type ProbeConfigAppliedFacts struct {
	Revision        int64
	SHA256          string
	AppliedAt       time.Time
	AssignmentCount int
}
