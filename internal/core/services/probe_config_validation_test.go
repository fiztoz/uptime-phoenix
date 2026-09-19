package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type localConfigValidatorFunc func(context.Context, []byte, domain.ProbeConfigTarget) error

func (f localConfigValidatorFunc) ValidateLocal(ctx context.Context, doc []byte, target domain.ProbeConfigTarget) error {
	return f(ctx, doc, target)
}

func TestLocalConfigValidationServiceExactProtectedRevision(t *testing.T) {
	ctx := context.Background()
	target := domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "local"}
	repo, protector := &configServiceRepo{}, &configServiceProtector{}
	prepared := NewProbeConfigService(repo, configServiceInspector{}, protector)
	meta, err := prepared.Prepare(ctx, target, []byte("fixture-confidential-document"), 6)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	validator := localConfigValidatorFunc(func(_ context.Context, doc []byte, got domain.ProbeConfigTarget) error {
		calls++
		if string(doc) != "fixture-confidential-document" || got != target {
			t.Fatal("validation did not receive authenticated original bytes")
		}
		return nil
	})
	s := NewLocalProbeConfigValidationService(prepared, validator)
	got, err := s.ValidatePrepared(ctx, target, 7)
	if err != nil || !domain.SameProbeConfigMetadata(meta, got) || calls != 1 || repo.gets != 1 || repo.latest != 0 || repo.saves != 1 {
		t.Fatal("validation read latest, wrote state, or lost original metadata")
	}
	for _, revision := range []int64{0, -1, 8} {
		got, err := s.ValidatePrepared(ctx, target, revision)
		if err == nil || got.Revision != 0 || calls != 1 {
			t.Fatal("invalid or mismatched revision reached validator")
		}
	}
	for _, target := range []domain.ProbeConfigTarget{{HubID: meta.HubID, ProbeID: meta.HubID}, {HubID: "22222222-2222-4222-8222-222222222222", ProbeID: "local"}} {
		if _, err := s.ValidatePrepared(ctx, target, 7); err == nil || calls != 1 {
			t.Fatal("wrong target reached validator")
		}
	}
	protector.openErr = fmt.Errorf("fixture-confidential-document: %w", domain.ErrValidation)
	if got, err := s.ValidatePrepared(ctx, target, 7); !errors.Is(err, domain.ErrValidation) || strings.Contains(err.Error(), "fixture-confidential-document") || got.Revision != 0 || calls != 1 {
		t.Fatal("protection failure leaked or reached validator")
	}
}

func TestLocalConfigValidationServiceFailureAndCancellation(t *testing.T) {
	target := domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "local"}
	for _, sentinel := range []error{domain.ErrValidation, domain.ErrInternal, ports.ErrConflict, context.Canceled, context.DeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			repo, protector := &configServiceRepo{}, &configServiceProtector{}
			prepared := NewProbeConfigService(repo, configServiceInspector{}, protector)
			if _, err := prepared.Prepare(context.Background(), target, []byte("secret"), 6); err != nil {
				t.Fatal(err)
			}
			validator := localConfigValidatorFunc(func(context.Context, []byte, domain.ProbeConfigTarget) error {
				return fmt.Errorf("fixture-secret-diagnostic: %w", sentinel)
			})
			got, err := NewLocalProbeConfigValidationService(prepared, validator).ValidatePrepared(context.Background(), target, 7)
			if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "fixture-secret") || got.Revision != 0 || repo.saves != 1 || repo.latest != 0 {
				t.Fatal("failed validation exposed metadata, diagnostics or changed storage")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	repo, protector := &configServiceRepo{}, &configServiceProtector{}
	prepared := NewProbeConfigService(repo, configServiceInspector{}, protector)
	if _, err := prepared.Prepare(ctx, target, []byte("secret"), 6); err != nil {
		t.Fatal(err)
	}
	s := NewLocalProbeConfigValidationService(prepared, localConfigValidatorFunc(func(context.Context, []byte, domain.ProbeConfigTarget) error {
		cancel()
		return nil
	}))
	if got, err := s.ValidatePrepared(ctx, target, 7); !errors.Is(err, context.Canceled) || got.Revision != 0 {
		t.Fatal("late cancellation returned success")
	}
	gets := repo.gets
	if _, err := s.ValidatePrepared(ctx, target, 7); !errors.Is(err, context.Canceled) || repo.gets != gets {
		t.Fatal("canceled validation read storage")
	}
	for _, missing := range []*LocalProbeConfigValidationService{nil, {}, NewLocalProbeConfigValidationService(prepared, nil)} {
		if _, err := missing.ValidatePrepared(context.Background(), target, 7); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("missing dependencies accepted")
		}
	}
}

func TestValidateNotificationTemplateDoesNotNormalizeCaller(t *testing.T) {
	template := &domain.NotificationTemplate{Name: " layout ", Provider: " webhook ", BodyTemplate: "{{monitor.name}}"}
	if err := ValidateNotificationTemplate(template); err != nil || template.Name != " layout " || template.Provider != " webhook " {
		t.Fatal("validation mutated caller data")
	}
	if err := ValidateNotificationTemplate(nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("nil template accepted")
	}
}
