package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func certificateFixture(t *testing.T) (*Store, string, domain.EdgeCommandAuthority, domain.ProbeCertificateCommand, *auth.ProbeConfigProtector) {
	t.Helper()
	s, dir, a, credential, _, _ := credentialFixture(t)
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0xC4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.certificates, err = probe.NewEdgeCertificateMaterial(p)
	if err != nil {
		t.Fatal(err)
	}
	c := domain.ProbeCertificateCommand{CommandID: uuid.NewString(), ProbeID: a.ProbeID, Kind: "certificate.prepare", CreatedAt: credential.CreatedAt, ExpiresAt: credential.ExpiresAt, PayloadHash: sha256.Sum256([]byte("original certificate prepare")), RotationID: uuid.NewString(), CertificateVersion: 2, ValidForDays: 365}
	return s, dir, a, c, p
}

func certificateActivation(c domain.ProbeCertificateCommand, out domain.ProbeCommandOutcome) domain.ProbeCertificateCommand {
	c.CommandID, c.Kind, c.ValidForDays, c.ExpectedFingerprint = uuid.NewString(), "certificate.activate", 0, out.CertificateFingerprint
	c.PayloadHash = sha256.Sum256([]byte("original certificate activate"))
	return c
}

func certificateResult(t *testing.T, s *Store, a domain.EdgeCommandAuthority, c domain.ProbeCertificateCommand, status string) domain.ProbeCommandOutcome {
	t.Helper()
	out, err := s.ApplyCertificateCommand(t.Context(), a, c)
	if err != nil || out.Status != status {
		t.Fatalf("certificate result: %+v %v", out, err)
	}
	return out
}

func TestEdgeCertificateRotationRestartAndOriginalReceipts(t *testing.T) {
	s, dir, a, c, protector := certificateFixture(t)
	before, err := s.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared := certificateResult(t, s, a, c, "applied")
	if prepared.CertificateVersion != 2 || !domain.ValidKeyHash(prepared.CertificateFingerprint) || prepared.CertificateNotAfter == nil || prepared.CredentialVersion != 0 {
		t.Fatal("prepare details missing or mixed")
	}
	state, err := s.ReadActiveCertificate(t.Context())
	if err != nil || state.ActiveVersion != 1 || state.HighestVersion != 2 || state.Certificate != nil {
		t.Fatal("preparation activated certificate", state, err)
	}
	var row edgeCertificateRotation
	if err := s.db.NewSelect().Model(&row).Where("rotation_id = ?", c.RotationID).Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	original := row.protected()
	plain, err := protector.OpenCertificate(t.Context(), original.EdgeCertificateMetadata, original.ProtectedPEM)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plain)
	if !bytes.Contains(plain, []byte("PRIVATE KEY")) {
		t.Fatal("fixture lacks real private key")
	}
	activate := certificateActivation(c, prepared)
	applied := certificateResult(t, s, a, activate, "applied")
	if applied.CertificateVersion != 0 || applied.CertificateFingerprint != "" || applied.CertificateNotAfter != nil {
		t.Fatal("activation exposed prepare details")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	material, err := probe.NewEdgeCertificateMaterial(protector)
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, testIdentity(), WithCertificateMaterial(material))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.commandNow = func() time.Time { return c.ExpiresAt.Add(time.Hour) }
	if err := s.AcceptConnectionGeneration(t.Context(), a.HubID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyCertificateCommand(t.Context(), a, activate); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale session recovered receipt", err)
	}
	a.ConnectionGeneration = 2
	if got := certificateResult(t, s, a, activate, "applied"); !reflect.DeepEqual(got, applied) {
		t.Fatal("activation receipt changed")
	}
	if got := certificateResult(t, s, a, c, "applied"); !reflect.DeepEqual(got, prepared) {
		t.Fatal("preparation receipt changed")
	}
	state, err = s.ReadActiveCertificate(t.Context())
	if err != nil || state.ActiveVersion != 2 || state.Certificate == nil || !reflect.DeepEqual(*state.Certificate, original) {
		t.Fatal("active material changed on restart", err)
	}
	if _, err := probe.OpenEdgeCertificate(t.Context(), protector, *state.Certificate, s.commandNow()); err != nil {
		t.Fatal(err)
	}
	after, err := s.ReadIdentity(t.Context())
	before.ConnectionGeneration = 2
	if err != nil || before != after {
		t.Fatal("certificate rotation changed source identity/progress", err)
	}
	var count int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &count); err != nil || count != 2 {
		t.Fatal("receipt duplicated", count, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, plain) || bytes.Contains(data, []byte("PRIVATE KEY")) {
			t.Fatal("plaintext private material persisted")
		}
	}
}

func TestEdgeCertificateRotationAtomicFailures(t *testing.T) {
	for name, point := range map[string]string{"prepare journal": "BEFORE INSERT ON edge_certificate_rotations", "prepare high water": "BEFORE UPDATE ON edge_certificate_state", "prepare receipt": "BEFORE INSERT ON edge_applied_commands", "activate pointer": "BEFORE UPDATE OF active_version ON edge_certificate_state", "activate journal": "BEFORE UPDATE OF activated_at ON edge_certificate_rotations", "activate receipt": "BEFORE INSERT ON edge_applied_commands"} {
		t.Run(name, func(t *testing.T) {
			s, _, a, c, _ := certificateFixture(t)
			activating := strings.HasPrefix(name, "activate")
			if activating {
				c = certificateActivation(c, certificateResult(t, s, a, c, "applied"))
			}
			if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER certificate_failure "+point+" BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
				t.Fatal(err)
			}
			out, err := s.ApplyCertificateCommand(t.Context(), a, c)
			if !errors.Is(err, ErrStorage) || out.Status != "" {
				t.Fatal("failed write returned success", out, err)
			}
			state, err := s.ReadActiveCertificate(t.Context())
			var receipts, rotations int
			if err != nil {
				t.Fatal(err)
			}
			if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &receipts); err != nil {
				t.Fatal(err)
			}
			if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_certificate_rotations").Scan(t.Context(), &rotations); err != nil {
				t.Fatal(err)
			}
			wantRows, wantHigh := 0, int64(1)
			if activating {
				wantRows, wantHigh = 1, 2
			}
			if state.ActiveVersion != 1 || state.HighestVersion != wantHigh || receipts != wantRows || rotations != wantRows {
				t.Fatal("partial certificate transaction", state, receipts, rotations)
			}
			if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER certificate_failure"); err != nil {
				t.Fatal(err)
			}
			certificateResult(t, s, a, c, "applied")
		})
	}
}

func TestEdgeCertificateRotationFencesAndIdentity(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	for _, mutate := range []func(*domain.EdgeCommandAuthority){func(v *domain.EdgeCommandAuthority) { v.HubID = uuid.NewString() }, func(v *domain.EdgeCommandAuthority) { v.ProbeID = uuid.NewString() }, func(v *domain.EdgeCommandAuthority) { v.StreamID = uuid.NewString() }, func(v *domain.EdgeCommandAuthority) { v.ConnectionGeneration++ }} {
		bad := a
		mutate(&bad)
		if _, err := s.ApplyCertificateCommand(t.Context(), bad, c); !errors.Is(err, ports.ErrConflict) {
			t.Fatal("foreign certificate authority accepted", err)
		}
	}
	prepared := certificateResult(t, s, a, c, "applied")
	for _, mutate := range []func(*domain.ProbeCertificateCommand){func(v *domain.ProbeCertificateCommand) { v.ValidForDays++ }, func(v *domain.ProbeCertificateCommand) { v.RotationID = uuid.NewString() }, func(v *domain.ProbeCertificateCommand) { v.CertificateVersion++ }, func(v *domain.ProbeCertificateCommand) { v.PayloadHash[0] ^= 1 }} {
		bad := c
		mutate(&bad)
		if _, err := s.ApplyCertificateCommand(t.Context(), a, bad); !errors.Is(err, ports.ErrConflict) {
			t.Fatal("changed command reused receipt", err)
		}
	}
	duplicate := c
	duplicate.CommandID = uuid.NewString()
	if got := certificateResult(t, s, a, duplicate, "already_applied"); got.CertificateFingerprint != prepared.CertificateFingerprint || !got.AppliedAt.Equal(*prepared.AppliedAt) {
		t.Fatal("rotation regenerated material")
	}
	bad := certificateActivation(c, prepared)
	bad.ExpectedFingerprint = strings.Repeat("a", 64)
	if got := certificateResult(t, s, a, bad, "rejected"); got.Code != "rotation_conflict" {
		t.Fatal(got)
	}
	bad = certificateActivation(c, prepared)
	bad.CertificateVersion++
	if got := certificateResult(t, s, a, bad, "rejected"); got.Code != "rotation_not_found" {
		t.Fatal(got)
	}
	certificateResult(t, s, a, certificateActivation(c, prepared), "applied")
}

func TestEdgeCertificateRotationExpiryDoesNotRevive(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	prepared := certificateResult(t, s, a, c, "applied")
	s.commandNow = func() time.Time { return c.CreatedAt.Add(domain.ProbeCredentialOverlap) }
	if _, err := s.ReadActiveCertificate(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.commandNow = func() time.Time { return c.CreatedAt.Add(time.Minute) }
	if got := certificateResult(t, s, a, certificateActivation(c, prepared), "rejected"); got.Code != "overlap_expired" {
		t.Fatal("clock rollback revived overlap", got)
	}
	state, err := s.ReadActiveCertificate(t.Context())
	if err != nil || state.ActiveVersion != 1 || state.HighestVersion != 2 {
		t.Fatal(state, err)
	}
	other := c
	other.CommandID, other.RotationID = uuid.NewString(), uuid.NewString()
	if got := certificateResult(t, s, a, other, "rejected"); got.Code != "rotation_conflict" {
		t.Fatal("version reused", got)
	}
}

func TestEdgeCertificateRotationExcludesCredentialOverlap(t *testing.T) {
	for _, certificateFirst := range []bool{false, true} {
		t.Run(strconvBool(certificateFirst), func(t *testing.T) {
			s, _, a, c, _ := certificateFixture(t)
			credential := domain.ProbeCredentialCommand{CommandID: uuid.NewString(), ProbeID: a.ProbeID, Kind: "credential.prepare", CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, PayloadHash: sha256.Sum256([]byte("credential")), RotationID: uuid.NewString(), CredentialVersion: 2, TokenHash: sha256.Sum256([]byte("new credential")), OverlapExpiresAt: c.CreatedAt.Add(domain.ProbeCredentialOverlap)}
			if certificateFirst {
				certificateResult(t, s, a, c, "applied")
				if out, err := s.ApplyCredentialCommand(t.Context(), a, credential); err != nil || out.Status != "rejected" || out.Code != "rotation_in_progress" {
					t.Fatal(out, err)
				}
			} else {
				requireCredentialOutcome(t, s, a, credential, "applied")
				if got := certificateResult(t, s, a, c, "rejected"); got.Code != "rotation_in_progress" {
					t.Fatal(got)
				}
			}
		})
	}
}

func strconvBool(v bool) string {
	if v {
		return "certificate first"
	}
	return "credential first"
}

func TestEdgeCertificateRotationConcurrentRetry(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	results := make(chan domain.ProbeCommandOutcome, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { out, err := s.ApplyCertificateCommand(t.Context(), a, c); results <- out; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *domain.ProbeCommandOutcome
	for result := range results {
		if first == nil {
			first = &result
		} else if !reflect.DeepEqual(*first, result) {
			t.Fatal("retry generated different certificate")
		}
	}
	var count int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_certificate_rotations").Scan(t.Context(), &count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestEdgeCertificateRotationMigrationAndDowngrade(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	for _, direction := range []string{"down", "up"} {
		script, err := migrations.ReadFile("migrations/008_certificate_rotation.tx." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(script)); return err }); err != nil {
			t.Fatal(err)
		}
	}
	prepared := certificateResult(t, s, a, c, "applied")
	down, err := migrations.ReadFile("migrations/008_certificate_rotation.tx.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(down)); return err }); err == nil {
		t.Fatal("downgrade discarded pending private key")
	}
	if got := certificateResult(t, s, a, c, "applied"); !reflect.DeepEqual(got, prepared) {
		t.Fatal("failed downgrade changed receipt")
	}
	certificateResult(t, s, a, certificateActivation(c, prepared), "applied")
	if err := s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(down)); return err }); err == nil {
		t.Fatal("downgrade discarded active private key")
	}
}

func TestEdgeCertificateRotationOldReceiptCannotDowngrade(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	first := certificateResult(t, s, a, c, "applied")
	activate := certificateActivation(c, first)
	original := certificateResult(t, s, a, activate, "applied")
	now := c.CreatedAt.Add(11 * time.Minute)
	s.commandNow = func() time.Time { return now }
	next := c
	next.CommandID, next.RotationID, next.CertificateVersion = uuid.NewString(), uuid.NewString(), 3
	next.CreatedAt, next.ExpiresAt = now, now.Add(time.Hour)
	prepared := certificateResult(t, s, a, next, "applied")
	certificateResult(t, s, a, certificateActivation(next, prepared), "applied")
	if got := certificateResult(t, s, a, activate, "applied"); !reflect.DeepEqual(got, original) {
		t.Fatal("old result changed")
	}
	// A different command ID for the same old operation must not repeat its effect.
	activate.CommandID = uuid.NewString()
	certificateResult(t, s, a, activate, "already_applied")
	state, err := s.ReadActiveCertificate(t.Context())
	if err != nil || state.ActiveVersion != 3 || state.Certificate == nil || state.Certificate.Fingerprint != prepared.CertificateFingerprint {
		t.Fatal("old operation undid new certificate", state, err)
	}
}

func TestEdgeCertificateRotationCorruptMaterialNeverActivates(t *testing.T) {
	for name, mutate := range map[string]string{"ciphertext": "protected_pem = zeroblob(length(protected_pem))", "fingerprint": "fingerprint = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'", "validity": "not_after = not_after + 1000000"} {
		t.Run(name, func(t *testing.T) {
			s, _, a, c, _ := certificateFixture(t)
			prepared := certificateResult(t, s, a, c, "applied")
			if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_certificate_rotations SET "+mutate); err != nil {
				t.Fatal(err)
			}
			activate := certificateActivation(c, prepared)
			if name == "fingerprint" {
				activate.ExpectedFingerprint = strings.Repeat("a", 64)
			}
			out, err := s.ApplyCertificateCommand(t.Context(), a, activate)
			if err == nil || out.Status != "" {
				t.Fatal("corrupt material returned success", out, err)
			}
			state, err := s.ReadActiveCertificate(t.Context())
			if err != nil || state.ActiveVersion != 1 {
				t.Fatal("corrupt material became current", state, err)
			}
		})
	}
}

func TestEdgeCertificateRotationRetainsActiveMaterialAndHighWater(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	prepared := certificateResult(t, s, a, c, "applied")
	certificateResult(t, s, a, certificateActivation(c, prepared), "applied")
	// Simulate an elapsed retention window without requiring an expired active cert.
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_certificate_rotations SET retain_until = 0"); err != nil {
		t.Fatal(err)
	}
	now := c.CreatedAt.Add(11 * time.Minute)
	s.commandNow = func() time.Time { return now }
	next := c
	next.CommandID, next.RotationID = uuid.NewString(), uuid.NewString()
	next.CertificateVersion = 3
	next.CreatedAt, next.ExpiresAt = now, now.Add(time.Hour)
	certificateResult(t, s, a, next, "applied")
	state, err := s.ReadActiveCertificate(t.Context())
	if err != nil || state.ActiveVersion != 2 || state.Certificate == nil || state.Certificate.Fingerprint != prepared.CertificateFingerprint {
		t.Fatal("cleanup discarded active material", state, err)
	}
	// A never-activated expired candidate can leave the bounded journal, but its
	// version may never be reused; current material remains selected throughout.
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_certificate_rotations SET retain_until = 0 WHERE version = 3"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute)
	third := next
	third.CommandID, third.RotationID = uuid.NewString(), uuid.NewString()
	third.CertificateVersion = 4
	third.CreatedAt, third.ExpiresAt = now, now.Add(time.Hour)
	certificateResult(t, s, a, third, "applied")
	var count int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_certificate_rotations WHERE version=3").Scan(t.Context(), &count); err != nil || count != 0 {
		t.Fatal("expired inactive candidate not pruned", count, err)
	}
	reused := third
	reused.CommandID, reused.RotationID = uuid.NewString(), uuid.NewString()
	reused.CertificateVersion = 3
	if out := certificateResult(t, s, a, reused, "rejected"); out.Code != "rotation_conflict" {
		t.Fatal("high-water reused", out)
	}
}

func TestEdgeCertificateRotationCapacityPreservesReceipts(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	prepared := certificateResult(t, s, a, c, "applied")
	// Populate bounded retained history from a real encrypted entry; none of these
	// inactive test rows is selected/decrypted or treated as a valid certificate.
	var template edgeCertificateRotation
	if err := s.db.NewSelect().Model(&template).Where("rotation_id = ?", c.RotationID).Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		for version := int64(3); version <= maxCertificateRotations+1; version++ {
			row := template
			row.RotationID = uuid.NewString()
			row.Version = version
			row.OverlapClosed = true
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE edge_certificate_state SET highest_version = ?", maxCertificateRotations+1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := c.CreatedAt.Add(11 * time.Minute)
	s.commandNow = func() time.Time { return now }
	next := c
	next.CommandID, next.RotationID = uuid.NewString(), uuid.NewString()
	next.CertificateVersion = maxCertificateRotations + 2
	next.CreatedAt, next.ExpiresAt = now, now.Add(time.Hour)
	out, err := s.ApplyCertificateCommand(t.Context(), a, next)
	if !errors.Is(err, ErrQueueFull) || out.Status != "" {
		t.Fatal("capacity fabricated receipt", out, err)
	}
	if got := certificateResult(t, s, a, c, "applied"); !reflect.DeepEqual(got, prepared) {
		t.Fatal("capacity changed original receipt")
	}
	var receipts int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &receipts); err != nil || receipts != 1 {
		t.Fatal("capacity wrote receipt", receipts, err)
	}
}

func TestEdgeCertificateMigrationPreservesAppliedAcknowledgement(t *testing.T) {
	s, _, a, command := ackFixture(t)
	original, err := s.ApplyAlertAcknowledgement(t.Context(), a, command)
	if err != nil || original.Status != "applied" {
		t.Fatal(original, err)
	}
	before, err := s.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"down", "up"} {
		script, err := migrations.ReadFile("migrations/008_certificate_rotation.tx." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(script)); return err }); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ApplyAlertAcknowledgement(t.Context(), a, command)
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatal("certificate migration changed ACK", got, err)
	}
	after, err := s.ReadIdentity(t.Context())
	if err != nil || before != after {
		t.Fatal("certificate migration changed identity/progress", err)
	}
	assertAckStorage(t, s, domain.AlertStatusAcked, 2, 3, 1)
}

func TestEdgeCertificateMissingActiveRowDoesNotSelectBootstrap(t *testing.T) {
	s, _, a, c, _ := certificateFixture(t)
	prepared := certificateResult(t, s, a, c, "applied")
	certificateResult(t, s, a, certificateActivation(c, prepared), "applied")
	if _, err := s.db.ExecContext(t.Context(), "DELETE FROM edge_certificate_rotations WHERE version = 2"); err != nil {
		t.Fatal(err)
	}
	state, err := s.ReadActiveCertificate(t.Context())
	if !errors.Is(err, ErrStorage) || state.ActiveVersion != 0 {
		t.Fatal("missing active material selected bootstrap", state, err)
	}
}
