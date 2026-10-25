package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeCleanupAdmissions adapts the cleanup admission store to the host-action
// admission port, so cleanup grants reuse the exact host-action authorization
// rules against their own admission table.
type probeCleanupAdmissions struct {
	store ports.ProbeCleanupAdmissionRepository
}

func (a probeCleanupAdmissions) AdmitHostAction(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
	if request.Intent.Kind != domain.ProbeHostActionCleanup {
		return domain.ProbeHostActionAdmission{}, domain.ErrValidation
	}
	return a.store.AdmitCleanupAction(ctx, request)
}

// NewProbeCleanupActionAuthorizationService authorizes cleanup grants: one
// irreversible cleanup admission, then the same domain-separated host-action
// signature.
func NewProbeCleanupActionAuthorizationService(store ports.ProbeCleanupAdmissionRepository, signer ports.ProbeHostActionGrantSigner) (*ProbeHostActionAuthorizationService, error) {
	if binaryStageAuthorizationNil(store) {
		return nil, domain.ErrValidation
	}
	return NewProbeHostActionAuthorizationService(probeCleanupAdmissions{store: store}, signer)
}
