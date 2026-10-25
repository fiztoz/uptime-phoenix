package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeActivationAdmissions adapts the activation admission store to the
// host-action admission port, so activation host steps reuse the exact
// host-action authorization rules against their own admission table.
type probeActivationAdmissions struct {
	store ports.ProbeActivationAdmissionRepository
}

func (a probeActivationAdmissions) AdmitHostAction(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
	if request.Intent.Kind != domain.ProbeActivationProbeInit && request.Intent.Kind != domain.ProbeActivationServiceStart {
		return domain.ProbeHostActionAdmission{}, domain.ErrValidation
	}
	return a.store.AdmitActivationAction(ctx, request)
}

// NewProbeActivationActionAuthorizationService authorizes probe_init and
// service_start grants: one irreversible activation admission, then the same
// domain-separated host-action signature.
func NewProbeActivationActionAuthorizationService(store ports.ProbeActivationAdmissionRepository, signer ports.ProbeHostActionGrantSigner) (*ProbeHostActionAuthorizationService, error) {
	if binaryStageAuthorizationNil(store) {
		return nil, domain.ErrValidation
	}
	return NewProbeHostActionAuthorizationService(probeActivationAdmissions{store: store}, signer)
}
