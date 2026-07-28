package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type fakeStatusPagePasswordHasher struct{}

func (fakeStatusPagePasswordHasher) Hash(password string) (string, error) {
	return "hashed:" + password, nil
}

func (fakeStatusPagePasswordHasher) Verify(hashed, password string) error {
	if hashed != "hashed:"+password {
		return fmt.Errorf("password mismatch")
	}
	return nil
}

func TestGetPublicStatus_ProtectedPageRequiresAccessCodeForContent(t *testing.T) {
	t.Parallel()

	spRepo := newFakeSPRepo()
	incidentRepo := newFakeIncidentRepo()
	spMonitorRepo := newFakeSPMonitorRepo()
	svc := NewStatusPageService(spRepo, incidentRepo, nil, spMonitorRepo, nil, nil, fakeStatusPagePasswordHasher{})
	ctx := context.Background()

	hash, err := svc.SetPassword("correct horse")
	if err != nil {
		t.Fatalf("hash access code: %v", err)
	}
	sp := &domain.StatusPage{
		Slug:         "protected",
		Title:        "Protected",
		Published:    true,
		PasswordHash: hash,
	}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create status page: %v", err)
	}
	if err := incidentRepo.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "Private incident",
		Active:       true,
	}); err != nil {
		t.Fatalf("create incident: %v", err)
	}

	anonymous, err := svc.GetPublicStatus(ctx, sp.Slug)
	if err != nil {
		t.Fatalf("anonymous public status: %v", err)
	}
	if anonymous.StatusPage.PasswordHash == "" {
		t.Fatal("protected metadata lost its access marker before the handler could project has_access")
	}
	if len(anonymous.Monitors) != 0 || len(anonymous.Incidents) != 0 {
		t.Fatalf("anonymous protected payload leaked content: monitors=%d incidents=%d", len(anonymous.Monitors), len(anonymous.Incidents))
	}

	if _, err := svc.GetPublicStatusWithAccess(ctx, sp.Slug, "wrong horse"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong access code error = %v, want domain.ErrUnauthorized", err)
	}

	verified, err := svc.GetPublicStatusWithAccess(ctx, sp.Slug, "correct horse")
	if err != nil {
		t.Fatalf("verified public status: %v", err)
	}
	if len(verified.Incidents) != 1 || verified.Incidents[0].Title != "Private incident" {
		t.Fatalf("verified payload incidents = %+v, want private incident", verified.Incidents)
	}
}

func TestSetStatusPagePassword_ValidatesAccessCodeLength(t *testing.T) {
	t.Parallel()

	svc := NewStatusPageService(nil, nil, nil, nil, nil, nil, fakeStatusPagePasswordHasher{})
	tests := []struct {
		name     string
		password string
	}{
		{name: "short", password: "1234567"},
		{name: "over bcrypt byte limit", password: strings.Repeat("a", StatusPageAccessCodeMaxBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.SetPassword(tt.password); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("SetPassword error = %v; want domain.ErrValidation", err)
			}
		})
	}
}

func TestSetStatusPagePassword_EmptyClearsAndValidUsesPort(t *testing.T) {
	t.Parallel()

	svc := NewStatusPageService(nil, nil, nil, nil, nil, nil, fakeStatusPagePasswordHasher{})
	cleared, err := svc.SetPassword("")
	if err != nil || cleared != "" {
		t.Fatalf("clear password = %q, %v; want empty hash and nil error", cleared, err)
	}

	hash, err := svc.SetPassword("valid password")
	if err != nil {
		t.Fatalf("SetPassword valid error: %v", err)
	}
	if hash != "hashed:valid password" {
		t.Fatalf("hash = %q; want password-port output", hash)
	}
}
