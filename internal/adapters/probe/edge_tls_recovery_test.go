package probe

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func expiredBootstrapAnchor(t *testing.T) *RuntimeIdentity {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, certificatePEM, keyPEM, pin, err := generateRuntimeTLSFor(time.Now().UTC().Add(-366*24*time.Hour), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(keyPEM)
	combined := append(keyPEM, certificatePEM...)
	defer clear(combined)
	manifest, err := json.Marshal(identityManifest{Version: identityManifestVersion, ProbeID: uuid.NewString(), StreamID: uuid.NewString(), Fingerprint: pin})
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := publishFileNoReplace(root, tlsFileName, combined); err != nil {
		t.Fatal(err)
	}
	if err := publishFileNoReplace(root, identityFileName, manifest); err != nil {
		t.Fatal(err)
	}
	if id, err := OpenRuntimeIdentity(t.Context(), dir); err == nil {
		_ = id.Close()
		t.Fatal("strict identity open accepted expired bootstrap")
	}
	id, err := OpenRuntimeIdentityAnchor(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func decodedCertificateCommand(t *testing.T, c domain.ProbeCertificateCommand) domain.ProbeCertificateCommand {
	t.Helper()
	payload, err := (CertificateCommandCodec{}).EncodeCertificateCommand(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := (CertificateCommandCodec{}).DecodeCertificateCommand(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestEdgeTLSRecoveryExpiredBootstrapRequiresValidActiveMaterial(t *testing.T) {
	id := expiredBootstrapAnchor(t)
	t.Cleanup(func() { _ = id.Close() })
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{95}, 32))
	if err != nil {
		t.Fatal(err)
	}
	material, err := NewEdgeCertificateMaterial(protector)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.EdgeIdentity{ProbeID: id.ProbeID, StreamID: id.StreamID, Fingerprint: id.Fingerprint}
	store, err := edge.Open(t.Context(), id.DataDir, identity, edge.WithCertificateMaterial(material))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := NewEdgeTLSManager(t.Context(), id, store, protector); err == nil {
		t.Fatal("expired bootstrap selected without an active row")
	}
	hubID := uuid.NewString()
	servicesNewEdgeEnrollmentForCommandTest(t, store, hubID, "phx_probe_"+"YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE")
	if err := store.AcceptConnectionGeneration(t.Context(), hubID, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := domain.EdgeCommandAuthority{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID, ConnectionGeneration: 1}
	c := decodedCertificateCommand(t, domain.ProbeCertificateCommand{CommandID: uuid.NewString(), ProbeID: id.ProbeID, Kind: CommandCertificatePrepare, CreatedAt: now, ExpiresAt: now.Add(time.Hour), RotationID: uuid.NewString(), CertificateVersion: 2, ValidForDays: 365})
	prepared, err := store.ApplyCertificateCommand(t.Context(), a, c)
	if err != nil || prepared.Status != "applied" {
		t.Fatal(prepared, err)
	}
	c.CommandID, c.Kind, c.ValidForDays, c.ExpectedFingerprint = uuid.NewString(), CommandCertificateActivate, 0, prepared.CertificateFingerprint
	c = decodedCertificateCommand(t, c)
	if out, err := store.ApplyCertificateCommand(t.Context(), a, c); err != nil || out.Status != "applied" {
		t.Fatal(out, err)
	}
	manager, err := NewEdgeTLSManager(t.Context(), id, store, protector)
	if err != nil {
		t.Fatal(err)
	}
	pin, _, err := manager.CurrentIdentity(t.Context())
	if err != nil || pin != prepared.CertificateFingerprint || pin == id.Fingerprint {
		t.Fatal("active certificate confused with expired anchor", err)
	}
	// Cold start must authenticate the new row without changing either bootstrap
	// file or weakening the ordinary strict identity-opening API.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dir := id.DataDir
	if err := id.Close(); err != nil {
		t.Fatal(err)
	}
	if strict, err := OpenRuntimeIdentity(t.Context(), dir); err == nil {
		_ = strict.Close()
		t.Fatal("strict opening stopped enforcing expiry")
	}
	id, err = OpenRuntimeIdentityAnchor(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err = edge.Open(t.Context(), dir, identity, edge.WithCertificateMaterial(material))
	if err != nil {
		t.Fatal(err)
	}
	manager, err = NewEdgeTLSManager(t.Context(), id, store, protector)
	if err != nil {
		t.Fatal(err)
	}
	if pin, _, err := manager.CurrentIdentity(t.Context()); err != nil || pin != prepared.CertificateFingerprint {
		t.Fatal("cold start lost active pin", err)
	}
	state, err := store.ReadActiveCertificate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt actual retained ciphertext, not a permissive fake repository.
	db, err := sql.Open("sqlite", filepath.Join(dir, "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), "UPDATE edge_certificate_rotations SET protected_pem = zeroblob(length(protected_pem)) WHERE version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reload(t.Context()); err == nil {
		t.Fatal("corrupt active row accepted")
	}
	if pin, _, err := manager.CurrentIdentity(t.Context()); err == nil || pin != "" {
		t.Fatal("corrupt active row fell back to bootstrap")
	}
	if _, err := NewEdgeTLSManager(t.Context(), id, store, protector); err == nil {
		t.Fatal("cold start accepted corrupt active material")
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE edge_certificate_rotations SET protected_pem = ? WHERE version = 2", state.Certificate.ProtectedPEM); err != nil {
		t.Fatal(err)
	}
	if pin, _, err := manager.CurrentIdentity(t.Context()); err != nil || pin != prepared.CertificateFingerprint {
		t.Fatal("restored material did not recover selection", err)
	}
	wrong, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{96}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEdgeTLSManager(t.Context(), id, store, wrong); err == nil {
		t.Fatal("wrong local key opened current certificate")
	}
	// A correctly authenticated but expired active certificate must also fail;
	// valid AEAD is not proof that the TLS identity remains usable now.
	expired, certPEM, keyPEM, expiredPin, err := generateRuntimeTLSFor(time.Now().UTC().Add(-2*24*time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(keyPEM)
	pem := append(keyPEM, certPEM...)
	defer clear(pem)
	m := state.Certificate.EdgeCertificateMetadata
	m.Fingerprint, m.NotBefore, m.NotAfter = expiredPin, expired.Leaf.NotBefore.UTC(), expired.Leaf.NotAfter.UTC()
	cipher, err := protector.SealCertificate(t.Context(), m, pem)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE edge_certificate_rotations SET protected_pem = ?, fingerprint = ?, not_before = ?, not_after = ? WHERE version = 2", cipher, m.Fingerprint, m.NotBefore.UnixMicro(), m.NotAfter.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reload(t.Context()); err == nil {
		t.Fatal("expired active certificate loaded")
	}
	if _, err := NewEdgeTLSManager(t.Context(), id, store, protector); err == nil {
		t.Fatal("cold start fell back from expired active certificate")
	}
}
