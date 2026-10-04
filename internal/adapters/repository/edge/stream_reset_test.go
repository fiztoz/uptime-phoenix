package edge

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func streamResetFixture(t *testing.T) (*Store, string, *auth.ProbeConfigProtector, domain.ProbeStreamResetPlan) {
	t.Helper()
	s, dir := testStore(t)
	enroll(t, s)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitEdgeCheck(t.Context(), checkRecord()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenForStreamReset(t.Context(), dir, testIdentity(), WithStreamResetProtection(p))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	binding, _ := offer()
	plan := domain.ProbeStreamResetPlan{ResetID: uuid.NewString(), HubID: testHubID, ProbeID: testIdentity().ProbeID, EnrollmentID: binding.EnrollmentID, PreviousStreamID: testIdentity().StreamID, StreamID: uuid.NewString(), Fingerprint: testIdentity().Fingerprint, CredentialVersion: 1, CertificateVersion: 1, HubCommittedSeq: 5, ConnectionGeneration: 10, PreparedAt: time.Now().UTC().Truncate(time.Microsecond)}
	return s, dir, p, plan
}

func immutableResetDB(t *testing.T, path string) *bun.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	b := bun.NewDB(db, sqlitedialect.New())
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func assertResetSource(t *testing.T, s *Store, stream string, seq int64) {
	t.Helper()
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.StreamID != stream || i.LastCreatedSeq != seq {
		t.Fatalf("source changed unexpectedly: %+v %v", i, err)
	}
}

func TestEdgeStreamResetArchivesWALAndReopensCurrentEpoch(t *testing.T) {
	s, dir, protector, plan := streamResetFixture(t)
	pending, err := s.reserveStreamReset(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// Read only the main file, deliberately ignoring WAL. The reservation has
	// not been checkpointed; a filesystem copy of edge.db would lose it.
	main := immutableResetDB(t, filepath.Join(dir, "edge.db"))
	var n int
	if err := main.NewRaw("SELECT COUNT(*) FROM edge_stream_resets").Scan(t.Context(), &n); err != nil || n != 0 {
		t.Fatalf("fixture does not exercise WAL-only evidence: %d %v", n, err)
	}
	archived, err := s.archiveForStreamReset(t.Context(), pending)
	if err != nil {
		t.Fatal("archive", err)
	}
	if archived.ArchiveBytes <= 0 || len(archived.ArchiveSHA256) != 64 {
		t.Fatal("missing archive identity")
	}
	old := immutableResetDB(t, filepath.Join(dir, resetArchiveDir, plan.ResetID, "edge.db"))
	for table, want := range map[string]int{"edge_stream_resets": 1, "edge_telemetry_outbox": 2, "edge_alerts": 1, "edge_delivery_outbox": 1, "edge_regional_state": 1} {
		if err := old.NewRaw("SELECT COUNT(*) FROM "+table).Scan(t.Context(), &n); err != nil || n != want {
			t.Fatalf("archive lost %s: %d %v", table, n, err)
		}
	}
	before, err := os.ReadFile(filepath.Join(dir, resetArchiveDir, plan.ResetID, "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ResetStream(t.Context(), plan)
	if err != nil || result.State != "applied" {
		t.Fatal("reset", result, err)
	}
	if result.Source.LastCreatedSeq != 2 || result.Plan.HubCommittedSeq != 5 {
		t.Fatal("restored source bounds were fabricated", result)
	}
	assertResetSource(t, s, plan.StreamID, 0)
	for _, table := range []string{"edge_telemetry_outbox", "edge_alerts", "edge_delivery_outbox", "edge_regional_state"} {
		if err := s.db.NewRaw("SELECT COUNT(*) FROM "+table).Scan(t.Context(), &n); err != nil || n != 0 {
			t.Fatalf("new epoch kept old %s: %d %v", table, n, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if wrong, err := Open(t.Context(), dir, testIdentity()); err == nil {
		_ = wrong.Close()
		t.Fatal("reset journal opened without key")
	}
	current, err := Open(t.Context(), dir, testIdentity(), WithStreamResetProtection(protector), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal("restart", err)
	}
	assertResetSource(t, current, plan.StreamID, 0)
	newRecord := checkRecord()
	newRecord.Observation.StreamID = plan.StreamID
	newRecord.Incident.SourceAlertID = uuid.NewString()
	newRecord.DeliveryIntents[0].SourceAlertID = newRecord.Incident.SourceAlertID
	newRecord.DeliveryIntents[0].DeliveryID = uuid.NewString()
	if observed, err := current.CommitEdgeCheck(t.Context(), newRecord); err != nil || observed.Seq != 1 {
		t.Fatalf("new sequence one: %+v %v", observed, err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	retry, err := OpenForStreamReset(t.Context(), dir, testIdentity(), WithStreamResetProtection(protector))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = retry.Close() }()
	again, err := retry.ResetStream(t.Context(), plan)
	if err != nil || again != result {
		t.Fatalf("original receipt changed: %+v %v", again, err)
	}
	assertResetSource(t, retry, plan.StreamID, 2)
	after, err := os.ReadFile(filepath.Join(dir, resetArchiveDir, plan.ResetID, "edge.db"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("published archive was modified", err)
	}
}

func TestEdgeStreamResetPublicationCrashFencesRuntimeAndExactRetry(t *testing.T) {
	s, dir, p, plan := streamResetFixture(t)
	pending, err := s.reserveStreamReset(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.archiveForStreamReset(t.Context(), pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime, err := Open(t.Context(), dir, testIdentity(), WithStreamResetProtection(p)); !errors.Is(err, ports.ErrConflict) {
		if runtime != nil {
			_ = runtime.Close()
		}
		t.Fatalf("pending reset permitted restart: %v", err)
	}
	retry, err := OpenForStreamReset(t.Context(), dir, testIdentity(), WithStreamResetProtection(p))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = retry.Close() }()
	assertResetSource(t, retry, plan.PreviousStreamID, 2)
	changed := plan
	changed.HubCommittedSeq++
	if _, err := retry.ResetStream(t.Context(), changed); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("changed retry", err)
	}
	result, err := retry.ResetStream(t.Context(), plan)
	if err != nil || result.ArchiveSHA256 != archive.ArchiveSHA256 {
		t.Fatal("publication recovery", err)
	}
}

func TestEdgeStreamResetStorageFailuresPreserveEvidence(t *testing.T) {
	for _, failure := range []string{"backup", "manifest_sync", "database_sync", "stage_sync", "publish_sync", "late_commit"} {
		t.Run(failure, func(t *testing.T) {
			s, _, _, plan := streamResetFixture(t)
			s.archiveIO.backup = func(ctx context.Context, path string) error {
				if failure == "backup" {
					return errors.New("private device failure")
				}
				return s.backupResetDatabase(ctx, path)
			}
			s.archiveIO.sync = func(f *os.File) error {
				name := f.Name()
				if failure == "manifest_sync" && strings.HasSuffix(name, "manifest.json") || failure == "database_sync" && strings.HasSuffix(name, "edge.db") || failure == "stage_sync" && strings.Contains(name, ".pending") && filepath.Base(name) == "." {
					return errors.New("private fsync failure")
				}
				if failure == "publish_sync" && strings.HasSuffix(name, resetArchiveDir+string(filepath.Separator)+".") {
					if _, err := os.Stat(filepath.Join(s.dataDir, resetArchiveDir, plan.ResetID)); err == nil {
						return errors.New("private directory sync failure")
					}
				}
				return f.Sync()
			}
			if failure == "late_commit" {
				if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_reset BEFORE UPDATE OF state ON edge_stream_resets BEGIN SELECT RAISE(ABORT, 'private reset failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ResetStream(t.Context(), plan); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("failure escaped or was exposed: %v", err)
			}
			assertResetSource(t, s, plan.PreviousStreamID, 2)
			var n int
			if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox").Scan(t.Context(), &n); err != nil || n != 1 {
				t.Fatal("delivery evidence lost", n, err)
			}
			s.archiveIO = resetArchiveIO{}
			if failure == "late_commit" {
				if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER fail_reset"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ResetStream(t.Context(), plan); err != nil {
				t.Fatal("retry", err)
			}
		})
	}
}

func TestEdgeStreamResetTamperAndSymlinksFailClosed(t *testing.T) {
	for _, damage := range []string{"archive_bytes", "archive_symlink", "manifest_proof", "manifest_symlink", "directory_symlink", "wrong_key", "journal_proof"} {
		t.Run(damage, func(t *testing.T) {
			s, dir, _, plan := streamResetFixture(t)
			pending, err := s.reserveStreamReset(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.archiveForStreamReset(t.Context(), pending); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(dir, resetArchiveDir, plan.ResetID)
			switch damage {
			case "archive_bytes":
				f, err := os.OpenFile(filepath.Join(base, "edge.db"), os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteAt([]byte{0xff}, 100)
				_ = f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "archive_symlink", "manifest_symlink":
				name := "edge.db"
				if damage == "manifest_symlink" {
					name = "manifest.json"
				}
				path := filepath.Join(base, name)
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			case "manifest_proof":
				path := filepath.Join(base, "manifest.json")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				b = bytes.Replace(b, []byte(`"proof":"`), []byte(`"proof":"A`), 1)
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory_symlink":
				if err := os.Rename(base, base+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(base+".saved", base); err != nil {
					t.Fatal(err)
				}
			case "wrong_key":
				wrong, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x21}, 32))
				if err != nil {
					t.Fatal(err)
				}
				s.resetProof = wrong
			case "journal_proof":
				if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_stream_resets SET proof = zeroblob(29)"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ResetStream(t.Context(), plan); err == nil {
				t.Fatal("tampered recovery accepted")
			}
			assertResetSource(t, s, plan.PreviousStreamID, 2)
		})
	}
}

func TestEdgeStreamResetExclusiveAndNormalWriteFences(t *testing.T) {
	s, dir, _, plan := streamResetFixture(t)
	if err := s.AcceptConnectionGeneration(t.Context(), testHubID, 99); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("recovery allowed runtime mutation", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if other, err := Open(ctx, dir, testIdentity()); err == nil {
		_ = other.Close()
		t.Fatal("second DB owner acquired recovery store")
	}
	if _, err := s.ResetStream(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeStreamResetMigrationRefusesProvenanceLoss(t *testing.T) {
	s, _, _, plan := streamResetFixture(t)
	for _, direction := range []string{"down", "up"} {
		script, err := migrations.ReadFile("migrations/009_stream_reset.tx." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(t.Context(), string(script)); err != nil {
			t.Fatal(direction, err)
		}
	}
	if _, err := s.ResetStream(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	down, err := migrations.ReadFile("migrations/009_stream_reset.tx.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	err = s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(down)); return err })
	if err == nil {
		t.Fatal("downgrade erased epoch provenance")
	}
	assertResetSource(t, s, plan.StreamID, 0)
}

func TestEdgeStreamResetRebindsActiveCertificateAtomically(t *testing.T) {
	s, dir, authority, command, protector := certificateFixture(t)
	prepared := certificateResult(t, s, authority, command, "applied")
	certificateResult(t, s, authority, certificateActivation(command, prepared), "applied")
	state, err := s.ReadActiveCertificate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	original := *state.Certificate
	before, err := protector.OpenCertificate(t.Context(), original.EdgeCertificateMetadata, original.ProtectedPEM)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(before)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	material, err := probe.NewEdgeCertificateMaterial(protector)
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenForStreamReset(t.Context(), dir, testIdentity(), WithStreamResetProtection(protector), WithCertificateMaterial(material))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	binding, _ := offer()
	plan := domain.ProbeStreamResetPlan{ResetID: uuid.NewString(), HubID: testHubID, ProbeID: testIdentity().ProbeID, EnrollmentID: binding.EnrollmentID, PreviousStreamID: testIdentity().StreamID, StreamID: uuid.NewString(), Fingerprint: prepared.CertificateFingerprint, CredentialVersion: 1, CertificateVersion: 2, ConnectionGeneration: 10, PreparedAt: command.CreatedAt}
	// Overlap is an unresolved identity transition until its original deadline.
	s.commandNow = func() time.Time { return command.CreatedAt }
	if _, err := s.ResetStream(t.Context(), plan); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("overlapping rotation allowed", err)
	}
	s.commandNow = func() time.Time { return command.ExpiresAt.Add(time.Hour) }
	if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_reset BEFORE UPDATE OF state ON edge_stream_resets BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetStream(t.Context(), plan); err == nil {
		t.Fatal("late write failure accepted")
	}
	var row edgeCertificateRotation
	if err := s.db.NewSelect().Model(&row).Where("version = 2").Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if row.StreamID != original.StreamID || !bytes.Equal(row.ProtectedPEM, original.ProtectedPEM) {
		t.Fatal("reseal escaped rollback")
	}
	if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER fail_reset"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetStream(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if err := s.db.NewSelect().Model(&row).Where("version = 2").Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	rebound := row.protected()
	after, err := protector.OpenCertificate(t.Context(), rebound.EdgeCertificateMetadata, rebound.ProtectedPEM)
	if err != nil || !bytes.Equal(before, after) || rebound.Fingerprint != original.Fingerprint || rebound.StreamID != plan.StreamID {
		t.Fatal("key or identity changed", err)
	}
	clear(after)
	if _, err := protector.OpenCertificate(t.Context(), original.EdgeCertificateMetadata, rebound.ProtectedPEM); err == nil {
		t.Fatal("new ciphertext authenticated under old epoch")
	}
	if _, err := probe.OpenEdgeCertificate(t.Context(), protector, rebound, s.commandNow()); err != nil {
		t.Fatal("TLS material invalid", err)
	}
	var commands int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &commands); err != nil || commands != 2 {
		t.Fatal("old command receipts lost", commands, err)
	}
	state, err = readCertificateState(t.Context(), s.db)
	if err != nil || state.ActiveVersion != 2 || state.HighestVersion != 2 {
		t.Fatal("certificate high-water reset", state, err)
	}
}

func TestEdgeStreamResetChainedProvenanceAndRetiredEpochs(t *testing.T) {
	s, dir, p, first := streamResetFixture(t)
	if _, err := s.ResetStream(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ResetID, second.PreviousStreamID, second.StreamID = uuid.NewString(), first.StreamID, uuid.NewString()
	second.ConnectionGeneration++
	retired := second
	retired.StreamID = first.PreviousStreamID
	if _, err := s.ResetStream(t.Context(), retired); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("retired initial epoch reused", err)
	}
	if _, err := s.ResetStream(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := Open(t.Context(), dir, testIdentity(), WithStreamResetProtection(p))
	if err != nil {
		t.Fatal("two-epoch chain", err)
	}
	defer func() { _ = current.Close() }()
	assertResetSource(t, current, second.StreamID, 0)
	if _, err := current.db.ExecContext(t.Context(), "UPDATE edge_identity SET connection_generation = 0"); err != nil {
		t.Fatal(err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	if bad, err := Open(t.Context(), dir, testIdentity(), WithStreamResetProtection(p)); err == nil {
		_ = bad.Close()
		t.Fatal("journal accepted counter rollback below reset fence")
	}
}

func TestEdgeStreamResetArchiveQuotaAndAbandonedStaging(t *testing.T) {
	for _, mode := range []string{"count", "total_bytes", "own_staging", "foreign_staging"} {
		t.Run(mode, func(t *testing.T) {
			s, dir, _, plan := streamResetFixture(t)
			root, err := s.openArchiveRoot()
			if err != nil {
				t.Fatal(err)
			}
			_ = root.Close()
			if strings.Contains(mode, "staging") {
				id := plan.ResetID
				if mode == "foreign_staging" {
					id = uuid.NewString()
				}
				staging := filepath.Join(dir, resetArchiveDir, "."+id+".pending")
				if err := os.Mkdir(staging, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(staging, "edge.db"), []byte("partial unpublished backup"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				count, size := maxEdgeStreamResets, int64(512)
				if mode == "total_bytes" {
					count, size = 4, maxResetArchiveBytes
				}
				for range count {
					path := filepath.Join(dir, resetArchiveDir, uuid.NewString())
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					f, err := os.OpenFile(filepath.Join(path, "edge.db"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					err = f.Truncate(size)
					_ = f.Close()
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "manifest.json"), []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err = s.ResetStream(t.Context(), plan)
			if mode == "own_staging" {
				if err != nil {
					t.Fatal("exact retry did not remove own abandoned staging", err)
				}
				return
			}
			if err == nil {
				t.Fatal("quota or foreign staging ignored")
			}
			assertResetSource(t, s, plan.PreviousStreamID, 2)
		})
	}
}

func TestEdgeStreamResetRetrySyncsPreviouslyPublishedArchiveBeforeCommit(t *testing.T) {
	s, _, _, plan := streamResetFixture(t)
	pending, err := s.reserveStreamReset(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.archiveForStreamReset(t.Context(), pending); err != nil {
		t.Fatal(err)
	}
	// Publication can be visible after rename even if its directory fsync failed.
	// The next attempt must establish durability again before deleting live rows.
	var syncedParent bool
	s.archiveIO.sync = func(f *os.File) error {
		if strings.HasSuffix(f.Name(), resetArchiveDir+string(filepath.Separator)+".") {
			syncedParent = true
			return errors.New("still cannot sync published directory")
		}
		return f.Sync()
	}
	if _, err := s.ResetStream(t.Context(), plan); err == nil {
		t.Fatal("retry committed without syncing published archive directory")
	}
	if !syncedParent {
		t.Fatal("retry did not attempt archive directory sync")
	}
	assertResetSource(t, s, plan.PreviousStreamID, 2)
}
