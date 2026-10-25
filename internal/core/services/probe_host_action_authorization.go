package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeHostActionAuthorizationService consumes one exact DB admission for a
// data_directory, unit_file or service attempt before signing, under the same
// rules and shared five-second budget as ProbeBinaryStageAuthorizationService.
type ProbeHostActionAuthorizationService struct {
	repository ports.ProbeHostActionAdmissionRepository
	signer     ports.ProbeHostActionGrantSigner
	publicKey  [32]byte
}

// NewProbeHostActionAuthorizationService binds the store and a separate
// signing capability; the public key is copied once.
func NewProbeHostActionAuthorizationService(repository ports.ProbeHostActionAdmissionRepository, signer ports.ProbeHostActionGrantSigner) (*ProbeHostActionAuthorizationService, error) {
	if binaryStageAuthorizationNil(repository) || binaryStageAuthorizationNil(signer) {
		return nil, domain.ErrValidation
	}
	publicKey := signer.PublicKey()
	if publicKey == ([32]byte{}) {
		return nil, domain.ErrValidation
	}
	return &ProbeHostActionAuthorizationService{repository: repository, signer: signer, publicKey: publicKey}, nil
}

// Authorize performs one irreversible admission then signs its acknowledged
// receipt. Every failure returns a zero grant and never rearms the attempt.
func (s *ProbeHostActionAuthorizationService) Authorize(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
	var zero domain.ProbeHostActionGrant
	if s == nil || binaryStageAuthorizationNil(s.repository) || binaryStageAuthorizationNil(s.signer) {
		return zero, domain.ErrInternal
	}
	if binaryStageAuthorizationNil(ctx) {
		return zero, domain.ErrValidation
	}
	opCtx, cancel := context.WithTimeout(ctx, binaryStageAuthorizationTimeout)
	defer cancel()
	if err := opCtx.Err(); err != nil {
		return zero, err
	}
	if !request.Valid() || request.Challenge.AuthorityKeyID != domain.ProbeBinaryStageAuthorityKeyID(s.publicKey) {
		return zero, domain.ErrValidation
	}
	admission, err := s.repository.AdmitHostAction(opCtx, request)
	if err != nil {
		return zero, installJournalError(err)
	}
	if err := opCtx.Err(); err != nil {
		return zero, err
	}
	if !admission.Valid() || admission.Request != request {
		return zero, domain.ErrInternal
	}
	grant, err := s.signer.SignHostActionAdmission(opCtx, admission)
	if err != nil {
		return zero, installJournalError(err)
	}
	if err := opCtx.Err(); err != nil {
		return zero, err
	}
	if grant.Request != request || !grant.ValidFor(s.publicKey) {
		return zero, domain.ErrInternal
	}
	return grant, nil
}
