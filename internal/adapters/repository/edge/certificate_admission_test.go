package edge

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestEdgeCertificateAdmissionFencesDelayedHandshake(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	binding, err := s.ReadEnrollment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	binding.CertificateFingerprint = testIdentity().Fingerprint
	binding.CertificateNotBefore = c.CreatedAt.Add(-time.Hour)
	binding.CertificateNotAfter = c.CreatedAt.Add(24 * time.Hour)
	prepared := certificateResult(t, s, a, c, "applied")
	admitted, err := s.AcceptCredentialConnection(t.Context(), binding, 2)
	deadline := c.CreatedAt.Add(domain.ProbeCredentialOverlap)
	if err != nil || admitted.ValidUntil == nil || !admitted.ValidUntil.Equal(deadline) {
		t.Fatal("prepared session escaped original deadline", admitted.ValidUntil, err)
	}
	a.ConnectionGeneration = 2
	certificateResult(t, s, a, certificateActivation(c, prepared), "applied")
	state, err := s.ReadActiveCertificate(t.Context())
	if err != nil || state.Certificate == nil {
		t.Fatal(err)
	}
	current := binding
	current.CertificateFingerprint = state.Certificate.Fingerprint
	current.CertificateNotBefore = state.Certificate.NotBefore
	current.CertificateNotAfter = state.Certificate.NotAfter
	admitted, err = s.AcceptCredentialConnection(t.Context(), binding, 3)
	if err != nil || admitted.ValidUntil == nil || !admitted.ValidUntil.Equal(deadline) {
		t.Fatal("old handshake escaped retirement bound", admitted.ValidUntil, err)
	}
	s.commandNow = func() time.Time { return deadline }
	if _, err := s.AcceptCredentialConnection(t.Context(), binding, 4); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("retired handshake admitted", err)
	}
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.ConnectionGeneration != 3 {
		t.Fatal("rejected handshake advanced authority", i, err)
	}
	// Failed admission must nevertheless persist the expiry observation.
	s.commandNow = func() time.Time { return c.CreatedAt.Add(time.Minute) }
	if _, err := s.AcceptCredentialConnection(t.Context(), binding, 4); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("backward clock revived old handshake", err)
	}
	admitted, err = s.AcceptCredentialConnection(t.Context(), current, 4)
	if err != nil || admitted.ValidUntil == nil || !admitted.ValidUntil.Equal(current.CertificateNotAfter) || admitted.CertificateFingerprint != current.CertificateFingerprint {
		t.Fatal("current certificate lost admission", admitted.ValidUntil, err)
	}
}

func TestEdgeCertificateAdmissionRejectsMissingAndForgedProof(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	binding, err := s.ReadEnrollment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared := certificateResult(t, s, a, c, "applied")
	certificateResult(t, s, a, certificateActivation(c, prepared), "applied")
	state, err := s.ReadActiveCertificate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.EdgeEnrollment){
		"missing": func(v *domain.EdgeEnrollment) {
			v.CertificateFingerprint = ""
			v.CertificateNotBefore = time.Time{}
			v.CertificateNotAfter = time.Time{}
		},
		"unknown pin":      func(v *domain.EdgeEnrollment) { v.CertificateFingerprint = c.RotationID },
		"wrong expiry":     func(v *domain.EdgeEnrollment) { v.CertificateNotAfter = v.CertificateNotAfter.Add(time.Hour) },
		"wrong not before": func(v *domain.EdgeEnrollment) { v.CertificateNotBefore = v.CertificateNotBefore.Add(time.Minute) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := binding
			bad.CertificateFingerprint = state.Certificate.Fingerprint
			bad.CertificateNotBefore = state.Certificate.NotBefore
			bad.CertificateNotAfter = state.Certificate.NotAfter
			mutate(&bad)
			if _, err := s.AcceptCredentialConnection(t.Context(), bad, 2); !errors.Is(err, ports.ErrConflict) {
				t.Fatal("invalid TLS proof admitted", err)
			}
		})
	}
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.ConnectionGeneration != 1 {
		t.Fatal("invalid TLS proof advanced generation", i, err)
	}
}

func TestEdgeCertificateAdmissionUsesEarliestCredentialAndCertificateDeadline(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	binding, err := s.ReadEnrollment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	binding.CertificateFingerprint = testIdentity().Fingerprint
	binding.CertificateNotBefore = c.CreatedAt.Add(-time.Hour)
	binding.CertificateNotAfter = c.CreatedAt.Add(time.Minute)
	certificateResult(t, s, a, c, "applied")
	admitted, err := s.AcceptCredentialConnection(t.Context(), binding, 2)
	if err != nil || admitted.ValidUntil == nil || !admitted.ValidUntil.Equal(binding.CertificateNotAfter) {
		t.Fatal("certificate expiry escaped session bound", admitted.ValidUntil, err)
	}
	s.commandNow = func() time.Time { return binding.CertificateNotAfter }
	if _, err := s.AcceptCredentialConnection(t.Context(), binding, 3); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired bootstrap handshake admitted", err)
	}
	t.Run("credential expires first", func(t *testing.T) {
		store, _, authority, credential, _, _ := credentialFixture(t)
		requireCredentialOutcome(t, store, authority, credential, "applied")
		old, err := store.ReadEnrollment(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		old.CertificateFingerprint = testIdentity().Fingerprint
		old.CertificateNotBefore = credential.CreatedAt.Add(-time.Hour)
		old.CertificateNotAfter = credential.CreatedAt.Add(time.Hour)
		admitted, err := store.AcceptCredentialConnection(t.Context(), old, 2)
		if err != nil || admitted.ValidUntil == nil || !admitted.ValidUntil.Equal(credential.OverlapExpiresAt) {
			t.Fatal("credential deadline escaped TLS admission", admitted.ValidUntil, err)
		}
	})
}
