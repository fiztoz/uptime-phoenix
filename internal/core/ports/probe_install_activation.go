package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeActivationRepository records one activation per resources_ready systemd
// install, with the same fencing as the install journal: every transition is
// serialized on the activation row, samples the DB clock after the authority
// lock, and requires the stored owner, state and an unexpired deadline. All
// receipts are complete, valid domain.ProbeActivation values.
type ProbeActivationRepository interface {
	// CreateActivation requires the install operation to exist in state
	// resources_ready with exactly identity.Install. created=true only for a
	// new queued row under ownerID; an exact retry returns the stored receipt
	// (created=false, original owner); a changed identity is ErrConflict.
	CreateActivation(ctx context.Context, identity domain.ProbeActivationIdentity, ownerID string) (*domain.ProbeActivation, bool, error)
	// ObserveActivation reconciles hard expiry (in-flight steps become
	// unknown/deadline_expired, the activation interrupted/deadline_expired).
	ObserveActivation(ctx context.Context, operationID string) (*domain.ProbeActivation, error)
	// BeginActivation is the single queued-to-running transition for ownerID.
	BeginActivation(ctx context.Context, operationID, ownerID string) (*domain.ProbeActivation, error)
	// RequestActivationCancellation records intent on any replica; queued
	// work becomes canceled directly. Terminal receipts are unchanged.
	RequestActivationCancellation(ctx context.Context, operationID string) (*domain.ProbeActivation, error)
	// FinishActivation records failed, canceled or owner_interrupted, under
	// the same rules as FinishInstall. Active is reached only by recording the
	// last applied step, never by Finish.
	FinishActivation(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeActivation, error)
}

// ProbeActivationStepJournal reserves and records activation steps in order,
// exactly like ProbeInstallResourceJournal. Recording probe_init applied
// requires valid facts and stores them atomically; any other report carries
// zero facts. Recording the last step applied makes the activation active.
type ProbeActivationStepJournal interface {
	ReserveActivationStep(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeActivation, bool, error)
	RecordActivationStep(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome, facts domain.ProbeActivationFacts) (*domain.ProbeActivation, error)
}

// ProbeActivationAdmissionRepository irreversibly admits one probe_init or
// service_start attempt: the activation must be running under the intent's
// owner, its install identity must equal the intent's, the step must be in
// flight with the intent's attempt and every earlier step applied, before the
// deadline. Every replay is refused, exactly like AdmitHostAction.
type ProbeActivationAdmissionRepository interface {
	AdmitActivationAction(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error)
}

// ProbeActivationStepExecutor applies one activation step for an exact
// reservation receipt and returns only after the work joined. For
// probe_init it also returns the public facts to record; every other step
// returns zero facts. Errors are recorded as unknown by the runner, so an
// implementation that knows nothing happened reports not_started instead.
type ProbeActivationStepExecutor interface {
	ApplyStep(ctx context.Context, activation domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error)
}

// ProbeActivationHostSession runs the two authorized host steps over one
// transient SSH key. ProbeInit returns the public facts and the one-time
// enrollment token, which the caller must hold only in memory and pass only
// to enrollment. Outcomes follow the install executor: applied, not_started
// (nothing reached the helper) or unknown.
type ProbeActivationHostSession interface {
	ProbeInit(ctx context.Context, intent domain.ProbeHostActionIntent) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, string)
	ServiceStart(ctx context.Context, intent domain.ProbeHostActionIntent) domain.ProbeInstallResourceOutcome
}

// ProbeActivationSessionFactory parses one transient deployment key and
// returns a host session plus the key's PUBLIC fingerprint. It performs no
// SSH I/O and creates nothing durable.
type ProbeActivationSessionFactory interface {
	NewActivationSession(ctx context.Context, privateKey []byte, hostFingerprint string) (ProbeActivationHostSession, string, error)
}
