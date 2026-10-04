package auth

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestEdgeCertificateProtectionBindsScopeAndPurpose(t *testing.T) {
	p, err := NewProbeConfigProtector(bytes.Repeat([]byte{6}, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	m := domain.EdgeCertificateMetadata{HubID: uuid.NewString(), ProbeID: uuid.NewString(), StreamID: uuid.NewString(), RotationID: uuid.NewString(), Version: 2, Fingerprint: strings.Repeat("a", 64), CreatedAt: now, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(24 * time.Hour)}
	plain := []byte("confidential local certificate material")
	encrypted, err := p.SealCertificate(t.Context(), m, plain)
	if err != nil || bytes.Contains(encrypted, plain) {
		t.Fatal("material not protected", err)
	}
	second, err := p.SealCertificate(t.Context(), m, plain)
	if err != nil || bytes.Equal(encrypted, second) {
		t.Fatal("nonce was reused", err)
	}
	got, err := p.OpenCertificate(t.Context(), m, encrypted)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("material did not round trip", err)
	}
	for name, mutate := range map[string]func(*domain.EdgeCertificateMetadata){"hub": func(v *domain.EdgeCertificateMetadata) { v.HubID = uuid.NewString() }, "probe": func(v *domain.EdgeCertificateMetadata) { v.ProbeID = uuid.NewString() }, "stream": func(v *domain.EdgeCertificateMetadata) { v.StreamID = uuid.NewString() }, "rotation": func(v *domain.EdgeCertificateMetadata) { v.RotationID = uuid.NewString() }, "version": func(v *domain.EdgeCertificateMetadata) { v.Version++ }, "fingerprint": func(v *domain.EdgeCertificateMetadata) { v.Fingerprint = strings.Repeat("b", 64) }, "created": func(v *domain.EdgeCertificateMetadata) { v.CreatedAt = v.CreatedAt.Add(time.Second) }, "not before": func(v *domain.EdgeCertificateMetadata) { v.NotBefore = v.NotBefore.Add(time.Second) }, "not after": func(v *domain.EdgeCertificateMetadata) { v.NotAfter = v.NotAfter.Add(time.Second) }} {
		t.Run(name, func(t *testing.T) {
			bad := m
			mutate(&bad)
			if got, err := p.OpenCertificate(t.Context(), bad, encrypted); err == nil || len(got) != 0 {
				t.Fatal("foreign scope exposed certificate")
			}
		})
	}
	wrong, err := NewProbeConfigProtector(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := wrong.OpenCertificate(t.Context(), m, encrypted); err == nil || len(got) != 0 {
		t.Fatal("wrong key exposed certificate")
	}
	for _, bad := range [][]byte{nil, {1}, append(bytes.Clone(encrypted), 0), encrypted[1:]} {
		if got, err := p.OpenCertificate(t.Context(), m, bad); err == nil || len(got) != 0 {
			t.Fatal("corrupt material accepted")
		}
	}
	if _, err := p.SealCertificate(t.Context(), m, make([]byte, domain.MaxEdgeCertificateBytes+1)); err == nil {
		t.Fatal("oversized PEM accepted")
	}
	// A valid tag made for another purpose is not a certificate envelope.
	foreign := p.aead.Seal([]byte{1}, nil, plain, []byte("phoenix.config.aes256gcm.v1"))
	if got, err := p.OpenCertificate(t.Context(), m, foreign); err == nil || len(got) != 0 {
		t.Fatal("foreign purpose exposed certificate")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.OpenCertificate(ctx, m, encrypted); err == nil {
		t.Fatal("canceled operation decrypted")
	}
}
