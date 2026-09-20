package services

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type edgeEnrollmentMemory struct {
	issued       domain.EdgeEnrollmentToken
	binding      domain.EdgeEnrollment
	consumedHash [32]byte
	err          error
}

func (m *edgeEnrollmentMemory) IssueEnrollmentToken(_ context.Context, token domain.EdgeEnrollmentToken) error {
	m.issued = token
	return m.err
}
func (m *edgeEnrollmentMemory) EnrollmentTokenValid(_ context.Context, hash [32]byte, at time.Time) (bool, error) {
	return hash == m.issued.Hash && !at.Before(m.issued.IssuedAt) && at.Before(m.issued.ExpiresAt), m.err
}
func (m *edgeEnrollmentMemory) CommitEnrollment(_ context.Context, hash [32]byte, binding domain.EdgeEnrollment) error {
	m.consumedHash, m.binding = hash, binding
	return m.err
}
func (m *edgeEnrollmentMemory) ReadEnrollment(context.Context) (domain.EdgeEnrollment, error) {
	return m.binding, m.err
}

func (m *edgeEnrollmentMemory) ReadRuntimeCredentials(ctx context.Context) ([]domain.EdgeEnrollment, error) {
	binding, err := m.ReadEnrollment(ctx)
	if err != nil {
		return nil, err
	}
	return []domain.EdgeEnrollment{binding}, nil
}

func TestEdgeEnrollmentServiceHashesAndSeparatesCredentials(t *testing.T) {
	m := &edgeEnrollmentMemory{}
	s := NewEdgeEnrollmentService(m)
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.FixedZone("test", 7*60*60))
	token, err := s.Issue(t.Context(), at)
	if err != nil || m.issued.Hash != sha256.Sum256([]byte(token)) || m.issued.IssuedAt.Location() != time.UTC || m.issued.ExpiresAt.Sub(at) != 10*time.Minute {
		t.Fatalf("issued metadata: %+v %v", m.issued, err)
	}
	runtime := "phx_probe_" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	binding := domain.EdgeEnrollment{HubID: "4878b218-e7a6-41d7-b0d3-5d0b93d5caf6", ProbeID: "550f9c0d-980f-4fbe-99b9-5f7b8f12719e", EnrollmentID: "9205cbb2-06cf-432c-9683-fde64e92f10d", CredentialVersion: 1}
	if err := s.Accept(t.Context(), token, runtime, binding, at); err != nil {
		t.Fatal(err)
	}
	if m.consumedHash != sha256.Sum256([]byte(token)) || m.binding.TokenHash != sha256.Sum256([]byte(runtime)) || m.binding.AppliedAt.Location() != time.UTC {
		t.Fatal("plaintext or non-UTC binding passed to storage")
	}
	if _, ok, err := s.AuthenticateRuntime(t.Context(), runtime); err != nil || !ok {
		t.Fatalf("valid auth: %v %v", ok, err)
	}
	for _, invalid := range []string{token, runtime + "A", "phx_probe_" + strings.Repeat("k", 300), "Bearer " + runtime} {
		if _, ok, err := s.AuthenticateRuntime(t.Context(), invalid); err != nil || ok {
			t.Fatalf("invalid auth: %v %v", ok, err)
		}
	}
	if err := s.Accept(t.Context(), token, token, binding, at); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("enrollment credential reused as runtime: %v", err)
	}
	m.err = errors.New("storage unavailable")
	if token, err := s.Issue(t.Context(), at); err == nil || token != "" {
		t.Fatal("unpersisted token returned")
	}
}
