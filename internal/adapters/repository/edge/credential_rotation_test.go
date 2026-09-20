package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func credentialFixture(t *testing.T) (*Store, string, domain.EdgeCommandAuthority, domain.ProbeCredentialCommand, string, string) {
	t.Helper()
	s, dir := testStore(t)
	_, oldToken := enroll(t, s)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.commandNow = func() time.Time { return now }
	newToken := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x53}, 32))
	a := domain.EdgeCommandAuthority{HubID: testHubID, ProbeID: testIdentity().ProbeID, StreamID: testIdentity().StreamID, ConnectionGeneration: 1}
	c := domain.ProbeCredentialCommand{CommandID: uuid.NewString(), ProbeID: a.ProbeID, Kind: "credential.prepare", CreatedAt: now, ExpiresAt: now.Add(20 * time.Minute), PayloadHash: sha256.Sum256([]byte("exact prepare bytes")), RotationID: uuid.NewString(), CredentialVersion: 2, TokenHash: sha256.Sum256([]byte(newToken)), OverlapExpiresAt: now.Add(10 * time.Minute)}
	return s, dir, a, c, oldToken, newToken
}

func activation(c domain.ProbeCredentialCommand) domain.ProbeCredentialCommand {
	c.CommandID, c.Kind, c.TokenHash, c.OverlapExpiresAt = uuid.NewString(), "credential.activate", [32]byte{}, time.Time{}
	c.PayloadHash = sha256.Sum256([]byte("exact activation bytes"))
	return c
}

func credentialAuth(t *testing.T, s *Store, token string, version int64, deadline *time.Time) domain.EdgeEnrollment {
	t.Helper()
	binding, ok, err := services.NewEdgeEnrollmentService(s).AuthenticateRuntime(t.Context(), token)
	if err != nil || ok != (version > 0) || binding.CredentialVersion != version || !reflect.DeepEqual(binding.ValidUntil, deadline) {
		t.Fatalf("credential auth: version=%d valid=%t deadline=%v err=%v", binding.CredentialVersion, ok, binding.ValidUntil, err)
	}
	return binding
}

func requireCredentialOutcome(t *testing.T, s *Store, a domain.EdgeCommandAuthority, c domain.ProbeCredentialCommand, status string) domain.ProbeCommandOutcome {
	t.Helper()
	out, err := s.ApplyCredentialCommand(t.Context(), a, c)
	if err != nil || out.Status != status {
		t.Fatalf("credential command result: %+v %v", out, err)
	}
	return out
}

func TestEdgeCredentialRotationRestartAndLostActivationReceipt(t *testing.T) {
	s, dir, a, prepare, oldToken, newToken := credentialFixture(t)
	before, err := s.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cachedOld := credentialAuth(t, s, oldToken, 1, nil)
	wantPrepare := requireCredentialOutcome(t, s, a, prepare, "applied")
	if wantPrepare.CredentialVersion != 2 {
		t.Fatal("prepare omitted version")
	}
	credentialAuth(t, s, oldToken, 1, &prepare.OverlapExpiresAt)
	credentialAuth(t, s, newToken, 2, &prepare.OverlapExpiresAt)
	admitted, err := s.AcceptCredentialConnection(t.Context(), cachedOld, 2)
	if err != nil || admitted.ValidUntil == nil || !admitted.ValidUntil.Equal(prepare.OverlapExpiresAt) {
		t.Fatal("cached auth escaped overlap deadline", err)
	}
	a.ConnectionGeneration = 2
	activate := activation(prepare)
	want := requireCredentialOutcome(t, s, a, activate, "applied")
	if want.CredentialVersion != 0 {
		t.Fatal("activate leaked prepare-only details")
	}
	credentialAuth(t, s, newToken, 2, nil)
	credentialAuth(t, s, oldToken, 1, &prepare.OverlapExpiresAt)
	// Lose the result and restart past both command expiry and overlap expiry.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.commandNow = func() time.Time { return prepare.ExpiresAt.Add(time.Hour) }
	credentialAuth(t, s, oldToken, 0, nil)
	newBinding := credentialAuth(t, s, newToken, 2, nil)
	if _, err := s.AcceptCredentialConnection(t.Context(), newBinding, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyCredentialCommand(t.Context(), a, activate); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale session recovered result", err)
	}
	a.ConnectionGeneration = 3
	got := requireCredentialOutcome(t, s, a, activate, "applied")
	gotPrepare := requireCredentialOutcome(t, s, a, prepare, "applied")
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotPrepare, wantPrepare) {
		t.Fatal("retry changed durable receipt")
	}
	after, err := s.ReadIdentity(t.Context())
	before.ConnectionGeneration = 3
	if err != nil || after != before {
		t.Fatal("rotation reset stream, configuration or sequence", err)
	}
	var receipts int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &receipts); err != nil || receipts != 2 {
		t.Fatal("duplicated receipts", receipts, err)
	}
	// Inspect actual source DB/WAL files, not just a struct used before writing.
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
		if bytes.Contains(data, []byte(oldToken)) || bytes.Contains(data, []byte(newToken)) {
			t.Fatal("plaintext credential persisted")
		}
	}
}

func TestEdgeCredentialRotationAtomicFailures(t *testing.T) {
	for name, fault := range map[string]string{
		"prepare row":         "BEFORE INSERT ON edge_credential_rotations",
		"prepare high water":  "BEFORE UPDATE ON edge_credential_state",
		"prepare receipt":     "BEFORE INSERT ON edge_applied_commands",
		"activate credential": "BEFORE UPDATE ON edge_credentials",
		"activate state":      "BEFORE UPDATE OF activated_at ON edge_credential_rotations",
		"activate receipt":    "BEFORE INSERT ON edge_applied_commands",
	} {
		t.Run(name, func(t *testing.T) {
			s, _, a, c, oldToken, newToken := credentialFixture(t)
			activating := name[:8] == "activate"
			if activating {
				requireCredentialOutcome(t, s, a, c, "applied")
				c = activation(c)
			}
			if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER rotation_failure "+fault+" BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
				t.Fatal(err)
			}
			out, err := s.ApplyCredentialCommand(t.Context(), a, c)
			if !errors.Is(err, ErrStorage) || out.Status != "" {
				t.Fatal("failed commit reported success", out, err)
			}
			current, err := s.ReadEnrollment(t.Context())
			if err != nil || current.CredentialVersion != 1 {
				t.Fatal("partial credential promotion", err)
			}
			var rows, receipts int
			var highest int64
			if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_credential_rotations").Scan(t.Context(), &rows); err != nil {
				t.Fatal(err)
			}
			if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &receipts); err != nil {
				t.Fatal(err)
			}
			if err := s.db.NewRaw("SELECT highest_version FROM edge_credential_state").Scan(t.Context(), &highest); err != nil {
				t.Fatal(err)
			}
			if activating {
				if rows != 1 || receipts != 1 || highest != 2 {
					t.Fatal("partial activation receipt")
				}
			} else if rows != 0 || receipts != 0 || highest != 0 {
				t.Fatal("partial preparation")
			}
			if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER rotation_failure"); err != nil {
				t.Fatal(err)
			}
			requireCredentialOutcome(t, s, a, c, "applied")
			if activating {
				credentialAuth(t, s, newToken, 2, nil)
			} else {
				credentialAuth(t, s, oldToken, 1, &c.OverlapExpiresAt)
			}
		})
	}
}

func TestEdgeCredentialRotationImmutableIdentityAndVersion(t *testing.T) {
	s, _, a, c, _, _ := credentialFixture(t)
	requireCredentialOutcome(t, s, a, c, "applied")
	for name, mutate := range map[string]func(*domain.ProbeCredentialCommand){
		"digest":     func(c *domain.ProbeCredentialCommand) { c.TokenHash[0]++ },
		"wire bytes": func(c *domain.ProbeCredentialCommand) { c.PayloadHash[0]++ },
		"rotation":   func(c *domain.ProbeCredentialCommand) { c.RotationID = uuid.NewString() },
		"version":    func(c *domain.ProbeCredentialCommand) { c.CredentialVersion++ },
		"deadline":   func(c *domain.ProbeCredentialCommand) { c.OverlapExpiresAt = c.OverlapExpiresAt.Add(-time.Second) },
		"expiry":     func(c *domain.ProbeCredentialCommand) { c.ExpiresAt = c.ExpiresAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			other := c
			mutate(&other)
			if _, err := s.ApplyCredentialCommand(t.Context(), a, other); !errors.Is(err, ports.ErrConflict) {
				t.Fatal("changed request reused command identity", err)
			}
		})
	}
	other := c
	other.CommandID = uuid.NewString()
	requireCredentialOutcome(t, s, a, other, "already_applied")
	other.CommandID, other.TokenHash = uuid.NewString(), sha256.Sum256([]byte("another digest"))
	if out := requireCredentialOutcome(t, s, a, other, "rejected"); out.Code != "rotation_conflict" {
		t.Fatal(out.Code)
	}
	other.CommandID, other.RotationID, other.CredentialVersion = uuid.NewString(), uuid.NewString(), 3
	if out := requireCredentialOutcome(t, s, a, other, "rejected"); out.Code != "rotation_in_progress" {
		t.Fatal(out.Code)
	}
	activate := activation(c)
	activate.CredentialVersion = 3
	requireCredentialOutcome(t, s, a, activate, "rejected")
	for _, version := range []int64{1, 2} {
		other.CommandID, other.CredentialVersion = uuid.NewString(), version
		requireCredentialOutcome(t, s, a, other, "rejected")
	}
	tooLong := c
	tooLong.CommandID = uuid.NewString()
	tooLong.OverlapExpiresAt = c.CreatedAt.Add(10*time.Minute + time.Microsecond)
	if _, err := s.ApplyCredentialCommand(t.Context(), a, tooLong); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("unbounded overlap", err)
	}
}

func TestEdgeCredentialRotationExpiryDoesNotBrickOrRevive(t *testing.T) {
	for _, activated := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "activated"}[activated], func(t *testing.T) {
			s, _, a, c, oldToken, newToken := credentialFixture(t)
			requireCredentialOutcome(t, s, a, c, "applied")
			if activated {
				requireCredentialOutcome(t, s, a, activation(c), "applied")
			}
			s.commandNow = func() time.Time { return c.OverlapExpiresAt }
			if activated {
				credentialAuth(t, s, oldToken, 0, nil)
				credentialAuth(t, s, newToken, 2, nil)
			} else {
				credentialAuth(t, s, newToken, 0, nil)
				credentialAuth(t, s, oldToken, 1, nil)
			}
			// Observed retirement is durable, including after a backward clock step.
			s.commandNow = func() time.Time { return c.CreatedAt.Add(time.Minute) }
			if activated {
				credentialAuth(t, s, oldToken, 0, nil)
			} else {
				credentialAuth(t, s, newToken, 0, nil)
				requireCredentialOutcome(t, s, a, activation(c), "rejected")
			}
		})
	}
}

func TestEdgeCredentialRotationExpiredCommandCannotReviveOverlap(t *testing.T) {
	s, _, a, c, _, newToken := credentialFixture(t)
	requireCredentialOutcome(t, s, a, c, "applied")
	s.commandNow = func() time.Time { return c.OverlapExpiresAt }
	activate := activation(c)
	// This receipt is written without any intervening authentication read.
	requireCredentialOutcome(t, s, a, activate, "rejected")
	s.commandNow = func() time.Time { return c.CreatedAt.Add(time.Minute) }
	activate.CommandID = uuid.NewString()
	requireCredentialOutcome(t, s, a, activate, "rejected")
	credentialAuth(t, s, newToken, 0, nil)
}

func TestEdgeCredentialRotationCachedExpiredAdmissionPersistsRetirement(t *testing.T) {
	s, _, a, c, oldToken, _ := credentialFixture(t)
	cached := credentialAuth(t, s, oldToken, 1, nil)
	requireCredentialOutcome(t, s, a, c, "applied")
	requireCredentialOutcome(t, s, a, activation(c), "applied")
	s.commandNow = func() time.Time { return c.OverlapExpiresAt }
	if _, err := s.AcceptCredentialConnection(t.Context(), cached, 2); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("cached expired header admitted", err)
	}
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.ConnectionGeneration != 1 {
		t.Fatal("rejected credential displaced current session", err)
	}
	s.commandNow = func() time.Time { return c.CreatedAt.Add(time.Minute) }
	credentialAuth(t, s, oldToken, 0, nil)
}

func TestEdgeCredentialRotationMigrationPreservesAcknowledgement(t *testing.T) {
	s, _, authority, command := ackFixture(t)
	want, err := s.ApplyAlertAcknowledgement(t.Context(), authority, command)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"migrations/007_credential_rotation.tx.down.sql", "migrations/007_credential_rotation.tx.up.sql"} {
		script, err := migrations.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := tx.ExecContext(ctx, string(script))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ApplyAlertAcknowledgement(t.Context(), authority, command)
	if err != nil || !reflect.DeepEqual(want, got) {
		t.Fatal("credential migration changed retained ACK", err)
	}
	assertAckStorage(t, s, domain.AlertStatusAcked, 2, 3, 1)
	_, token := offer()
	credentialAuth(t, s, token, 1, nil)
}

func TestEdgeCredentialRotationAdmissionFencesPrepare(t *testing.T) {
	for range 8 {
		s, _, a, c, oldToken, _ := credentialFixture(t)
		cached := credentialAuth(t, s, oldToken, 1, nil)
		var wg sync.WaitGroup
		var admission domain.EdgeEnrollment
		var prepared domain.ProbeCommandOutcome
		var admissionErr, prepareErr error
		wg.Go(func() { admission, admissionErr = s.AcceptCredentialConnection(t.Context(), cached, 2) })
		wg.Go(func() { prepared, prepareErr = s.ApplyCredentialCommand(t.Context(), a, c) })
		wg.Wait()
		if admissionErr != nil {
			t.Fatal(admissionErr)
		}
		if prepareErr == nil {
			if prepared.Status != "applied" || admission.ValidUntil == nil || !admission.ValidUntil.Equal(c.OverlapExpiresAt) {
				t.Fatal("prepare won without bounding new admission")
			}
		} else if !errors.Is(prepareErr, ports.ErrConflict) || admission.ValidUntil != nil {
			t.Fatal("admission winner failed to fence prior command", prepareErr)
		}
	}
}

func TestEdgeCredentialRotationDuplicateConcurrency(t *testing.T) {
	s, _, a, c, _, _ := credentialFixture(t)
	results := make(chan domain.ProbeCommandOutcome, 12)
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { out, err := s.ApplyCredentialCommand(t.Context(), a, c); results <- out; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := <-results
	for out := range results {
		if !reflect.DeepEqual(want, out) {
			t.Fatal("concurrent duplicate changed result")
		}
	}
	var count int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_credential_rotations").Scan(t.Context(), &count); err != nil || count != 1 {
		t.Fatal("multiple source effects", err)
	}
}

func TestEdgeCredentialRotationBoundsAndHighWater(t *testing.T) {
	s, _, a, c, _, _ := credentialFixture(t)
	now := s.commandNow()
	if err := s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		for n := range maxCredentialRotations {
			row := edgeCredentialRotation{RotationID: uuid.NewString(), Version: int64(n + 2), TokenHash: c.TokenHash[:], PreviousTokenHash: c.TokenHash[:], PreviousVersion: 1, PreviousIssuedAt: now.Add(-time.Hour).UnixMicro(), PreparedAt: now.Add(-time.Minute).UnixMicro(), OverlapExpiresAt: now.Add(-time.Second).UnixMicro(), RetainUntil: now.Add(time.Hour).UnixMicro(), OverlapClosed: true}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE edge_credential_state SET highest_version = ? WHERE id=1", maxCredentialRotations+1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c.CredentialVersion = maxCredentialRotations + 2
	if _, err := s.ApplyCredentialCommand(t.Context(), a, c); !errors.Is(err, ErrQueueFull) {
		t.Fatal("capacity returned success", err)
	}
	var receipts int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &receipts); err != nil || receipts != 0 {
		t.Fatal("capacity committed receipt", err)
	}
	s.commandNow = func() time.Time { return now.Add(2 * time.Hour) }
	c.CreatedAt, c.ExpiresAt, c.OverlapExpiresAt = s.commandNow(), s.commandNow().Add(time.Hour), s.commandNow().Add(10*time.Minute)
	requireCredentialOutcome(t, s, a, c, "applied")
	var retained int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_credential_rotations").Scan(t.Context(), &retained); err != nil || retained != 2 {
		t.Fatal("safe history cleanup failed", retained, err)
	}
	c.CommandID, c.RotationID, c.CredentialVersion = uuid.NewString(), uuid.NewString(), 2
	requireCredentialOutcome(t, s, a, c, "rejected")
}

func TestEdgeCredentialRotationDowngradeGuard(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "prepared"}[prepared], func(t *testing.T) {
			s, _, a, c, _, _ := credentialFixture(t)
			if prepared {
				requireCredentialOutcome(t, s, a, c, "applied")
			}
			down, err := migrations.ReadFile("migrations/007_credential_rotation.tx.down.sql")
			if err != nil {
				t.Fatal(err)
			}
			err = s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(down)); return err })
			if prepared {
				if err == nil {
					t.Fatal("downgrade discarded credential state")
				}
				requireCredentialOutcome(t, s, a, c, "applied")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
