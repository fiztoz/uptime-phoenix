package repository_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	m8PreflightOp1     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	m8PreflightOp2     = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	m8PreflightOp3     = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	m8PreflightOp4     = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	m8PreflightOp5     = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	m8PreflightOp6     = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	m8PreflightOwnerA  = "11111111-2222-4333-8444-555555555555"
	m8PreflightOwnerB  = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	m8PreflightActorID = int64(4242)
)

// m8PreflightFingerprint builds a canonical OpenSSH SHA256 fingerprint so the
// fixtures exercise the real domain validator instead of bypassing it.
func m8PreflightFingerprint(seed byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

func m8PreflightIdentity(opID string) domain.ProbePreflightIdentity {
	return domain.ProbePreflightIdentity{
		OperationID:          opID,
		Host:                 domain.ProbeSSHHost{Address: "192.0.2.10:22", User: "phoenix"},
		HostKeyFingerprint:   m8PreflightFingerprint(1),
		ClientKeyFingerprint: m8PreflightFingerprint(2),
		RequestedBy:          m8PreflightActorID,
	}
}

func m8PreflightPassed() domain.ProbePreflightOutcome {
	return domain.ProbePreflightOutcome{
		State: domain.ProbePreflightPassed,
		Result: &domain.ProbeHostPreflight{
			OS: "linux", Architecture: "amd64", DockerCLI: true, Systemd: false, Privilege: "sudo",
		},
	}
}

func m8PreflightFailed() domain.ProbePreflightOutcome {
	return domain.ProbePreflightOutcome{State: domain.ProbePreflightFailed, ErrorCode: "ssh_connect"}
}

func m8PreflightCanceled() domain.ProbePreflightOutcome {
	return domain.ProbePreflightOutcome{State: domain.ProbePreflightCanceled, ErrorCode: "canceled"}
}

func m8PreflightInterrupted() domain.ProbePreflightOutcome {
	return domain.ProbePreflightOutcome{State: domain.ProbePreflightInterrupted, ErrorCode: "interrupted"}
}

// m8PreflightExpire backdates created_at and deadline_at together so the row
// is hard-expired relative to the DB clock while remaining schema-valid.
func m8PreflightExpire(t *testing.T, f probeRegistryFixture, opID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := f.db.ExecContext(t.Context(),
		"UPDATE probe_host_preflights SET created_at = ?, deadline_at = ? WHERE operation_id = ?",
		past, past.Add(domain.ProbePreflightLifetime), opID); err != nil {
		t.Fatalf("expire preflight %s: %v", opID, err)
	}
}

func m8PreflightCount(t *testing.T, db *bun.DB) int {
	t.Helper()
	var n int
	if err := db.NewRaw("SELECT COUNT(*) FROM probe_host_preflights").Scan(t.Context(), &n); err != nil {
		t.Fatalf("count preflights: %v", err)
	}
	return n
}

// TestM8PreflightOperationContract proves the durable M8 asynchronous SSH
// preflight storage slice through the real repository production fixtures on
// both engines: commit-before-return, exact immutable retries, DB-clock
// owner/expiry fencing after the row lock, cancellation intent versus joined
// outcomes, cold expiry interruption, corruption refusal, rollback, guarded
// downgrade and immutable terminal history. MariaDB requires TEST_MARIADB_DSN;
// main owns that disposable-engine execution.
func TestM8PreflightOperationContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			store := func(f probeRegistryFixture) *repository.ProbePreflightStore {
				return repository.NewProbePreflightStore(f.db)
			}
			t.Run("CreateCommitAndExactRetryIdentity", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				id := m8PreflightIdentity(m8PreflightOp1)
				op, created, err := store(f).CreatePreflight(ctx, id, m8PreflightOwnerA)
				if err != nil || !created {
					t.Fatalf("create: created=%v err=%v", created, err)
				}
				if op.State != domain.ProbePreflightQueued || op.OwnerID != m8PreflightOwnerA ||
					!op.DeadlineAt.Equal(op.CreatedAt.Add(domain.ProbePreflightLifetime)) {
					t.Fatalf("create shape: %+v", op)
				}
				if op.CreatedAt.Location() != time.UTC || op.UpdatedAt.Location() != time.UTC || op.DeadlineAt.Location() != time.UTC {
					t.Fatalf("create timestamps must cross the boundary in UTC: %+v", op)
				}
				// Commit-before-return: a cold read on the same store sees the row.
				seen, err := store(f).ObservePreflight(ctx, m8PreflightOp1)
				if err != nil || !seen.CreatedAt.Equal(op.CreatedAt) || seen.State != domain.ProbePreflightQueued {
					t.Fatalf("committed read: %+v err=%v", seen, err)
				}
				// Exact retry returns the original owner and deadline, created=false,
				// even when the retrying process presents a different owner UUID.
				again, created, err := store(f).CreatePreflight(ctx, id, m8PreflightOwnerB)
				if err != nil || created {
					t.Fatalf("exact retry: created=%v err=%v", created, err)
				}
				if again.OwnerID != m8PreflightOwnerA || !again.DeadlineAt.Equal(op.DeadlineAt) || !again.CreatedAt.Equal(op.CreatedAt) {
					t.Fatalf("exact retry changed original identity: %+v", again)
				}
				// Every immutable identity member is bound: changed selector,
				// actor or public-key fingerprint conflicts and never overwrites.
				for name, changed := range map[string]domain.ProbePreflightIdentity{
					"address": func() domain.ProbePreflightIdentity {
						c := id
						c.Host.Address = "192.0.2.11:22"
						return c
					}(),
					"user": func() domain.ProbePreflightIdentity {
						c := id
						c.Host.User = "other"
						return c
					}(),
					"host_key": func() domain.ProbePreflightIdentity {
						c := id
						c.HostKeyFingerprint = m8PreflightFingerprint(3)
						return c
					}(),
					"client_key": func() domain.ProbePreflightIdentity {
						c := id
						c.ClientKeyFingerprint = m8PreflightFingerprint(4)
						return c
					}(),
					"actor": func() domain.ProbePreflightIdentity {
						c := id
						c.RequestedBy = id.RequestedBy + 1
						return c
					}(),
				} {
					if _, _, err := store(f).CreatePreflight(ctx, changed, m8PreflightOwnerA); !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("changed %s must conflict, got %v", name, err)
					}
				}
				stored, err := store(f).ObservePreflight(ctx, m8PreflightOp1)
				if err != nil || !preflightOpsEqual(stored, op) {
					t.Fatalf("conflicting retries mutated the receipt: %+v err=%v", stored, err)
				}
				if _, _, err := store(f).CreatePreflight(ctx,
					domain.ProbePreflightIdentity{OperationID: m8PreflightOp1, Host: id.Host,
						HostKeyFingerprint: "SHA256:not-canonical", ClientKeyFingerprint: id.ClientKeyFingerprint,
						RequestedBy: id.RequestedBy}, m8PreflightOwnerA); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid fingerprint must fail validation, got %v", err)
				}
			})
			t.Run("BeginOwnerExpiryFences", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				id := m8PreflightIdentity(m8PreflightOp2)
				if _, _, err := store(f).CreatePreflight(ctx, id, m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp2, m8PreflightOwnerB); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("wrong owner begin must conflict, got %v", err)
				}
				running, err := store(f).BeginPreflight(ctx, m8PreflightOp2, m8PreflightOwnerA)
				if err != nil || running.State != domain.ProbePreflightRunning {
					t.Fatalf("begin: %+v err=%v", running, err)
				}
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp2, m8PreflightOwnerA); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("double begin must conflict, got %v", err)
				}
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp3, m8PreflightOwnerA); !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("missing begin must be not-found, got %v", err)
				}
				// An expired queued row never begins: the DB clock fences it.
				if _, _, err := store(f).CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp4), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				m8PreflightExpire(t, f, m8PreflightOp4)
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp4, m8PreflightOwnerA); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("expired begin must conflict, got %v", err)
				}
				cold, err := store(f).ObservePreflight(ctx, m8PreflightOp4)
				if err != nil || cold.State != domain.ProbePreflightInterrupted || cold.ErrorCode != "interrupted" {
					t.Fatalf("cold observe must reconcile to interrupted: %+v err=%v", cold, err)
				}
			})
			t.Run("FinishStateExpiryAndIdempotentRetry", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				id := m8PreflightIdentity(m8PreflightOp1)
				if _, _, err := store(f).CreatePreflight(ctx, id, m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				// Passed requires running: queued is refused and writes nothing.
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA, m8PreflightPassed()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("finish from queued must conflict, got %v", err)
				}
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerB, m8PreflightPassed()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("wrong owner finish must conflict, got %v", err)
				}
				finished, err := store(f).FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA, m8PreflightPassed())
				if err != nil || finished.State != domain.ProbePreflightPassed || finished.Result == nil ||
					finished.Result.OS != "linux" || finished.Result.Privilege != "sudo" {
					t.Fatalf("finish passed: %+v err=%v", finished, err)
				}
				// Exact same-owner terminal retry is idempotent and immutable.
				retry, err := store(f).FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA, m8PreflightPassed())
				if err != nil || !preflightOpsEqual(retry, finished) {
					t.Fatalf("idempotent terminal retry: %+v err=%v", retry, err)
				}
				// A different terminal outcome never replaces historical evidence.
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA, m8PreflightFailed()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("terminal replacement must conflict, got %v", err)
				}
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerB, m8PreflightPassed()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("terminal retry by other owner must conflict, got %v", err)
				}
				// Invalid outcomes fail closed before touching the row.
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA,
					domain.ProbePreflightOutcome{State: domain.ProbePreflightFailed, ErrorCode: "made_up_code"}); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("non-allowlisted error code must fail validation, got %v", err)
				}
				// Expired running work: passed/failed finish conflicts, then the
				// honest cold reconcile is interrupted, which the joined owner
				// may also record idempotently.
				if _, _, err := store(f).CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp2), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp2, m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				m8PreflightExpire(t, f, m8PreflightOp2)
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp2, m8PreflightOwnerA, m8PreflightPassed()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("expired passed finish must conflict, got %v", err)
				}
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp2, m8PreflightOwnerA, m8PreflightFailed()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("expired failed finish must conflict, got %v", err)
				}
				interrupted, err := store(f).ObservePreflight(ctx, m8PreflightOp2)
				if err != nil || interrupted.State != domain.ProbePreflightInterrupted {
					t.Fatalf("expired reconcile: %+v err=%v", interrupted, err)
				}
				joined, err := store(f).FinishPreflight(ctx, m8PreflightOp2, m8PreflightOwnerA, m8PreflightInterrupted())
				if err != nil || joined.State != domain.ProbePreflightInterrupted {
					t.Fatalf("owner interrupted retry: %+v err=%v", joined, err)
				}
				// An expired queued row the owner joins on shutdown may record
				// honest interruption directly, without a prior cold Observe.
				if _, _, err := store(f).CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp5), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				m8PreflightExpire(t, f, m8PreflightOp5)
				byOwner, err := store(f).FinishPreflight(ctx, m8PreflightOp5, m8PreflightOwnerA, m8PreflightInterrupted())
				if err != nil || byOwner.State != domain.ProbePreflightInterrupted {
					t.Fatalf("owner shutdown interruption: %+v err=%v", byOwner, err)
				}
			})
			t.Run("CancellationIntentVersusJoinedOutcome", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				if _, _, err := store(f).CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp3), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				// Cancellation persists intent on any replica before any join.
				intent, err := store(f).RequestPreflightCancellation(ctx, m8PreflightOp3)
				if err != nil || intent.State != domain.ProbePreflightCancelRequested {
					t.Fatalf("cancel intent: %+v err=%v", intent, err)
				}
				again, err := store(f).RequestPreflightCancellation(ctx, m8PreflightOp3)
				if err != nil || again.State != domain.ProbePreflightCancelRequested {
					t.Fatalf("idempotent cancel intent: %+v err=%v", again, err)
				}
				// Passed evidence must not overwrite pending cancellation.
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp3, m8PreflightOwnerA); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("begin after cancel intent must conflict, got %v", err)
				}
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp3, m8PreflightOwnerA, m8PreflightPassed()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("passed finish after cancel intent must conflict, got %v", err)
				}
				// Only the executor's joined return may persist canceled.
				canceled, err := store(f).FinishPreflight(ctx, m8PreflightOp3, m8PreflightOwnerA, m8PreflightCanceled())
				if err != nil || canceled.State != domain.ProbePreflightCanceled || canceled.ErrorCode != "canceled" {
					t.Fatalf("joined cancel completion: %+v err=%v", canceled, err)
				}
				// A completed preflight is never relabeled canceled.
				final, err := store(f).RequestPreflightCancellation(ctx, m8PreflightOp3)
				if err != nil || final.State != domain.ProbePreflightCanceled {
					t.Fatalf("terminal cancel must stay unchanged: %+v err=%v", final, err)
				}
				// Canceled without prior cancel_requested is refused.
				if _, _, err := store(f).CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp4), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp4, m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				if _, err := store(f).FinishPreflight(ctx, m8PreflightOp4, m8PreflightOwnerA, m8PreflightCanceled()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("canceled without intent must conflict, got %v", err)
				}
				if _, err := store(f).RequestPreflightCancellation(ctx, m8PreflightOp5); !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("cancel missing must be not-found, got %v", err)
				}
			})
			t.Run("ExpiredCancellationCannotFinishLate", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				st := store(f)
				ctx := t.Context()
				if _, _, err := st.CreatePreflight(ctx, m8PreflightIdentity(m8PreflightOp1), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				if _, err := st.RequestPreflightCancellation(ctx, m8PreflightOp1); err != nil {
					t.Fatal(err)
				}
				m8PreflightExpire(t, f, m8PreflightOp1)
				if _, err := st.FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA, m8PreflightCanceled()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("late cancellation claimed completion: %v", err)
				}
				interrupted, err := st.RequestPreflightCancellation(ctx, m8PreflightOp1)
				if err != nil || interrupted.State != domain.ProbePreflightInterrupted {
					t.Fatalf("expired intent did not reconcile interruption: %+v %v", interrupted, err)
				}
				if _, err := st.FinishPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA, m8PreflightCanceled()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("historical interruption overwritten: %v", err)
				}
			})
			t.Run("ObserveColdReadNeverCreates", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				before := m8PreflightCount(t, f.db)
				if _, err := store(f).ObservePreflight(ctx, m8PreflightOp6); !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("cold observe missing must be not-found, got %v", err)
				}
				if got := m8PreflightCount(t, f.db); got != before {
					t.Fatalf("observe created work: %d -> %d", before, got)
				}
				if _, err := store(f).ObservePreflight(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid operation id must fail validation, got %v", err)
				}
			})
			t.Run("SeparateConnectionConcurrentCreates", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				second := m8SecondPreflightDB(t, f)
				ctx := t.Context()
				id := m8PreflightIdentity(m8PreflightOp5)
				owners := []string{m8PreflightOwnerA, m8PreflightOwnerB}
				const workers = 6
				created := make([]bool, workers)
				errs := make([]error, workers)
				ops := make([]*domain.ProbePreflightOperation, workers)
				var wg sync.WaitGroup
				for i := range workers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						st := repository.NewProbePreflightStore(f.db)
						if i%2 == 1 {
							st = repository.NewProbePreflightStore(second)
						}
						// Alternate the presented owner: identity binding must
						// still resolve to the single committed original owner.
						ops[i], created[i], errs[i] = st.CreatePreflight(ctx, id, owners[i%2])
					}()
				}
				wg.Wait()
				wins := 0
				for i := range workers {
					if errs[i] != nil {
						t.Fatalf("worker %d: %v", i, errs[i])
					}
					if created[i] {
						wins++
					}
					if ops[i] == nil || ops[i].OwnerID != m8PreflightOwnerA && ops[i].OwnerID != m8PreflightOwnerB {
						t.Fatalf("worker %d row: %+v", i, ops[i])
					}
				}
				if wins != 1 {
					t.Fatalf("exact concurrent creates: %d commit winners, want 1", wins)
				}
				var winner *domain.ProbePreflightOperation
				for i := range workers {
					if created[i] {
						winner = ops[i]
					}
				}
				for i := range workers {
					if ops[i].OwnerID != winner.OwnerID || !ops[i].DeadlineAt.Equal(winner.DeadlineAt) {
						t.Fatalf("workers disagree on the committed receipt: %+v vs %+v", ops[i], winner)
					}
				}
			})
			t.Run("CancelFinishRaceStaysConsistent", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				second := m8SecondPreflightDB(t, f)
				ctx := t.Context()
				if _, _, err := store(f).CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp6), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				if _, err := store(f).BeginPreflight(ctx, m8PreflightOp6, m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				// A second replica requests cancellation while the executor
				// finishes: whichever wins, the stored facts stay coherent.
				var cancelOp *domain.ProbePreflightOperation
				var finishOp *domain.ProbePreflightOperation
				var cancelErr, finishErr error
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					cancelOp, cancelErr = repository.NewProbePreflightStore(second).
						RequestPreflightCancellation(ctx, m8PreflightOp6)
				}()
				go func() {
					defer wg.Done()
					finishOp, finishErr = store(f).FinishPreflight(ctx, m8PreflightOp6, m8PreflightOwnerA, m8PreflightPassed())
				}()
				wg.Wait()
				if cancelErr != nil {
					t.Fatalf("cancel intent must persist on any replica: %v", cancelErr)
				}
				final, err := store(f).ObservePreflight(ctx, m8PreflightOp6)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case finishErr == nil:
					// Finish won the row lock: the passed receipt is terminal and
					// the cancel replica observed it unchanged.
					if finishOp.State != domain.ProbePreflightPassed ||
						final.State != domain.ProbePreflightPassed || cancelOp.State != domain.ProbePreflightPassed {
						t.Fatalf("finish-win invariants: finish=%+v final=%+v cancel=%+v", finishOp, final, cancelOp)
					}
				case errors.Is(finishErr, ports.ErrConflict):
					// Cancel won: passed is refused and the executor records the
					// genuine joined outcome next.
					if final.State != domain.ProbePreflightCancelRequested || cancelOp.State != domain.ProbePreflightCancelRequested {
						t.Fatalf("cancel-win invariants: final=%+v cancel=%+v", final, cancelOp)
					}
					canceled, err := store(f).FinishPreflight(ctx, m8PreflightOp6, m8PreflightOwnerA, m8PreflightCanceled())
					if err != nil || canceled.State != domain.ProbePreflightCanceled {
						t.Fatalf("joined cancel after lost race: %+v err=%v", canceled, err)
					}
				default:
					t.Fatalf("finish raced with an unexpected error: %v", finishErr)
				}
			})
			t.Run("RawCorruptionAndSchemaRefusal", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				st := store(f)
				if _, _, err := st.CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp1), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				// SQL CHECK must refuse NULL outcomes too (SQL CHECK accepts
				// UNKNOWN unless every required branch field is explicit).
				for _, assignment := range []string{
					"state = 'preflight_passed'",
					"state = 'preflight_passed', result_os = 'linux', result_architecture = 'amd64', result_privilege = 'root', result_systemd = 1",
					"state = 'failed'", "state = 'canceled'", "state = 'interrupted'",
				} {
					if _, err := f.db.ExecContext(ctx, "UPDATE probe_host_preflights SET "+assignment+" WHERE operation_id = ?", m8PreflightOp1); err == nil {
						t.Fatalf("schema accepted incomplete outcome: %s", assignment)
					}
				}
				// Schema CHECK refuses an unknown state outright.
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_host_preflights SET state = 'exploded' WHERE operation_id = ?", m8PreflightOp1); err == nil {
					t.Fatal("schema accepted an unknown preflight state")
				}
				// Schema CHECK refuses result columns on a failed row.
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_host_preflights SET state = 'failed', error_code = 'timeout', result_os = 'linux' WHERE operation_id = ?", m8PreflightOp1); err == nil {
					t.Fatal("schema accepted a result on a failed row")
				}
				// Schema-legal but domain-corrupt rows fail closed on read:
				// the user charset and canonical fingerprint are domain rules.
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_host_preflights SET ssh_user = 'root!' WHERE operation_id = ?", m8PreflightOp1); err != nil {
					t.Fatal(err)
				}
				m8PreflightExpire(t, f, m8PreflightOp1)
				if _, err := st.ObservePreflight(ctx, m8PreflightOp1); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("expired corrupt user must fail closed, got %v", err)
				}
				var state string
				if err := f.db.NewRaw("SELECT state FROM probe_host_preflights WHERE operation_id = ?", m8PreflightOp1).Scan(ctx, &state); err != nil || state != domain.ProbePreflightQueued {
					t.Fatalf("expiry repaired corrupt receipt: %s %v", state, err)
				}
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_host_preflights SET ssh_user = 'phoenix', client_key_fingerprint = 'SHA256:short' WHERE operation_id = ?", m8PreflightOp1); err != nil {
					t.Fatal(err)
				}
				if _, err := st.ObservePreflight(ctx, m8PreflightOp1); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("corrupt fingerprint must fail closed, got %v", err)
				}
			})
			t.Run("ConflictPathsRollbackWithoutPartialWrite", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				st := store(f)
				if _, _, err := st.CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp2), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				before, err := st.ObservePreflight(ctx, m8PreflightOp2)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.BeginPreflight(ctx, m8PreflightOp2, m8PreflightOwnerB); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("wrong owner begin: %v", err)
				}
				after, err := st.ObservePreflight(ctx, m8PreflightOp2)
				if err != nil {
					t.Fatal(err)
				}
				if !preflightOpsEqual(before, after) {
					t.Fatalf("conflicting begin mutated the row: before=%+v after=%+v", before, after)
				}
			})
			t.Run("ClockRollbackNeverRewindsUpdatedAt", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				st := store(f)
				if _, _, err := st.CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp3), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				// Simulate DB-clock rollback: the stored updated_at is ahead of
				// the sampled clock; the next transition must keep it.
				future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_host_preflights SET updated_at = ? WHERE operation_id = ?", future, m8PreflightOp3); err != nil {
					t.Fatal(err)
				}
				intent, err := st.RequestPreflightCancellation(ctx, m8PreflightOp3)
				if err != nil || !intent.UpdatedAt.Equal(future) {
					t.Fatalf("clock rollback rewound updated_at: %+v err=%v", intent, err)
				}
			})
			t.Run("DowngradeRefusesNonterminalKeepsTerminalHistory", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				st := store(f)
				// A live queued receipt blocks the downgrade loudly.
				if _, _, err := st.CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp4), m8PreflightOwnerA); err != nil {
					t.Fatal(err)
				}
				if err := runEngineMigration(t, f.db, engine, "083_probe_host_preflights", "down"); err == nil {
					t.Fatal("downgrade proceeded with a nonterminal receipt present")
				}
				// An expired-but-unreconciled row is still nonterminal and blocks.
				m8PreflightExpire(t, f, m8PreflightOp4)
				if err := runEngineMigration(t, f.db, engine, "083_probe_host_preflights", "down"); err == nil {
					t.Fatal("downgrade proceeded with an unreconciled expired receipt")
				}
				// Reconcile and finish honestly; terminal history may be removed.
				if _, err := st.ObservePreflight(ctx, m8PreflightOp4); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := runEngineMigration(t, f.db, engine, "083_probe_host_preflights", "up"); err != nil {
						t.Errorf("restore 083 after downgrade rehearsal: %v", err)
					}
				})
				if err := runEngineMigration(t, f.db, engine, "083_probe_host_preflights", "down"); err != nil {
					t.Fatalf("terminal-only downgrade: %v", err)
				}
				if err := runEngineMigration(t, f.db, engine, "083_probe_host_preflights", "up"); err != nil {
					t.Fatalf("re-upgrade: %v", err)
				}
				if _, _, err := st.CreatePreflight(ctx,
					m8PreflightIdentity(m8PreflightOp5), m8PreflightOwnerA); err != nil {
					t.Fatalf("store unusable after downgrade rehearsal: %v", err)
				}
			})
		})
	}
}

// m8SecondPreflightDB opens a genuinely separate connection pool against the
// same database so concurrency leaves exercise independent writers, not bun
// goroutine multiplexing over one handle.
func m8SecondPreflightDB(t *testing.T, f probeRegistryFixture) *bun.DB {
	t.Helper()
	var db *bun.DB
	var err error
	if f.engine == "sqlite" {
		db, err = sqlite.NewDB(f.dsn)
	} else {
		db, err = mariadb.NewDB(f.dsn)
	}
	if err != nil {
		t.Fatalf("open second %s handle: %v", f.engine, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func preflightOpsEqual(a, b *domain.ProbePreflightOperation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Identity != b.Identity || a.OwnerID != b.OwnerID || a.State != b.State ||
		a.ErrorCode != b.ErrorCode || !a.CreatedAt.Equal(b.CreatedAt) ||
		!a.UpdatedAt.Equal(b.UpdatedAt) || !a.DeadlineAt.Equal(b.DeadlineAt) {
		return false
	}
	if a.Result == nil || b.Result == nil {
		return a.Result == nil && b.Result == nil
	}
	return *a.Result == *b.Result
}
