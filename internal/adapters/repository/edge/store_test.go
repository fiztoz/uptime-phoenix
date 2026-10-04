package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const testHubID = "4d5e29ba-a1c4-4e39-a0c7-f73b35a2c521"

func testIdentity() domain.EdgeIdentity {
	return domain.EdgeIdentity{ProbeID: "0897874c-a573-457b-8953-893992136ed1", StreamID: "34f542f2-eab7-4c28-a11b-7746fc0a2944", Fingerprint: strings.Repeat("a", 64)}
}

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func offer() (domain.EdgeEnrollment, string) {
	return domain.EdgeEnrollment{HubID: testHubID, ProbeID: testIdentity().ProbeID, EnrollmentID: "c06ba679-30c7-4a3c-95e8-0b3e3f7b546a", CredentialVersion: 1}, "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x82}, 32))
}

func enroll(t *testing.T, s *Store) (string, string) {
	t.Helper()
	svc := services.NewEdgeEnrollmentService(s)
	now := time.Now().UTC()
	token, err := svc.Issue(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	binding, runtimeToken := offer()
	if err := svc.Accept(t.Context(), token, runtimeToken, binding, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptConnectionGeneration(t.Context(), testHubID, 1); err != nil {
		t.Fatal(err)
	}
	return token, runtimeToken
}

func TestEdgeEnrollmentAtomicRestartAndReplay(t *testing.T) {
	s, dir := testStore(t)
	ctx := t.Context()
	svc := services.NewEdgeEnrollmentService(s)
	now := time.Now().UTC()
	token, err := svc.Issue(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	binding, runtimeToken := offer()
	if _, err := s.db.ExecContext(ctx, "CREATE TRIGGER fail_enrollment BEFORE DELETE ON edge_credentials BEGIN SELECT RAISE(ABORT, 'injected secret'); END"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Accept(ctx, token, runtimeToken, binding, now); !errors.Is(err, ErrStorage) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe/missing rollback failure: %v", err)
	}
	identity, err := s.ReadIdentity(ctx)
	if err != nil || identity.HubID != "" {
		t.Fatalf("partial binding: %+v %v", identity, err)
	}
	if _, err := s.ReadEnrollment(ctx); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("partial runtime credential: %v", err)
	}
	if valid, err := svc.AuthenticateEnrollment(ctx, token, now); err != nil || !valid {
		t.Fatalf("token consumed before commit: %v %v", valid, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_enrollment"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Accept(ctx, token, runtimeToken, binding, now); err != nil {
		t.Fatal(err)
	}
	// Simulate loss of the enrollment response and a process restart. The durable
	// runtime token recovers; the consumed enrollment bearer cannot be replayed.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	svc = services.NewEdgeEnrollmentService(reopened)
	got, valid, err := svc.AuthenticateRuntime(ctx, runtimeToken)
	if err != nil || !valid || got.HubID != binding.HubID || got.EnrollmentID != binding.EnrollmentID {
		t.Fatalf("lost-response recovery failed: %+v %v %v", got, valid, err)
	}
	if valid, err := svc.AuthenticateEnrollment(ctx, token, now.Add(time.Second)); err != nil || valid {
		t.Fatalf("replayed enrollment accepted: %v %v", valid, err)
	}
	binding.HubID = testIdentity().StreamID
	if err := svc.Accept(ctx, token, runtimeToken, binding, now.Add(time.Second)); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("rebound to another hub: %v", err)
	}
	if token, err := svc.Issue(ctx, now.Add(time.Second)); !errors.Is(err, ports.ErrConflict) || token != "" {
		t.Fatalf("issued after binding: %q %v", token, err)
	}
	for _, token := range []string{token, runtimeToken + "A", "phx_probe_invalid"} {
		if _, valid, err := svc.AuthenticateRuntime(ctx, token); err != nil || valid {
			t.Fatalf("wrong runtime credential accepted: %v %v", valid, err)
		}
	}
	if err := reopened.AcceptConnectionGeneration(ctx, testHubID, 9); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []int64{9, 8, 1} {
		if err := reopened.AcceptConnectionGeneration(ctx, testHubID, generation); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("old fence accepted: %v", err)
		}
	}
	if err := reopened.AcceptConnectionGeneration(ctx, testIdentity().StreamID, 10); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("foreign hub fence: %v", err)
	}
	// Inspect the SQLite file/WAL while live: only digests are ever bound to SQL.
	for _, name := range []string{"edge.db", "edge.db-wal"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(token)) || bytes.Contains(b, []byte(runtimeToken)) {
			t.Fatal("plaintext token reached SQLite")
		}
	}
}

func TestEdgeEnrollmentExpiryAndReplacement(t *testing.T) {
	s, _ := testStore(t)
	svc := services.NewEdgeEnrollmentService(s)
	now := time.Now().UTC()
	first, err := svc.Issue(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now.Add(-time.Second), now.Add(10 * time.Minute)} {
		if valid, err := svc.AuthenticateEnrollment(t.Context(), first, at); err != nil || valid {
			t.Fatalf("out-of-window token accepted: %v %v", valid, err)
		}
	}
	second, err := svc.Issue(t.Context(), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("local refresh reused token")
	}
	if valid, err := svc.AuthenticateEnrollment(t.Context(), first, now.Add(time.Minute)); err != nil || valid {
		t.Fatalf("replaced token accepted: %v %v", valid, err)
	}
	binding, runtimeToken := offer()
	if err := svc.Accept(t.Context(), second, runtimeToken, binding, now.Add(11*time.Minute)); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expired token committed: %v", err)
	}
}

func protectedConfig(t *testing.T, revision int64) domain.EdgeActiveConfig {
	t.Helper()
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"confidential":"provider-secret-unique-test-value"}`)
	hash := sha256.Sum256(plain)
	metadata := domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: testHubID, ProbeID: testIdentity().ProbeID}, Revision: revision, SchemaVersion: 1, SHA256: hex.EncodeToString(hash[:]), CreatedAt: time.Now().UTC().Truncate(time.Second), EffectiveAt: time.Now().UTC().Truncate(time.Second)}
	ciphertext, err := p.Seal(t.Context(), metadata, plain)
	if err != nil {
		t.Fatal(err)
	}
	return domain.EdgeActiveConfig{Snapshot: domain.ProtectedProbeConfig{ProbeConfigMetadata: metadata, KeyConfirmation: p.KeyHash(testHubID), ProtectedPayload: ciphertext}, AppliedAt: time.Now().UTC(), ConnectionGeneration: 1, Assignments: []domain.EdgeAssignmentIdentity{{MonitorID: 17, Generation: 1, Active: true}}}
}

func TestEdgeConfigAtomicityRestartAndGeneration(t *testing.T) {
	s, dir := testStore(t)
	enroll(t, s)
	ctx := t.Context()
	first := protectedConfig(t, 1)
	if err := s.ActivateConfig(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConfig(ctx, first); err != nil {
		t.Fatalf("same revision retry: %v", err)
	}
	conflict := first
	conflict.Snapshot.SHA256 = strings.Repeat("f", 64)
	if err := s.ActivateConfig(ctx, conflict); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("conflicting hash accepted: %v", err)
	}
	second := protectedConfig(t, 2)
	if _, err := s.db.ExecContext(ctx, "CREATE TRIGGER fail_config BEFORE UPDATE OF config_revision ON edge_identity BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConfig(ctx, second); !errors.Is(err, ErrStorage) {
		t.Fatalf("fault accepted: %v", err)
	}
	current, err := s.ReadActiveConfig(ctx)
	if err != nil || current.Snapshot.Revision != 1 {
		t.Fatalf("partial activation: %+v %v", current, err)
	}
	var count int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_config").Scan(ctx, &count); err != nil || count != 1 {
		t.Fatalf("snapshot escaped rollback: %d %v", count, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_config"); err != nil {
		t.Fatal(err)
	}
	second.Assignments = []domain.EdgeAssignmentIdentity{}
	if err := s.ActivateConfig(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConfig(ctx, second); err != nil {
		t.Fatalf("empty revision retry: %v", err)
	}
	third := protectedConfig(t, 3)
	if err := s.ActivateConfig(ctx, third); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("removed assignment reused generation: %v", err)
	}
	third.Assignments[0].Generation = 2
	if err := s.ActivateConfig(ctx, third); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.ReadActiveConfig(ctx)
	if err != nil || got.Snapshot.Revision != 3 || len(got.Assignments) != 1 || got.Assignments[0].Generation != 2 {
		t.Fatalf("restart lost config: %+v %v", got, err)
	}
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := p.Open(ctx, got.Snapshot.ProbeConfigMetadata, got.Snapshot.ProtectedPayload)
	if err != nil || !bytes.Contains(plain, []byte("provider-secret-unique-test-value")) {
		t.Fatalf("restart decryption: %v", err)
	}
	for _, name := range []string{"edge.db", "edge.db-wal"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("provider-secret-unique-test-value")) {
			t.Fatal("plaintext config reached SQLite")
		}
	}
}

func TestEdgeStoreIsolationDurabilityAndFileGuards(t *testing.T) {
	s, dir := testStore(t)
	for query, want := range map[string]string{"PRAGMA journal_mode": "wal", "PRAGMA synchronous": "2", "PRAGMA foreign_keys": "1", "PRAGMA busy_timeout": "5000"} {
		var got string
		if err := s.db.NewRaw(query).Scan(t.Context(), &got); err != nil || got != want {
			t.Fatalf("%s = %q, want %q: %v", query, got, want, err)
		}
	}
	for _, table := range []string{"users", "monitors", "heartbeats"} {
		var count int
		if err := s.db.NewRaw("SELECT COUNT(*) FROM sqlite_master WHERE name = ?", table).Scan(t.Context(), &count); err != nil || count != 0 {
			t.Fatalf("hub table %s in edge schema: %d %v", table, count, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wrong := testIdentity()
	wrong.StreamID = testHubID
	if db, err := Open(t.Context(), dir, wrong); !errors.Is(err, ports.ErrConflict) {
		if db != nil {
			_ = db.Close()
		}
		t.Fatalf("identity mismatch: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "edge.db"), 0644); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(t.Context(), dir, testIdentity()); err == nil {
		_ = db.Close()
		t.Fatal("world-readable DB accepted")
	}
	if err := os.Chmod(filepath.Join(dir, "edge.db"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "edge.db"), filepath.Join(dir, "saved.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("saved.db", filepath.Join(dir, "edge.db")); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(t.Context(), dir, testIdentity()); err == nil {
		_ = db.Close()
		t.Fatal("symlink DB accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(canceled, dir, testIdentity()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
