package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestAssignmentEvaluatorsRejectUnscopedRepositories(t *testing.T) {
	ctx := context.Background()
	monitor := &domain.Monitor{ID: 1, CertExpiryNotify: true}
	certNotifier := &certFakeNotifier{}
	certRepo := newCertFakeTLSRepo()
	certs := NewCertificateAlertService(certRepo, certNotifier, nil)
	conditionRepo := newConditionRepoFake()
	conditions := NewMonitorConditionService(conditionRepo, nil, nil, nil)
	if err := certs.OnAssignmentCheck(ctx, monitor, "local", 2, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("certificate evaluation fell back to monitor-only state: %v", err)
	}
	if err := conditions.OnAssignmentCheck(ctx, monitor, "local", 2, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("condition evaluation fell back to monitor-only state: %v", err)
	}
	if certNotifier.count() != 0 || len(certRepo.info) != 0 || len(conditionRepo.items) != 0 {
		t.Fatal("unsupported assignment evaluation produced side effects")
	}
	for _, invalid := range []*domain.Monitor{nil, {ID: 0}, {ID: -1}} {
		if err := certs.OnAssignmentCheck(ctx, invalid, "local", 1, nil); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid certificate monitor accepted: %v", err)
		}
		if err := conditions.OnAssignmentCheck(ctx, invalid, "local", 1, nil); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid condition monitor accepted: %v", err)
		}
	}
}
