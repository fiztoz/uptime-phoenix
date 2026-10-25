package services

import (
	"context"
	"reflect"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const binaryStageAuthorizationTimeout = 5 * time.Second

// ProbeBinaryStageAuthorizationService consumes one exact DB admission before
// signing. It grants authority at commit, not continuous remote authority, an
// applied resource or a joined remote action. It has no production caller yet.
type ProbeBinaryStageAuthorizationService struct {
	repository ports.ProbeBinaryStageAdmissionRepository
	signer     ports.ProbeBinaryStageGrantSigner
	publicKey  [32]byte
}

// NewProbeBinaryStageAuthorizationService binds the store and a separate signing
// capability. The public key is copied once; rotation requires a new service and
// can never reset a stored attempt's consumption.
func NewProbeBinaryStageAuthorizationService(repository ports.ProbeBinaryStageAdmissionRepository, signer ports.ProbeBinaryStageGrantSigner) (*ProbeBinaryStageAuthorizationService, error) {
	if binaryStageAuthorizationNil(repository) || binaryStageAuthorizationNil(signer) {
		return nil, domain.ErrValidation
	}
	publicKey := signer.PublicKey()
	if publicKey == ([32]byte{}) {
		return nil, domain.ErrValidation
	}
	return &ProbeBinaryStageAuthorizationService{repository: repository, signer: signer, publicKey: publicKey}, nil
}

// Authorize performs one irreversible admission then signs its acknowledged
// receipt under one shared five-second budget. Every failure returns zero grant;
// signing failures, canceled callers and lost acknowledgments never rearm work.
func (s *ProbeBinaryStageAuthorizationService) Authorize(ctx context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
	var zero domain.ProbeBinaryStageGrant
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
	admission, err := s.repository.AdmitBinaryStage(opCtx, request)
	if err != nil {
		return zero, installJournalError(err)
	}
	if err := opCtx.Err(); err != nil {
		return zero, err
	}
	if !admission.Valid() || admission.Request != request {
		return zero, domain.ErrInternal
	}
	grant, err := s.signer.SignStageAdmission(opCtx, admission)
	if err != nil {
		return zero, installJournalError(err)
	}
	if err := opCtx.Err(); err != nil {
		return zero, err
	}
	if grant.Request != request || !grant.ValidFor(s.publicKey) {
		return zero, domain.ErrInternal
	}
	if err := opCtx.Err(); err != nil {
		return zero, err
	}
	return grant, nil
}

func binaryStageAuthorizationNil(value interface{}) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
