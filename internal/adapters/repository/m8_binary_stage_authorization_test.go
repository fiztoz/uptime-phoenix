package repository_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type m8StageRepositoryFunc func(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error)

func (f m8StageRepositoryFunc) AdmitBinaryStage(ctx context.Context, r domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
	return f(ctx, r)
}

type m8StageSigner struct {
	inner  ports.ProbeBinaryStageGrantSigner
	calls  atomic.Int32
	before func(context.Context, domain.ProbeBinaryStageAdmission) error
}

func (s *m8StageSigner) PublicKey() [32]byte { return s.inner.PublicKey() }
func (s *m8StageSigner) SignStageAdmission(ctx context.Context, a domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
	s.calls.Add(1)
	if s.before != nil {
		if err := s.before(ctx, a); err != nil {
			return domain.ProbeBinaryStageGrant{}, err
		}
	}
	return s.inner.SignStageAdmission(ctx, a)
}

func m8StageAuthority(t *testing.T, seed byte) *m8StageSigner {
	t.Helper()
	inner, err := provisioning.NewEd25519ProbeStageGrantSigner(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32)))
	if err != nil {
		t.Fatal("construct fixture-only stage authority")
	}
	return &m8StageSigner{inner: inner}
}

func m8StageRequest(signer ports.ProbeBinaryStageGrantSigner) domain.ProbeBinaryStageAdmissionRequest {
	identity := m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1)
	identity.Host.User = "root"
	intent := domain.ProbeBinaryStageIntent{Identity: identity, OwnerID: m8InstallOwnerA, AttemptID: m8InstallAttempt2}
	return domain.ProbeBinaryStageAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}}
}

func m8StageService(t *testing.T, repo ports.ProbeBinaryStageAdmissionRepository, signer ports.ProbeBinaryStageGrantSigner) *services.ProbeBinaryStageAuthorizationService {
	t.Helper()
	svc, err := services.NewProbeBinaryStageAuthorizationService(repo, signer)
	if err != nil {
		t.Fatal("construct authorization service")
	}
	return svc
}

// These are production journal entry points with an explicit joined test report
// for data_directory; no host data-directory existence is claimed by this test.
func m8StagePrepare(t *testing.T, f probeRegistryFixture, r domain.ProbeBinaryStageAdmissionRequest, artifact bool) *services.ProbeInstallJournalService {
	t.Helper()
	svc := m8JournalService(t, f)
	if _, fresh, err := svc.Create(t.Context(), r.Intent.Identity, r.Intent.OwnerID); err != nil || !fresh {
		t.Fatal("create fixture intent")
	}
	if _, err := svc.Begin(t.Context(), r.Intent.Identity.OperationID, r.Intent.OwnerID); err != nil {
		t.Fatal("begin fixture intent")
	}
	if _, fresh, err := svc.ReserveResource(t.Context(), r.Intent.Identity.OperationID, r.Intent.OwnerID, "data_directory", m8InstallAttempt1); err != nil || !fresh {
		t.Fatal("reserve test data directory")
	}
	if _, err := svc.RecordResource(t.Context(), r.Intent.Identity.OperationID, r.Intent.OwnerID, "data_directory", m8InstallAttempt1, m8InstallApplied()); err != nil {
		t.Fatal("record joined test data report")
	}
	if artifact {
		if _, fresh, err := svc.ReserveResource(t.Context(), r.Intent.Identity.OperationID, r.Intent.OwnerID, "artifact_directory", r.Intent.AttemptID); err != nil || !fresh {
			t.Fatal("reserve artifact")
		}
	}
	return svc
}

func m8StageCount(ctx context.Context, db *bun.DB) (int64, error) {
	return db.NewSelect().Table("probe_binary_stage_admissions").Count(ctx)
}

func m8StageRows(t *testing.T, f probeRegistryFixture, want int64) {
	t.Helper()
	count, err := m8StageCount(t.Context(), f.db)
	if err != nil || count != want {
		t.Fatalf("admission row count: got %d want %d", count, want)
	}
}

func m8StageRefused(t *testing.T, grant domain.ProbeBinaryStageGrant, err, want error) {
	t.Helper()
	if grant != (domain.ProbeBinaryStageGrant{}) || !errors.Is(err, want) {
		t.Fatalf("zero grant/fixed refusal missing (match=%v)", errors.Is(err, want))
	}
}

func m8StageGranted(t *testing.T, grant domain.ProbeBinaryStageGrant, err error, r domain.ProbeBinaryStageAdmissionRequest, signer ports.ProbeBinaryStageGrantSigner) {
	t.Helper()
	if err != nil || grant.Request != r || !grant.ValidFor(signer.PublicKey()) {
		t.Fatal("expected exact cryptographically verified admission")
	}
}

type m8StageResult struct {
	grant domain.ProbeBinaryStageGrant
	err   error
}

func m8StageAwait(t *testing.T, ctx context.Context, result <-chan m8StageResult) m8StageResult {
	t.Helper()
	select {
	case r := <-result:
		return r
	case <-ctx.Done():
		t.Fatal("stage authorization did not join")
		return m8StageResult{}
	}
}

func m8StageClock(t *testing.T, ctx context.Context, tx bun.Tx, engine string) time.Time {
	t.Helper()
	query := "SELECT strftime('%Y-%m-%d %H:%M:%f', 'now')"
	if engine == "mariadb" {
		query = "SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f')"
	}
	var value string
	if err := tx.NewRaw(query).Scan(ctx, &value); err != nil {
		t.Fatal("read actual database clock")
	}
	now, err := time.ParseInLocation("2006-01-02 15:04:05.999999", value, time.UTC)
	if err != nil {
		t.Fatal("parse database clock")
	}
	return now
}

func TestM8BinaryStageAuthorization(t *testing.T) {
	cases := []string{"GrantBindsCommittedReservation", "RejectPlannedResource", "RejectUncommittedPredecessor", "IdentityOwnerAttemptMismatch", "CancelBeforeAdmission", "AdmissionBeforeCancel", "ExpiryAfterLock", "CorruptGraphNoGrant", "ConcurrentAdmissionSingleGrant", "ExactReplayNoGrant", "DifferentChallengeNoRearm", "LostAdmissionAckNoSign", "SignFailureConsumesAdmission", "CallerCanceledAfterCommitNoGrant", "IndependentReaderSeesAdmissionBeforeSign", "ColdReopenNoGrant", "ClockRollbackStamp", "DowngradeRetainsAdmission"}
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, name := range cases {
				t.Run(name, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					signer := m8StageAuthority(t, 53)
					r := m8StageRequest(signer)
					store := repository.NewProbeBinaryStageAdmissionStore(f.db)
					svc := m8StageService(t, store, signer)
					ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
					defer cancel()
					if name == "RejectUncommittedPredecessor" {
						m8StageUncommittedPredecessor(t, ctx, f, r, signer)
						return
					}
					if name == "DowngradeRetainsAdmission" {
						if err := runEngineMigration(t, f.db, engine, "085_probe_binary_stage_admissions", "down"); err != nil {
							t.Fatal("empty admission downgrade refused")
						}
						if _, err := m8StageCount(ctx, f.db); err == nil {
							t.Fatal("empty downgrade did not remove table")
						}
						if err := runEngineMigration(t, f.db, engine, "085_probe_binary_stage_admissions", "up"); err != nil {
							t.Fatal("admission re-upgrade failed")
						}
					}
					journal := m8StagePrepare(t, f, r, name != "RejectPlannedResource")
					switch name {
					case "GrantBindsCommittedReservation":
						// No row exists yet: each bad SQL insert must be rejected
						// by its own schema constraint, not a duplicate claim.
						for _, bad := range []struct{ column, value string }{{"owner_id", "invalid"}, {"authority_key_id", string(bytes.Repeat([]byte{'A'}, 64))}, {"nonce_hex", string(bytes.Repeat([]byte{'0'}, 64))}, {"marker_sha256", "bad"}, {"kind", "unit_file"}} {
							values := map[string]string{"owner_id": r.Intent.OwnerID, "authority_key_id": r.Challenge.AuthorityKeyID, "nonce_hex": hex.EncodeToString(r.Challenge.Nonce[:]), "marker_sha256": r.Challenge.MarkerSHA256, "kind": "artifact_directory"}
							values[bad.column] = bad.value
							_, err := f.db.ExecContext(ctx, "INSERT INTO probe_binary_stage_admissions (operation_id, kind, owner_id, attempt_id, authority_key_id, nonce_hex, marker_sha256, admitted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", r.Intent.Identity.OperationID, values["kind"], values["owner_id"], r.Intent.AttemptID, values["authority_key_id"], values["nonce_hex"], values["marker_sha256"], time.Now().UTC().Truncate(time.Microsecond))
							if err == nil {
								t.Fatal("schema accepted noncanonical admission metadata")
							}
						}
						before := m8InstallRawResources(t, f, r.Intent.Identity.OperationID)
						g, err := svc.Authorize(ctx, r)
						m8StageGranted(t, g, err, r, signer)
						m8StageRows(t, f, 1)
						after := m8InstallRawResources(t, f, r.Intent.Identity.OperationID)
						if after[1].State != domain.ProbeInstallResourceInFlight || after[1].FinishedAt != nil || !after[1].StartedAt.Equal(*before[1].StartedAt) {
							t.Fatal("admission fabricated an applied resource")
						}
					case "RejectPlannedResource":
						g, err := svc.Authorize(ctx, r)
						m8StageRefused(t, g, err, ports.ErrConflict)
						m8StageRows(t, f, 0)
						if signer.calls.Load() != 0 {
							t.Fatal("planned artifact signed")
						}
						if _, fresh, err := journal.ReserveResource(ctx, r.Intent.Identity.OperationID, r.Intent.OwnerID, "artifact_directory", r.Intent.AttemptID); err != nil || !fresh {
							t.Fatal("precommit refusal consumed reservation")
						}
						g, err = svc.Authorize(ctx, r)
						m8StageGranted(t, g, err, r, signer)
					case "IdentityOwnerAttemptMismatch":
						for _, change := range []func(*domain.ProbeBinaryStageAdmissionRequest){
							func(r *domain.ProbeBinaryStageAdmissionRequest) { r.Intent.OwnerID = m8InstallOwnerB },
							func(r *domain.ProbeBinaryStageAdmissionRequest) { r.Intent.AttemptID = m8InstallAttempt3 },
							func(r *domain.ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.RequestedBy++ },
							func(r *domain.ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Host.Address = "192.0.2.11:22" },
							func(r *domain.ProbeBinaryStageAdmissionRequest) { r.Intent.Identity.Binary.Version = "1.2.4" },
						} {
							bad := r
							change(&bad)
							bad.Challenge.MarkerSHA256 = bad.Intent.MarkerSHA256()
							g, err := svc.Authorize(ctx, bad)
							m8StageRefused(t, g, err, ports.ErrConflict)
						}
						m8StageRows(t, f, 0)
						if signer.calls.Load() != 0 {
							t.Fatal("foreign authority signed")
						}
					case "CancelBeforeAdmission":
						if _, err := journal.Cancel(ctx, r.Intent.Identity.OperationID); err != nil {
							t.Fatal("persist cancellation")
						}
						g, err := svc.Authorize(ctx, r)
						m8StageRefused(t, g, err, ports.ErrConflict)
						m8StageRows(t, f, 0)
						if signer.calls.Load() != 0 || m8InstallRawOp(t, f, r.Intent.Identity.OperationID).State != domain.ProbeInstallCancelRequested {
							t.Fatal("cancellation fence lost")
						}
					case "AdmissionBeforeCancel":
						g, err := svc.Authorize(ctx, r)
						m8StageGranted(t, g, err, r, signer)
						if _, err := journal.Cancel(ctx, r.Intent.Identity.OperationID); err != nil {
							t.Fatal("persist later cancellation")
						}
						if !g.ValidFor(signer.PublicKey()) || m8InstallRawOp(t, f, r.Intent.Identity.OperationID).State != domain.ProbeInstallCancelRequested {
							t.Fatal("admission semantics changed")
						}
						g, err = svc.Authorize(ctx, r)
						m8StageRefused(t, g, err, ports.ErrConflict)
						m8StageRows(t, f, 1)
					case "ExpiryAfterLock":
						m8StageExpiryAfterLock(t, ctx, f, r, signer)
					case "CorruptGraphNoGrant":
						m8InstallExpire(t, f, r.Intent.Identity.OperationID)
						if _, err := f.db.ExecContext(ctx, "UPDATE probe_install_resources SET reference = ? WHERE operation_id = ? AND kind = ?", "/foreign", r.Intent.Identity.OperationID, "artifact_directory"); err != nil {
							t.Fatal("construct malformed graph")
						}
						before := m8InstallSnapshot(t, f, r.Intent.Identity.OperationID)
						g, err := svc.Authorize(ctx, r)
						m8StageRefused(t, g, err, domain.ErrInternal)
						m8StageRows(t, f, 0)
						if before != m8InstallSnapshot(t, f, r.Intent.Identity.OperationID) || signer.calls.Load() != 0 {
							t.Fatal("corrupt graph was repaired or signed")
						}
					case "ConcurrentAdmissionSingleGrant":
						peer := m8InstallSecondDB(t, f, "")
						other := m8StageService(t, repository.NewProbeBinaryStageAdmissionStore(peer), signer)
						start := make(chan struct{})
						results := make(chan m8StageResult, 2)
						var wg sync.WaitGroup
						for _, instance := range []*services.ProbeBinaryStageAuthorizationService{svc, other} {
							wg.Go(func() { <-start; g, err := instance.Authorize(ctx, r); results <- m8StageResult{g, err} })
						}
						t.Cleanup(func() { cancel(); wg.Wait() })
						close(start)
						success := 0
						for range 2 {
							got := m8StageAwait(t, ctx, results)
							if got.err == nil {
								m8StageGranted(t, got.grant, nil, r, signer)
								success++
							} else {
								m8StageRefused(t, got.grant, got.err, ports.ErrConflict)
							}
						}
						wg.Wait()
						if success != 1 || signer.calls.Load() != 1 {
							t.Fatal("concurrent attempts did not yield exactly one grant")
						}
						m8StageRows(t, f, 1)
					case "ExactReplayNoGrant", "DifferentChallengeNoRearm", "ColdReopenNoGrant":
						g, err := svc.Authorize(ctx, r)
						m8StageGranted(t, g, err, r, signer)
						before := m8InstallSnapshot(t, f, r.Intent.Identity.OperationID)
						if name == "DifferentChallengeNoRearm" {
							r.Challenge.Nonce[1]++
						}
						if name == "ColdReopenNoGrant" {
							svc = m8StageService(t, repository.NewProbeBinaryStageAdmissionStore(m8InstallSecondDB(t, f, "")), m8StageAuthority(t, 53))
						}
						g, err = svc.Authorize(ctx, r)
						m8StageRefused(t, g, err, ports.ErrConflict)
						if before != m8InstallSnapshot(t, f, r.Intent.Identity.OperationID) {
							t.Fatal("replay modified durable history")
						}
						if name == "DifferentChallengeNoRearm" {
							rotated := m8StageAuthority(t, 54)
							r.Challenge.AuthorityKeyID = domain.ProbeBinaryStageAuthorityKeyID(rotated.PublicKey())
							g, err = m8StageService(t, store, rotated).Authorize(ctx, r)
							m8StageRefused(t, g, err, ports.ErrConflict)
							if rotated.calls.Load() != 0 {
								t.Fatal("rotation rearmed consumed attempt")
							}
						}
						m8StageRows(t, f, 1)
					case "LostAdmissionAckNoSign", "CallerCanceledAfterCommitNoGrant":
						peer := m8InstallSecondDB(t, f, "")
						observed := false
						decorated := m8StageRepositoryFunc(func(call context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
							a, err := store.AdmitBinaryStage(call, request)
							if err != nil {
								return domain.ProbeBinaryStageAdmission{}, err
							}
							count, readErr := m8StageCount(call, peer)
							if readErr != nil || count != 1 {
								return domain.ProbeBinaryStageAdmission{}, domain.ErrInternal
							}
							observed = true
							if name == "CallerCanceledAfterCommitNoGrant" {
								cancel()
								return a, nil
							}
							return domain.ProbeBinaryStageAdmission{}, domain.ErrInternal
						})
						g, err := m8StageService(t, decorated, signer).Authorize(ctx, r)
						want := domain.ErrInternal
						if name == "CallerCanceledAfterCommitNoGrant" {
							want = context.Canceled
						}
						m8StageRefused(t, g, err, want)
						if !observed || signer.calls.Load() != 0 {
							t.Fatal("lost/canceled postcommit acknowledgment signed")
						}
						m8StageRows(t, f, 1)
						g, err = m8StageService(t, repository.NewProbeBinaryStageAdmissionStore(peer), signer).Authorize(t.Context(), r)
						m8StageRefused(t, g, err, ports.ErrConflict)
					case "SignFailureConsumesAdmission", "DowngradeRetainsAdmission":
						signer.before = func(context.Context, domain.ProbeBinaryStageAdmission) error {
							return errors.New("private fixture signing diagnostic")
						}
						g, err := svc.Authorize(ctx, r)
						m8StageRefused(t, g, err, domain.ErrInternal)
						m8StageRows(t, f, 1)
						if name == "DowngradeRetainsAdmission" {
							if err := runEngineMigration(t, f.db, engine, "085_probe_binary_stage_admissions", "down"); err == nil {
								t.Fatal("downgrade erased irreversible admission")
							}
							m8StageRows(t, f, 1)
						}
						signer.before = nil
						g, err = svc.Authorize(ctx, r)
						m8StageRefused(t, g, err, ports.ErrConflict)
						if signer.calls.Load() != 1 || m8InstallRawResources(t, f, r.Intent.Identity.OperationID)[1].State != domain.ProbeInstallResourceInFlight {
							t.Fatal("sign failure rearmed or applied a resource")
						}
					case "IndependentReaderSeesAdmissionBeforeSign":
						peer := m8InstallSecondDB(t, f, "")
						observed := false
						signer.before = func(call context.Context, a domain.ProbeBinaryStageAdmission) error {
							count, err := m8StageCount(call, peer)
							if err != nil || count != 1 {
								return domain.ErrInternal
							}
							var nonce, marker, attempt string
							if err := peer.NewRaw("SELECT nonce_hex, marker_sha256, attempt_id FROM probe_binary_stage_admissions WHERE operation_id = ?", r.Intent.Identity.OperationID).Scan(call, &nonce, &marker, &attempt); err != nil {
								return domain.ErrInternal
							}
							if nonce != hex.EncodeToString(r.Challenge.Nonce[:]) || marker != r.Intent.MarkerSHA256() || attempt != r.Intent.AttemptID || a.Request != r {
								return domain.ErrInternal
							}
							var state string
							if err := peer.NewRaw("SELECT state FROM probe_install_resources WHERE operation_id = ? AND ordinal = 1", r.Intent.Identity.OperationID).Scan(call, &state); err != nil || state != domain.ProbeInstallResourceApplied {
								return domain.ErrInternal
							}
							observed = true
							return nil
						}
						g, err := svc.Authorize(ctx, r)
						m8StageGranted(t, g, err, r, signer)
						if !observed {
							t.Fatal("signer did not independently observe committed admission")
						}
					case "ClockRollbackStamp":
						op := m8InstallRawOp(t, f, r.Intent.Identity.OperationID)
						floor := op.DeadlineAt.Add(-time.Minute)
						if _, err := f.db.ExecContext(ctx, "UPDATE probe_install_operations SET updated_at = ? WHERE operation_id = ?", floor, r.Intent.Identity.OperationID); err != nil {
							t.Fatal("set causal floor")
						}
						if _, err := f.db.ExecContext(ctx, "UPDATE probe_install_resources SET started_at = ? WHERE operation_id = ? AND ordinal = 2", floor, r.Intent.Identity.OperationID); err != nil {
							t.Fatal("set resource floor")
						}
						var stamp time.Time
						signer.before = func(_ context.Context, a domain.ProbeBinaryStageAdmission) error { stamp = a.AdmittedAt; return nil }
						g, err := svc.Authorize(ctx, r)
						m8StageGranted(t, g, err, r, signer)
						if !stamp.Equal(floor) || stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 || !m8InstallRawOp(t, f, r.Intent.Identity.OperationID).UpdatedAt.Equal(floor) || !m8InstallRawOp(t, f, r.Intent.Identity.OperationID).DeadlineAt.Equal(op.DeadlineAt) {
							t.Fatal("admission rewound causal stamp or renewed deadline")
						}
					default:
						t.Fatal("unimplemented required stage authorization case")
					}
				})
			}
		})
	}
}

func m8StageUncommittedPredecessor(t *testing.T, ctx context.Context, f probeRegistryFixture, r domain.ProbeBinaryStageAdmissionRequest, signer *m8StageSigner) {
	t.Helper()
	journal := m8JournalService(t, f)
	if _, fresh, err := journal.Create(ctx, r.Intent.Identity, r.Intent.OwnerID); err != nil || !fresh {
		t.Fatal("create predecessor fixture")
	}
	if _, err := journal.Begin(ctx, r.Intent.Identity.OperationID, r.Intent.OwnerID); err != nil {
		t.Fatal("begin predecessor fixture")
	}
	if _, fresh, err := journal.ReserveResource(ctx, r.Intent.Identity.OperationID, r.Intent.OwnerID, "data_directory", m8InstallAttempt1); err != nil || !fresh {
		t.Fatal("reserve predecessor fixture")
	}
	blocker, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin held transaction")
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.ExecContext(ctx, "UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", r.Intent.Identity.OperationID); err != nil {
		t.Fatal("hold operation lock")
	}
	if _, err := blocker.ExecContext(ctx, "UPDATE probe_install_resources SET state = 'applied', finished_at = started_at WHERE operation_id = ? AND ordinal = 1", r.Intent.Identity.OperationID); err != nil {
		t.Fatal("construct uncommitted prerequisite")
	}
	// Present a complete eligible graph inside the held transaction: a dirty
	// reader must not authorize it. The artifact must not remain planned here,
	// otherwise that unrelated refusal can mask missing isolation.
	if _, err := blocker.ExecContext(ctx, "UPDATE probe_install_resources SET state = 'in_flight', attempt_id = ?, started_at = (SELECT updated_at FROM probe_install_operations WHERE operation_id = ?) WHERE operation_id = ? AND ordinal = 2", r.Intent.AttemptID, r.Intent.Identity.OperationID, r.Intent.Identity.OperationID); err != nil {
		t.Fatal("construct uncommitted artifact reservation")
	}
	result, entered, joined, cancel := m8StageBlockedCall(t, ctx, f, r, signer)
	defer func() { cancel(); _ = blocker.Rollback(); <-joined }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("admission never reached operation lock")
	}
	select {
	case <-result:
		t.Fatal("admission bypassed held operation")
	default:
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal("rollback uncommitted prerequisite")
	}
	got := m8StageAwait(t, ctx, result)
	m8StageRefused(t, got.grant, got.err, ports.ErrConflict)
	m8StageRows(t, f, 0)
	resources := m8InstallRawResources(t, f, r.Intent.Identity.OperationID)
	if signer.calls.Load() != 0 || resources[0].State != domain.ProbeInstallResourceInFlight || resources[1].State != domain.ProbeInstallResourcePlanned || resources[1].AttemptID != nil {
		t.Fatal("uncommitted graph escaped rollback")
	}
	// A separate corruption control isolates the prerequisite invariant from
	// the planned-artifact check. Bypassing both graph and predecessor checks
	// must be caught by a real signing/consumption effect, not a leaf name.
	if _, err := f.db.ExecContext(ctx, "UPDATE probe_install_resources SET state = 'in_flight', attempt_id = ?, started_at = (SELECT updated_at FROM probe_install_operations WHERE operation_id = ?) WHERE operation_id = ? AND ordinal = 2", r.Intent.AttemptID, r.Intent.Identity.OperationID, r.Intent.Identity.OperationID); err != nil {
		t.Fatal("construct committed prerequisite corruption control")
	}
	g, err := m8StageService(t, repository.NewProbeBinaryStageAdmissionStore(f.db), signer).Authorize(ctx, r)
	if g != (domain.ProbeBinaryStageGrant{}) || err == nil || signer.calls.Load() != 0 {
		t.Fatal("predecessor refusal must leave zero grant and zero signing")
	}
	m8StageRows(t, f, 0)
	// Remove only the deliberately injected corruption, never an admission.
	if _, err := f.db.ExecContext(ctx, "UPDATE probe_install_resources SET state = 'planned', attempt_id = NULL, started_at = NULL WHERE operation_id = ? AND ordinal = 2", r.Intent.Identity.OperationID); err != nil {
		t.Fatal("restore prerequisite corruption control")
	}
	// The same request must succeed once its prerequisites really commit;
	// refusal above must not leave a consumption tombstone.
	if _, err := journal.RecordResource(ctx, r.Intent.Identity.OperationID, r.Intent.OwnerID, "data_directory", m8InstallAttempt1, m8InstallApplied()); err != nil {
		t.Fatal("commit predecessor positive control")
	}
	if _, fresh, err := journal.ReserveResource(ctx, r.Intent.Identity.OperationID, r.Intent.OwnerID, "artifact_directory", r.Intent.AttemptID); err != nil || !fresh {
		t.Fatal("commit artifact positive control")
	}
	g, err = m8StageService(t, repository.NewProbeBinaryStageAdmissionStore(f.db), signer).Authorize(ctx, r)
	m8StageGranted(t, g, err, r, signer)
	m8StageRows(t, f, 1)
	if signer.calls.Load() != 1 {
		t.Fatal("committed graph did not sign exactly once")
	}
}

func m8StageBlockedCall(t *testing.T, ctx context.Context, f probeRegistryFixture, r domain.ProbeBinaryStageAdmissionRequest, signer *m8StageSigner) (<-chan m8StageResult, <-chan struct{}, <-chan struct{}, context.CancelFunc) {
	t.Helper()
	peer := m8InstallSecondDB(t, f, "")
	hook := &m8JournalLockEntry{entered: make(chan struct{})}
	attachInPlaceQueryHook(peer, hook)
	svc := m8StageService(t, repository.NewProbeBinaryStageAdmissionStore(peer), signer)
	call, cancel := context.WithCancel(ctx)
	result := make(chan m8StageResult, 1)
	joined := make(chan struct{})
	go func() { defer close(joined); g, err := svc.Authorize(call, r); result <- m8StageResult{g, err} }()
	return result, hook.entered, joined, cancel
}

func m8StageExpiryAfterLock(t *testing.T, ctx context.Context, f probeRegistryFixture, r domain.ProbeBinaryStageAdmissionRequest, signer *m8StageSigner) {
	t.Helper()
	seed, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin deadline fixture")
	}
	defer func() { _ = seed.Rollback() }()
	deadline := m8StageClock(t, ctx, seed, f.engine).Add(1200 * time.Millisecond)
	if _, err := seed.ExecContext(ctx, "UPDATE probe_install_operations SET created_at = ?, deadline_at = ? WHERE operation_id = ?", deadline.Add(-domain.ProbeInstallLifetime), deadline, r.Intent.Identity.OperationID); err != nil {
		t.Fatal("set live deadline")
	}
	if err := seed.Commit(); err != nil {
		t.Fatal("commit live deadline")
	}
	blocker, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin authority blocker")
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.ExecContext(ctx, "UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", r.Intent.Identity.OperationID); err != nil {
		t.Fatal("hold authority lock")
	}
	result, entered, joined, cancel := m8StageBlockedCall(t, ctx, f, r, signer)
	defer func() { cancel(); _ = blocker.Rollback(); <-joined }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("admission never reached authority lock")
	}
	if !m8StageClock(t, ctx, blocker, f.engine).Before(deadline) {
		t.Fatal("fixture was not live at lock entry")
	}
	for m8StageClock(t, ctx, blocker, f.engine).Before(deadline) {
		select {
		case <-result:
			t.Fatal("admission bypassed held lock")
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("deadline observation timed out")
		}
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal("release authority lock after expiry")
	}
	got := m8StageAwait(t, ctx, result)
	m8StageRefused(t, got.grant, got.err, ports.ErrConflict)
	m8StageRows(t, f, 0)
	op := m8InstallRawOp(t, f, r.Intent.Identity.OperationID)
	resources := m8InstallRawResources(t, f, r.Intent.Identity.OperationID)
	if op.State != domain.ProbeInstallInterrupted || op.ErrorCode == nil || *op.ErrorCode != "deadline_expired" || resources[1].State != domain.ProbeInstallResourceUnknown || resources[0].State != domain.ProbeInstallResourceApplied || signer.calls.Load() != 0 {
		t.Fatal("expired admission was not durably refused after lock")
	}
}
