package probe

import (
	"bytes"
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestEdgeCertificateMaterialValidatesExactLocalIdentity(t *testing.T) {
	for _, setting := range []string{"", "x509keypairleaf=0"} {
		t.Run(setting, func(t *testing.T) {
			t.Setenv("GODEBUG", setting)
			exerciseEdgeCertificateMaterial(t)
		})
	}
}

func exerciseEdgeCertificateMaterial(t *testing.T) {
	t.Helper()
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewEdgeCertificateMaterial(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := domain.EdgeCommandAuthority{HubID: uuid.NewString(), ProbeID: uuid.NewString(), StreamID: uuid.NewString(), ConnectionGeneration: 1}
	c := domain.ProbeCertificateCommand{CommandID: uuid.NewString(), ProbeID: a.ProbeID, Kind: "certificate.prepare", CreatedAt: now, ExpiresAt: now.Add(time.Hour), PayloadHash: sha256.Sum256([]byte("original prepare")), RotationID: uuid.NewString(), CertificateVersion: 2, ValidForDays: 3650}
	material, err := g.PrepareCertificate(t.Context(), a, c, now)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := OpenEdgeCertificate(t.Context(), p, material, now)
	if err != nil || cert.Leaf == nil || !cert.Leaf.NotAfter.Equal(now.Truncate(time.Second).Add(3650*24*time.Hour)) {
		t.Fatal("certificate validity mismatch", err)
	}
	for name, at := range map[string]time.Time{"before validity": material.NotBefore.Add(-time.Second), "at expiry": material.NotAfter, "after expiry": material.NotAfter.Add(time.Second)} {
		t.Run(name, func(t *testing.T) {
			if _, err := OpenEdgeCertificate(t.Context(), p, material, at); err == nil {
				t.Fatal("invalid certificate time accepted")
			}
		})
	}
	plain, err := p.OpenCertificate(t.Context(), material.EdgeCertificateMetadata, material.ProtectedPEM)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plain)
	for name, mutate := range map[string]func(*domain.ProtectedEdgeCertificate){"fingerprint": func(v *domain.ProtectedEdgeCertificate) { v.Fingerprint = strings.Repeat("f", 64) }, "not after": func(v *domain.ProtectedEdgeCertificate) { v.NotAfter = v.NotAfter.Add(-time.Second) }, "not before": func(v *domain.ProtectedEdgeCertificate) { v.NotBefore = v.NotBefore.Add(time.Second) }} {
		t.Run(name, func(t *testing.T) {
			bad := material
			mutate(&bad)
			bad.ProtectedPEM, err = p.SealCertificate(t.Context(), bad.EdgeCertificateMetadata, plain)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.ValidateCertificate(t.Context(), bad, now); err == nil {
				t.Fatal("authenticated but mismatched certificate accepted")
			}
		})
	}
	bad := material
	bad.ProtectedPEM, err = p.SealCertificate(t.Context(), bad.EdgeCertificateMetadata, []byte("not PEM"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.ValidateCertificate(t.Context(), bad, now); err == nil {
		t.Fatal("invalid PEM accepted")
	}
	for _, days := range []int{0, 3651} {
		c.ValidForDays = days
		if _, err := g.PrepareCertificate(t.Context(), a, c, now); err == nil {
			t.Fatal("invalid lifetime accepted")
		}
	}
}

func TestRuntimeIdentityLeafWithCompatibilitySetting(t *testing.T) {
	t.Setenv("GODEBUG", "x509keypairleaf=0")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := InitializeRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Certificate.Leaf == nil {
		t.Fatal("initialization omitted parsed certificate")
	}
	fingerprint := identity.Fingerprint
	if err := identity.Close(); err != nil {
		t.Fatal(err)
	}
	identity, err = OpenRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = identity.Close() }()
	if identity.Certificate.Leaf == nil || identity.Fingerprint != fingerprint {
		t.Fatal("reopen lost validated leaf identity")
	}
}
