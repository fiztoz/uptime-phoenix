package services

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeActivationAdmin is the subset of ProbeAdminService the hub steps use.
type probeActivationAdmin interface {
	CreateRegistration(ctx context.Context, req ProbeRegistrationRequest) (*domain.Probe, error)
	EnrollSource(ctx context.Context, probeID, enrollmentToken, streamID string) (*domain.ProbeOperation, error)
}

// ProbeActivationExecutor applies the four activation steps for one
// activation: the two host steps through an authorized SSH session and the
// two hub steps through the probe admin service. It is the only holder of
// the one-time enrollment token, in memory, from probe_init until the
// enrollment call; it is cleared after that call whatever its outcome and
// never logged, stored or returned. One executor serves one run.
type ProbeActivationExecutor struct {
	host  ports.ProbeActivationHostSession
	admin probeActivationAdmin
	ready ports.ProbeEndpointReadiness

	mu    sync.Mutex
	token string
}

var _ ports.ProbeActivationStepExecutor = (*ProbeActivationExecutor)(nil)

// probeActivationReadyWait bounds how long enrollment waits for a started
// probe's listener before sending the one-time token.
const probeActivationReadyWait = 30 * time.Second

// NewProbeActivationExecutor binds one host session, the admin service and
// the endpoint readiness check enrollment waits on.
func NewProbeActivationExecutor(host ports.ProbeActivationHostSession, admin *ProbeAdminService, ready ports.ProbeEndpointReadiness) (*ProbeActivationExecutor, error) {
	if admin == nil {
		return nil, domain.ErrValidation
	}
	return newProbeActivationExecutor(host, admin, ready)
}

func newProbeActivationExecutor(host ports.ProbeActivationHostSession, admin probeActivationAdmin, ready ports.ProbeEndpointReadiness) (*ProbeActivationExecutor, error) {
	if binaryStageAuthorizationNil(host) || binaryStageAuthorizationNil(admin) || binaryStageAuthorizationNil(ready) {
		return nil, domain.ErrValidation
	}
	return &ProbeActivationExecutor{host: host, admin: admin, ready: ready}, nil
}

// Forget clears any held enrollment token. The runner's owner calls it when
// the run ends, so a token never outlives its activation.
func (e *ProbeActivationExecutor) Forget() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.token = ""
	e.mu.Unlock()
}

// ApplyStep runs one step for an exact reservation receipt. Reports are
// joined and conservative: applied only on a confirmed effect, not_started
// when nothing was attempted (or the hub refused before any write), unknown
// otherwise. It never returns an error.
func (e *ProbeActivationExecutor) ApplyStep(ctx context.Context, activation domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
	var noFacts domain.ProbeActivationFacts
	notStarted := domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
	unknown := domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}
	applied := domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}
	if e == nil || binaryStageAuthorizationNil(e.host) || binaryStageAuthorizationNil(e.admin) || binaryStageAuthorizationNil(e.ready) || ctx == nil {
		return notStarted, noFacts, nil
	}
	step, ok := probeActivationInFlightStep(activation, kind)
	if !ok {
		return notStarted, noFacts, nil
	}
	identity := activation.Identity
	switch kind {
	case domain.ProbeActivationProbeInit:
		intent := domain.ProbeHostActionIntent{Identity: identity.Install, OwnerID: activation.OwnerID, AttemptID: step.AttemptID, Kind: kind}
		outcome, facts, token := e.host.ProbeInit(ctx, intent)
		if outcome.State != domain.ProbeInstallResourceApplied {
			if !outcome.Valid() {
				return unknown, noFacts, nil
			}
			return outcome, noFacts, nil
		}
		if !facts.Valid() || !ValidEnrollmentToken(token) {
			// The probe initialized something the hub cannot use.
			return unknown, noFacts, nil
		}
		e.mu.Lock()
		e.token = token
		e.mu.Unlock()
		return applied, facts, nil
	case domain.ProbeActivationRegistration:
		if !activation.Facts.Valid() {
			return notStarted, noFacts, nil
		}
		_, err := e.admin.CreateRegistration(ctx, ProbeRegistrationRequest{
			ProbeID: activation.Facts.ProbeID, Key: identity.ProbeKey, Name: identity.Name, Location: identity.Location,
			Endpoint: identity.Endpoint(), TLSPin: activation.Facts.CertificateFingerprint,
		})
		switch {
		case err == nil:
			return applied, noFacts, nil
		case probeActivationDefiniteRefusal(err):
			return notStarted, noFacts, nil
		default:
			return unknown, noFacts, nil
		}
	case domain.ProbeActivationServiceStart:
		intent := domain.ProbeHostActionIntent{Identity: identity.Install, OwnerID: activation.OwnerID, AttemptID: step.AttemptID, Kind: kind}
		outcome := e.host.ServiceStart(ctx, intent)
		if !outcome.Valid() {
			return unknown, noFacts, nil
		}
		return outcome, noFacts, nil
	case domain.ProbeActivationEnrollment:
		e.mu.Lock()
		token := e.token
		e.token = ""
		e.mu.Unlock()
		if token == "" || !activation.Facts.Valid() {
			// The token is lost (or never existed): nothing can be sent.
			return notStarted, noFacts, nil
		}
		// systemd reports a Type=simple unit active before its listener is
		// up. Send the token only once the probe accepts connections; a probe
		// that never becomes reachable received nothing.
		readyCtx, cancel := context.WithTimeout(ctx, probeActivationReadyWait)
		readyErr := e.ready.WaitReady(readyCtx, identity.Endpoint())
		cancel()
		if readyErr != nil {
			token = ""
			return notStarted, noFacts, nil
		}
		operation, err := e.admin.EnrollSource(ctx, activation.Facts.ProbeID, token, activation.Facts.StreamID)
		token = ""
		switch {
		case err == nil && operation != nil && operation.Status == domain.ProbeOperationSucceeded:
			return applied, noFacts, nil
		case err != nil && probeActivationDefiniteRefusal(err):
			return notStarted, noFacts, nil
		default:
			// A failed exchange may or may not have consumed the token.
			return unknown, noFacts, nil
		}
	default:
		return notStarted, noFacts, nil
	}
}

// probeActivationInFlightStep returns the exact in-flight step for kind from
// a running activation whose earlier steps are all applied.
func probeActivationInFlightStep(activation domain.ProbeActivation, kind string) (domain.ProbeInstallResource, bool) {
	if activation.State != domain.ProbeActivationRunning || !activation.Valid() {
		return domain.ProbeInstallResource{}, false
	}
	for i, step := range activation.Steps {
		if step.Spec.Kind != kind {
			if step.State != domain.ProbeInstallResourceApplied {
				return domain.ProbeInstallResource{}, false
			}
			continue
		}
		if step.State != domain.ProbeInstallResourceInFlight || step.AttemptID == "" || i >= len(activation.Steps) {
			return domain.ProbeInstallResource{}, false
		}
		return step, true
	}
	return domain.ProbeInstallResource{}, false
}

// probeActivationDefiniteRefusal reports hub errors returned before any write.
func probeActivationDefiniteRefusal(err error) bool {
	for _, definite := range []error{ErrInvalidRegistration, ErrInvalidOperation, domain.ErrValidation, ports.ErrConflict, ports.ErrNotFound, domain.ErrNotFound} {
		if errors.Is(err, definite) {
			return true
		}
	}
	return false
}
