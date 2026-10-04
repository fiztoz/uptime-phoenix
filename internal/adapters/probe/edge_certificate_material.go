package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeCertificateMaterial generates and validates protected source TLS material.
// Private keys stay local; callers receive only ciphertext or validated TLS state.
type EdgeCertificateMaterial struct {
	protector ports.EdgeCertificateProtector
}

var _ ports.EdgeCertificateMaterial = (*EdgeCertificateMaterial)(nil)
var _ ports.EdgeCertificateRebinder = (*EdgeCertificateMaterial)(nil)

// RebindCertificate preserves the exact key and pin across an explicit reset.
func (g *EdgeCertificateMaterial) RebindCertificate(ctx context.Context, c domain.ProtectedEdgeCertificate, streamID string, now time.Time) (domain.ProtectedEdgeCertificate, error) {
	if g == nil || !domain.ValidHubID(streamID) || streamID == c.StreamID {
		return domain.ProtectedEdgeCertificate{}, domain.ErrValidation
	}
	if _, err := OpenEdgeCertificate(ctx, g.protector, c, now); err != nil {
		return domain.ProtectedEdgeCertificate{}, err
	}
	pem, err := g.protector.OpenCertificate(ctx, c.EdgeCertificateMetadata, c.ProtectedPEM)
	if err != nil {
		return domain.ProtectedEdgeCertificate{}, err
	}
	defer clear(pem)
	c.StreamID = streamID
	c.ProtectedPEM, err = g.protector.SealCertificate(ctx, c.EdgeCertificateMetadata, pem)
	if err != nil {
		return domain.ProtectedEdgeCertificate{}, err
	}
	return c, nil
}

// NewEdgeCertificateMaterial requires the already provisioned local key adapter.
func NewEdgeCertificateMaterial(protector ports.EdgeCertificateProtector) (*EdgeCertificateMaterial, error) {
	if protector == nil {
		return nil, domain.ErrValidation
	}
	return &EdgeCertificateMaterial{protector: protector}, nil
}

// PrepareCertificate performs local bounded generation and purpose-separated
// protection. The caller must atomically persist its result and original receipt.
func (g *EdgeCertificateMaterial) PrepareCertificate(ctx context.Context, a domain.EdgeCommandAuthority, c domain.ProbeCertificateCommand, now time.Time) (domain.ProtectedEdgeCertificate, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProtectedEdgeCertificate{}, err
	}
	if g == nil || g.protector == nil || !domain.ValidHubID(a.HubID) || !domain.ValidHubID(a.ProbeID) || !domain.ValidHubID(a.StreamID) || a.ConnectionGeneration <= 0 || a.ProbeID != c.ProbeID || !domain.ValidProbeCertificateCommand(c) || c.Kind != "certificate.prepare" || now.IsZero() {
		return domain.ProtectedEdgeCertificate{}, domain.ErrValidation
	}
	cert, certPEM, keyPEM, fingerprint, err := generateRuntimeTLSFor(now, c.ValidForDays)
	if err != nil {
		return domain.ProtectedEdgeCertificate{}, errors.New("generate local certificate failed")
	}
	defer clear(keyPEM)
	pem := append(keyPEM, certPEM...)
	defer clear(pem)
	m := domain.EdgeCertificateMetadata{HubID: a.HubID, ProbeID: a.ProbeID, StreamID: a.StreamID, RotationID: c.RotationID, Version: c.CertificateVersion, Fingerprint: fingerprint, CreatedAt: c.CreatedAt.UTC(), NotBefore: cert.Leaf.NotBefore.UTC(), NotAfter: cert.Leaf.NotAfter.UTC()}
	ciphertext, err := g.protector.SealCertificate(ctx, m, pem)
	if err != nil {
		return domain.ProtectedEdgeCertificate{}, err
	}
	return domain.ProtectedEdgeCertificate{EdgeCertificateMetadata: m, ProtectedPEM: ciphertext}, nil
}

// ValidateCertificate authenticates and checks retained material before selection.
func (g *EdgeCertificateMaterial) ValidateCertificate(ctx context.Context, c domain.ProtectedEdgeCertificate, now time.Time) error {
	if g == nil {
		return domain.ErrValidation
	}
	_, err := OpenEdgeCertificate(ctx, g.protector, c, now)
	return err
}

// OpenEdgeCertificate returns local TLS state only after AEAD, key-pair, exact
// fingerprint, validity and metadata verification. It never falls back to a file.
func OpenEdgeCertificate(ctx context.Context, protector ports.EdgeCertificateProtector, c domain.ProtectedEdgeCertificate, now time.Time) (tls.Certificate, error) {
	if protector == nil || now.IsZero() {
		return tls.Certificate{}, domain.ErrValidation
	}
	pem, err := protector.OpenCertificate(ctx, c.EdgeCertificateMetadata, c.ProtectedPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	defer clear(pem)
	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil || len(cert.Certificate) != 1 {
		return tls.Certificate{}, errors.New("invalid protected certificate")
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, errors.New("invalid protected leaf certificate")
	}
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return tls.Certificate{}, errors.New("unsupported protected certificate key")
	}
	hash := sha256.Sum256(cert.Certificate[0])
	leaf := cert.Leaf
	if leaf.IsCA || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return tls.Certificate{}, errors.New("protected certificate is not a TLS server identity")
	}
	if hex.EncodeToString(hash[:]) != c.Fingerprint || !leaf.NotBefore.Equal(c.NotBefore) || !leaf.NotAfter.Equal(c.NotAfter) || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil {
		return tls.Certificate{}, errors.New("protected certificate identity or validity mismatch")
	}
	return cert, nil
}
