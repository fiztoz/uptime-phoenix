package repository_test

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestProbeBinaryStageAdmissionStoreRefusals(t *testing.T) {
	r := m8StageRequest(m8StageAuthority(t, 53))
	var missing *repository.ProbeBinaryStageAdmissionStore
	for _, store := range []*repository.ProbeBinaryStageAdmissionStore{missing, repository.NewProbeBinaryStageAdmissionStore(nil)} {
		a, err := store.AdmitBinaryStage(t.Context(), r)
		if a != (domain.ProbeBinaryStageAdmission{}) || err != domain.ErrInternal {
			t.Fatal("missing DB returned admission")
		}
		a, err = store.AdmitBinaryStage(nil, r) //nolint:staticcheck // SA1012: a nil context is the refusal under test
		if a != (domain.ProbeBinaryStageAdmission{}) || err != domain.ErrValidation {
			t.Fatal("nil context accepted")
		}
		var nilContext *struct{ context.Context }
		a, err = store.AdmitBinaryStage(nilContext, r)
		if a != (domain.ProbeBinaryStageAdmission{}) || err != domain.ErrValidation {
			t.Fatal("typed nil context accepted")
		}
		a, err = store.AdmitBinaryStage(t.Context(), domain.ProbeBinaryStageAdmissionRequest{})
		if a != (domain.ProbeBinaryStageAdmission{}) || err != domain.ErrValidation {
			t.Fatal("invalid request accepted")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		a, err = store.AdmitBinaryStage(ctx, r)
		if a != (domain.ProbeBinaryStageAdmission{}) || err != context.Canceled {
			t.Fatal("canceled request accepted")
		}
	}
}
