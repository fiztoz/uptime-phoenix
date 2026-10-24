package repository_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// m8CleanID and m8CleanAttempt are canonical, globally unique cleanup and
// cleanup-attempt UUIDs derived from one fixture number.
func m8CleanID(n int) string { return fmt.Sprintf("dead%04d-dead-4ead-8ead-deadbeef%04d", n, n) }

func m8CleanAttempt(n int) string { return fmt.Sprintf("feed%04d-feed-4eed-8eed-feedface%04d", n, n) }

// m8CleanIdentity is a valid cleanup request over a root systemd install. The
// cleanup's transient key fingerprint deliberately differs from the install's.
func m8CleanIdentity(operationID string, n int) domain.ProbeCleanupIdentity {
	install := m8ActIdentity(operationID).Install
	return domain.ProbeCleanupIdentity{
		CleanupID: m8CleanID(n), Install: install,
		ClientKeyFingerprint: m8InstallFingerprint('c'), RequestedBy: install.RequestedBy,
	}
}

// m8CleanRequest is the host-action admission request for one cleanup attempt.
func m8CleanRequest(signer ports.ProbeHostActionGrantSigner, operationID, ownerID string, n int) domain.ProbeHostActionAdmissionRequest {
	intent := m8CleanIdentity(operationID, n).Intent(ownerID, m8CleanAttempt(n))
	return domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}}
}

// m8CleanRaw is the test-only raw view of one stored cleanup row.
type m8CleanRaw struct {
	bun.BaseModel        `bun:"table:probe_install_cleanups"`
	CleanupID            string    `bun:"cleanup_id,pk"`
	OperationID          string    `bun:"operation_id"`
	ClientKeyFingerprint string    `bun:"client_key_fingerprint"`
	RequestedBy          int64     `bun:"requested_by"`
	OwnerID              string    `bun:"owner_id"`
	State                string    `bun:"state"`
	ErrorCode            *string   `bun:"error_code"`
	AttemptID            *string   `bun:"attempt_id"`
	CreatedAt            time.Time `bun:"created_at"`
	UpdatedAt            time.Time `bun:"updated_at"`
	DeadlineAt           time.Time `bun:"deadline_at"`
}

// m8CleanRawAdmission is the test-only raw view of one cleanup admission.
type m8CleanRawAdmission struct {
	bun.BaseModel  `bun:"table:probe_cleanup_admissions"`
	AttemptID      string    `bun:"attempt_id"`
	CleanupID      string    `bun:"cleanup_id"`
	OwnerID        string    `bun:"owner_id"`
	AuthorityKeyID string    `bun:"authority_key_id"`
	NonceHex       string    `bun:"nonce_hex"`
	MarkerSHA256   string    `bun:"marker_sha256"`
	AdmittedAt     time.Time `bun:"admitted_at"`
}

// m8CleanEnv is one isolated engine fixture with the cleanup store next to the
// activation and install journal stores it must coordinate with.
type m8CleanEnv struct {
	*m8ActEnv
	cstore *repository.ProbeInstallCleanupStore
	ctx    context.Context
	cancel context.CancelFunc
}

func newM8CleanEnv(t *testing.T, engine string) *m8CleanEnv {
	t.Helper()
	base := newM8ActEnv(t, engine)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return &m8CleanEnv{m8ActEnv: base, cstore: repository.NewProbeInstallCleanupStore(base.f.db), ctx: ctx, cancel: cancel}
}

func (e *m8CleanEnv) raw(n int) m8CleanRaw {
	e.t.Helper()
	row := new(m8CleanRaw)
	if err := e.f.db.NewSelect().Model(row).Where("cleanup_id = ?", m8CleanID(n)).Scan(e.t.Context()); err != nil {
		e.t.Fatalf("raw read cleanup %d: %v", n, err)
	}
	return *row
}

func (e *m8CleanEnv) rawAdmissions() []m8CleanRawAdmission {
	e.t.Helper()
	var rows []m8CleanRawAdmission
	if err := e.f.db.NewSelect().Model(&rows).Order("attempt_id ASC").Scan(e.t.Context()); err != nil {
		e.t.Fatalf("raw read cleanup admissions: %v", err)
	}
	return rows
}

// snapshot renders every durable column of one cleanup (and its admission) so a
// refused transaction can prove it left nothing behind.
func (e *m8CleanEnv) snapshot(n int) string {
	e.t.Helper()
	text := func(v *string) string {
		if v == nil {
			return "<nil>"
		}
		return *v
	}
	r := e.raw(n)
	out := fmt.Sprintf("cleanup op=%s state=%s owner=%s error=%s attempt=%s fp=%s by=%d created=%s updated=%s deadline=%s",
		r.OperationID, r.State, r.OwnerID, text(r.ErrorCode), text(r.AttemptID), r.ClientKeyFingerprint, r.RequestedBy,
		r.CreatedAt, r.UpdatedAt, r.DeadlineAt)
	for _, a := range e.rawAdmissions() {
		if a.CleanupID == m8CleanID(n) {
			out += fmt.Sprintf(" | admission attempt=%s nonce=%s at=%s", a.AttemptID, a.NonceHex, a.AdmittedAt)
		}
	}
	return out
}

func (e *m8CleanEnv) rows(want int64) {
	e.t.Helper()
	if got := m8ActCount(e.t, e.f, "probe_install_cleanups"); got != want {
		e.t.Fatalf("cleanup row count: got %d want %d", got, want)
	}
}

func (e *m8CleanEnv) admissions(want int64) {
	e.t.Helper()
	if got := m8ActCount(e.t, e.f, "probe_cleanup_admissions"); got != want {
		e.t.Fatalf("cleanup admission row count: got %d want %d", got, want)
	}
}

// shift moves every stored stamp of one cleanup by the same amount, so the row
// stays internally consistent (a valid stored receipt) while the DB clock is
// either already past its fixed deadline (negative) or behind it (positive).
func (e *m8CleanEnv) shift(n int, d time.Duration) {
	e.t.Helper()
	r := e.raw(n)
	if _, err := e.f.db.ExecContext(e.t.Context(),
		"UPDATE probe_install_cleanups SET created_at = ?, updated_at = ?, deadline_at = ? WHERE cleanup_id = ?",
		r.CreatedAt.Add(d), r.UpdatedAt.Add(d), r.DeadlineAt.Add(d), r.CleanupID); err != nil {
		e.t.Fatalf("shift cleanup %d: %v", n, err)
	}
}

func (e *m8CleanEnv) expire(n int) { e.shift(n, -2*time.Hour) }

// check asserts every returned receipt is a valid domain cleanup whose stamps
// crossed the DB boundary as UTC microseconds.
func (e *m8CleanEnv) check(c *domain.ProbeCleanup) *domain.ProbeCleanup {
	e.t.Helper()
	if c == nil {
		e.t.Fatal("nil cleanup receipt")
	}
	if !c.Valid() {
		e.t.Fatalf("returned receipt is not a valid domain cleanup: %+v", c)
	}
	for name, stamp := range map[string]time.Time{"created": c.CreatedAt, "updated": c.UpdatedAt, "deadline": c.DeadlineAt} {
		if stamp.IsZero() || stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 {
			e.t.Fatalf("%s stamp is not a UTC microsecond value: %v", name, stamp)
		}
	}
	return c
}

func (e *m8CleanEnv) create(op string, n int) *domain.ProbeCleanup {
	e.t.Helper()
	c, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(op, n), m8InstallOwnerA)
	if err != nil || !created {
		e.t.Fatalf("create cleanup %d over %s: created=%v err=%v", n, op, created, err)
	}
	e.check(c)
	if c.State != domain.ProbeCleanupQueued || c.OwnerID != m8InstallOwnerA || c.AttemptID != "" || c.ErrorCode != "" ||
		c.Identity != m8CleanIdentity(op, n) || !c.DeadlineAt.Equal(c.CreatedAt.Add(domain.ProbeCleanupLifetime)) {
		e.t.Fatalf("queued cleanup shape: %+v", c)
	}
	return c
}

func (e *m8CleanEnv) begin(n int) *domain.ProbeCleanup {
	e.t.Helper()
	c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(n), m8InstallOwnerA, m8CleanAttempt(n))
	if err != nil || c == nil || c.State != domain.ProbeCleanupRunning || c.AttemptID != m8CleanAttempt(n) || c.ErrorCode != "" {
		e.t.Fatalf("begin cleanup %d: %+v err=%v", n, c, err)
	}
	return e.check(c)
}

func (e *m8CleanEnv) running(op string, n int) *domain.ProbeCleanup {
	e.t.Helper()
	e.create(op, n)
	return e.begin(n)
}

func (e *m8CleanEnv) record(n int, outcome domain.ProbeInstallResourceOutcome) *domain.ProbeCleanup {
	e.t.Helper()
	c, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(n), m8InstallOwnerA, m8CleanAttempt(n), outcome)
	if err != nil {
		e.t.Fatalf("record cleanup %d %s: %v", n, outcome.State, err)
	}
	return e.check(c)
}

func (e *m8CleanEnv) observe(n int) *domain.ProbeCleanup {
	e.t.Helper()
	c, err := e.cstore.ObserveCleanup(e.ctx, m8CleanID(n))
	if err != nil {
		e.t.Fatalf("observe cleanup %d: %v", n, err)
	}
	return e.check(c)
}

func (e *m8CleanEnv) latest(op string) *domain.ProbeCleanup {
	e.t.Helper()
	c, err := e.cstore.LatestCleanup(e.ctx, op)
	if err != nil {
		e.t.Fatalf("latest cleanup of %s: %v", op, err)
	}
	return e.check(c)
}

func (e *m8CleanEnv) abandon(n int) *domain.ProbeCleanup {
	e.t.Helper()
	c, err := e.cstore.AbandonCleanup(e.ctx, m8CleanID(n), m8InstallOwnerA)
	if err != nil {
		e.t.Fatalf("abandon cleanup %d: %v", n, err)
	}
	return e.check(c)
}

func (e *m8CleanEnv) conflict(what string, err error) {
	e.t.Helper()
	if !errors.Is(err, ports.ErrConflict) {
		e.t.Fatalf("%s must conflict, got %v", what, err)
	}
}

func (e *m8CleanEnv) ineligible(what string, err error) {
	e.t.Helper()
	if !errors.Is(err, domain.ErrProbeCleanupIneligible) {
		e.t.Fatalf("%s must be ineligible, got %v", what, err)
	}
}

// expectRaw asserts the stored lifecycle fields of one cleanup row.
func (e *m8CleanEnv) expectRaw(n int, state, code string, attempted bool) {
	e.t.Helper()
	r := e.raw(n)
	gotCode := ""
	if r.ErrorCode != nil {
		gotCode = *r.ErrorCode
	}
	if r.State != state || gotCode != code || (r.AttemptID != nil) != attempted {
		e.t.Fatalf("cleanup %d stored %s/%q attempt=%v, want %s/%q attempt=%v", n, r.State, gotCode, r.AttemptID != nil, state, code, attempted)
	}
}

// sameCleanup compares two receipts field by field with time equality.
func m8CleanSame(a, b *domain.ProbeCleanup) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Identity == b.Identity && a.OwnerID == b.OwnerID && a.State == b.State && a.ErrorCode == b.ErrorCode &&
		a.AttemptID == b.AttemptID && a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt) && a.DeadlineAt.Equal(b.DeadlineAt)
}

func (e *m8CleanEnv) same(got, want *domain.ProbeCleanup, what string) {
	e.t.Helper()
	if !m8CleanSame(got, want) {
		e.t.Fatalf("%s changed the receipt:\n got %+v\nwant %+v", what, got, want)
	}
}

// installInState drives one fixture install to the named install state through
// the production install journal store.
func (e *m8CleanEnv) installInState(op, state string) {
	e.t.Helper()
	istore, ctx := m8InstallStore(e.f), e.t.Context()
	install := m8ActIdentity(op).Install
	attempt := m8ActInstallAttempt(op, 1)
	applyFirst := func() {
		m8InstallBeginOK(e.t, istore, op, m8InstallOwnerA)
		m8InstallReserveOK(e.t, istore, op, m8InstallOwnerA, "data_directory", attempt)
	}
	switch state {
	case domain.ProbeInstallResourcesReady:
		e.installReady(op)
	case domain.ProbeInstallQueued:
		m8InstallCreate(e.t, istore, install, m8InstallOwnerA)
	case domain.ProbeInstallRunning:
		m8InstallCreate(e.t, istore, install, m8InstallOwnerA)
		applyFirst()
	case "running_prefix":
		m8InstallCreate(e.t, istore, install, m8InstallOwnerA)
		applyFirst()
		m8InstallRecordOK(e.t, istore, op, m8InstallOwnerA, "data_directory", attempt, m8InstallApplied())
	case domain.ProbeInstallFailed:
		m8InstallCreate(e.t, istore, install, m8InstallOwnerA)
		applyFirst()
		m8InstallRecordOK(e.t, istore, op, m8InstallOwnerA, "data_directory", attempt, m8InstallApplied())
		if _, err := istore.FinishInstall(ctx, op, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "execution_failed")); err != nil {
			e.t.Fatal(err)
		}
	case domain.ProbeInstallCanceled:
		m8InstallCreate(e.t, istore, install, m8InstallOwnerA)
		if _, err := istore.RequestInstallCancellation(ctx, op); err != nil {
			e.t.Fatal(err)
		}
	case domain.ProbeInstallInterrupted:
		m8InstallCreate(e.t, istore, install, m8InstallOwnerA)
		applyFirst()
		m8InstallRecordOK(e.t, istore, op, m8InstallOwnerA, "data_directory", attempt, m8InstallUnknownOutcome())
	case domain.ProbeInstallCancelRequested:
		e.installReady(op)
		if c, err := istore.RequestInstallCancellation(ctx, op); err != nil || c.State != domain.ProbeInstallCancelRequested {
			e.t.Fatalf("install cancel intent: %+v err=%v", c, err)
		}
	default:
		e.t.Fatalf("unknown fixture install state %s", state)
	}
}

type m8CleanLeaf struct {
	name string
	run  func(t *testing.T, engine string)
}

// TestM8InstallCleanup proves the durable M8 install cleanup journal and its
// one-shot host-action admission through the real production fixtures on both
// engines: creation only over a cleanup-eligible install with a terminal (or no)
// activation and no live or cleaned cleanup, immutable exact retries, the
// queued/running/settled lifecycle for all three executor outcomes,
// owner/attempt/state/expiry fencing after the shared row lock on the DB clock,
// honest cold expiry, corrupt-row refusal before reconciliation, deterministic
// newest-first ordering, irreversible single-use admission, activation
// refusal once a cleanup exists, rollback discipline, guarded downgrade and UTC
// stamps. MariaDB leaves require TEST_MARIADB_DSN; a skip establishes nothing
// about InnoDB, so the real-engine gate must execute and count them.
func TestM8InstallCleanup(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, leaf := range m8CleanLeaves() {
				t.Run(leaf.name, func(t *testing.T) { leaf.run(t, engine) })
			}
		})
	}
}

func m8CleanLeaves() []m8CleanLeaf {
	leaves := m8CleanCreateLeaves()
	leaves = append(leaves, m8CleanLifecycleLeaves()...)
	leaves = append(leaves, m8CleanAdmissionLeaves()...)
	return append(leaves, m8CleanSchemaLeaves()...)
}

func m8CleanCreateLeaves() []m8CleanLeaf {
	return []m8CleanLeaf{
		{"CreateQueuedOverEachEligibleInstall", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			states := []string{domain.ProbeInstallResourcesReady, domain.ProbeInstallFailed, domain.ProbeInstallCanceled, domain.ProbeInstallInterrupted}
			for i, state := range states {
				op := m8ActOps[i]
				e.installInState(op, state)
				before := m8InstallSnapshot(t, e.f, op)
				c := e.create(op, i+1)
				if after := m8InstallSnapshot(t, e.f, op); after != before {
					t.Fatalf("cleanup create over %s modified the install journal", state)
				}
				r := e.raw(i + 1)
				if r.OperationID != op || r.OwnerID != m8InstallOwnerA || r.State != domain.ProbeCleanupQueued || r.ErrorCode != nil ||
					r.AttemptID != nil || r.ClientKeyFingerprint != m8InstallFingerprint('c') || r.RequestedBy != m8InstallActor ||
					!r.CreatedAt.Equal(c.CreatedAt) || !r.UpdatedAt.Equal(c.CreatedAt) || !r.DeadlineAt.Equal(c.DeadlineAt) {
					t.Fatalf("stored queued row differs from the receipt (%s): %+v", state, r)
				}
				e.same(e.observe(i+1), c, "observe")
				e.same(e.latest(op), c, "latest")
			}
			e.rows(int64(len(states)))
			e.admissions(0)
		}},
		{"CreateExactRetryReturnsStored", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			first := e.create(m8InstallOp1, 1)
			// A retrying process presents another owner: it still reads the
			// original receipt and never gets a fresh grant.
			again, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 1), m8InstallOwnerB)
			if err != nil || created {
				t.Fatalf("exact retry: created=%v err=%v", created, err)
			}
			e.check(again)
			e.same(again, first, "exact retry")
			if again.OwnerID != m8InstallOwnerA {
				t.Fatal("retry rebound the cleanup owner")
			}
			e.begin(1)
			running := e.observe(1)
			again, created, err = e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 1), m8InstallOwnerA)
			if err != nil || created {
				t.Fatalf("retry of a running cleanup: created=%v err=%v", created, err)
			}
			e.same(again, running, "retry of a running cleanup")
			e.record(1, m8InstallApplied())
			settled := e.observe(1)
			again, created, err = e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 1), m8InstallOwnerA)
			if err != nil || created {
				t.Fatalf("retry of a cleaned cleanup: created=%v err=%v", created, err)
			}
			e.same(again, settled, "retry of a cleaned cleanup")
			e.rows(1)
		}},
		{"CreateChangedIdentityConflicts", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.installReady(m8InstallOp2)
			e.create(m8InstallOp1, 1)
			before := e.snapshot(1)
			variants := map[string]func(*domain.ProbeCleanupIdentity){
				"client_key":  func(i *domain.ProbeCleanupIdentity) { i.ClientKeyFingerprint = m8InstallFingerprint('d') },
				"actor":       func(i *domain.ProbeCleanupIdentity) { i.RequestedBy++ },
				"host_key":    func(i *domain.ProbeCleanupIdentity) { i.Install.HostKeyFingerprint = m8InstallFingerprint('e') },
				"binary":      func(i *domain.ProbeCleanupIdentity) { i.Install.Binary.Version = "9.9.9" },
				"ssh_address": func(i *domain.ProbeCleanupIdentity) { i.Install.Host.Address = "192.0.2.99:22" },
				"other_install": func(i *domain.ProbeCleanupIdentity) {
					i.Install = m8ActIdentity(m8InstallOp2).Install
				},
			}
			for name, change := range variants {
				identity := m8CleanIdentity(m8InstallOp1, 1)
				change(&identity)
				if c, created, err := e.cstore.CreateCleanup(e.ctx, identity, m8InstallOwnerA); created || c != nil || !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("changed identity (%s) must conflict: created=%v err=%v", name, created, err)
				}
			}
			e.rows(1)
			if e.snapshot(1) != before {
				t.Fatal("conflicting retries modified the stored cleanup")
			}
		}},
		{"CreateMissingInstallNotFound", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			if c, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp9, 1), m8InstallOwnerA); created || c != nil || !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("missing install must be not-found: created=%v err=%v", created, err)
			}
			e.rows(0)
		}},
		{"CreateInstallIdentityMismatchConflicts", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			before := m8InstallSnapshot(t, e.f, m8InstallOp1)
			identity := m8CleanIdentity(m8InstallOp1, 1)
			identity.Install.Binary.SHA256 = strings.Repeat("b", 64)
			if c, created, err := e.cstore.CreateCleanup(e.ctx, identity, m8InstallOwnerA); created || c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("install identity mismatch must conflict: created=%v err=%v", created, err)
			}
			e.rows(0)
			if m8InstallSnapshot(t, e.f, m8InstallOp1) != before {
				t.Fatal("refused create modified the install journal")
			}
		}},
		{"CreateRejectsInvalidRequest", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			bad := map[string]domain.ProbeCleanupIdentity{}
			noKey := m8CleanIdentity(m8InstallOp1, 1)
			noKey.ClientKeyFingerprint = ""
			bad["no_client_key"] = noKey
			noActor := m8CleanIdentity(m8InstallOp1, 1)
			noActor.RequestedBy = 0
			bad["no_actor"] = noActor
			badID := m8CleanIdentity(m8InstallOp1, 1)
			badID.CleanupID = "not-a-uuid"
			bad["bad_cleanup_id"] = badID
			docker := m8CleanIdentity(m8InstallOp1, 1)
			docker.Install = m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp1)
			bad["docker_install"] = docker
			unprivileged := m8CleanIdentity(m8InstallOp1, 1)
			unprivileged.Install.Host.User = "phoenix"
			bad["non_root_install"] = unprivileged
			for name, identity := range bad {
				if c, created, err := e.cstore.CreateCleanup(e.ctx, identity, m8InstallOwnerA); created || c != nil || !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid identity (%s) must be a validation error: created=%v err=%v", name, created, err)
				}
			}
			if _, _, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 1), "owner"); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid owner must be a validation error: %v", err)
			}
			var nilStore *repository.ProbeInstallCleanupStore
			if _, _, err := nilStore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 1), m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("nil store must refuse: %v", err)
			}
			e.rows(0)
		}},
		{"CreateRefusesIneligibleInstallStates", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			states := []string{domain.ProbeInstallQueued, domain.ProbeInstallRunning, domain.ProbeInstallCancelRequested, "running_prefix"}
			for i, state := range states {
				op := m8ActOps[i]
				e.installInState(op, state)
				before := m8InstallSnapshot(t, e.f, op)
				c, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(op, i+1), m8InstallOwnerA)
				if created || c != nil {
					t.Fatalf("install in %s was cleanable", state)
				}
				e.ineligible("install in "+state, err)
				if m8InstallSnapshot(t, e.f, op) != before {
					t.Fatalf("refused create modified the %s install", state)
				}
			}
			e.rows(0)
		}},
		{"CreateReconcilesExpiredInstall", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			// A running install whose hard deadline passed is interrupted by the
			// reconciliation inside the create, which then becomes eligible.
			e.installInState(m8InstallOp1, domain.ProbeInstallRunning)
			m8InstallExpire(t, e.f, m8InstallOp1)
			e.create(m8InstallOp1, 1)
			op := m8InstallRawOp(t, e.f, m8InstallOp1)
			if op.State != domain.ProbeInstallInterrupted || op.ErrorCode == nil || *op.ErrorCode != "deadline_expired" {
				t.Fatalf("install expiry was not reconciled by the create: %+v", op)
			}
			if res := m8InstallRawResources(t, e.f, m8InstallOp1)[0]; res.State != domain.ProbeInstallResourceUnknown {
				t.Fatalf("in-flight resource was not made unknown: %+v", res)
			}
			// An expired resources_ready install is also interrupted, then cleanable.
			e.installReady(m8InstallOp2)
			m8InstallExpire(t, e.f, m8InstallOp2)
			e.create(m8InstallOp2, 2)
			if op := m8InstallRawOp(t, e.f, m8InstallOp2); op.State != domain.ProbeInstallInterrupted {
				t.Fatalf("expired resources_ready install was not interrupted: %+v", op)
			}
		}},
		{"RefusedCreateKeepsInstallExpiryReconciliation", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installInState(m8InstallOp1, domain.ProbeInstallRunning)
			m8InstallExpire(t, e.f, m8InstallOp1)
			identity := m8CleanIdentity(m8InstallOp1, 1)
			identity.Install.Binary.Version = "9.9.9"
			if _, created, err := e.cstore.CreateCleanup(e.ctx, identity, m8InstallOwnerA); created || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("mismatched identity must conflict: created=%v err=%v", created, err)
			}
			e.rows(0)
			// Read the raw row BEFORE anything can reconcile again: a rolled-back
			// expiry must not be hidden by a subsequent healing read.
			op := m8InstallRawOp(t, e.f, m8InstallOp1)
			if op.State != domain.ProbeInstallInterrupted || op.ErrorCode == nil || *op.ErrorCode != "deadline_expired" {
				t.Fatalf("install expiry reconciliation did not commit through the refusal: %+v", op)
			}
		}},
		{"CreateRefusesNonterminalActivation", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			owner := m8InstallOwnerA
			e.fresh(m8InstallOp1) // queued
			e.fresh(m8InstallOp2)
			e.m8ActEnv.begin(m8InstallOp2, owner) // running
			e.fresh(m8InstallOp3)
			e.m8ActEnv.begin(m8InstallOp3, owner)
			e.reserve(m8InstallOp3, owner, domain.ProbeActivationProbeInit) // in-flight step
			e.fresh(m8InstallOp4)
			e.m8ActEnv.begin(m8InstallOp4, owner)
			if a, err := e.store.RequestActivationCancellation(e.ctx, m8InstallOp4); err != nil || a.State != domain.ProbeActivationCancelRequested {
				t.Fatalf("cancel intent: %+v err=%v", a, err)
			}
			for i, op := range []string{m8InstallOp1, m8InstallOp2, m8InstallOp3, m8InstallOp4} {
				before := m8ActSnapshot(t, e.f, op)
				c, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(op, i+1), owner)
				if created || c != nil {
					t.Fatalf("nonterminal activation %s was cleanable", op)
				}
				e.ineligible("nonterminal activation "+op, err)
				if m8ActSnapshot(t, e.f, op) != before {
					t.Fatalf("refused create modified activation %s", op)
				}
			}
			e.rows(0)
		}},
		{"CreateAfterTerminalActivation", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			owner := m8InstallOwnerA
			// active
			e.fresh(m8InstallOp1)
			e.m8ActEnv.begin(m8InstallOp1, owner)
			if a := e.advance(m8InstallOp1, owner, 4); a.State != domain.ProbeActivationActive {
				t.Fatalf("fixture activation must be active: %+v", a)
			}
			// failed
			e.fresh(m8InstallOp2)
			e.m8ActEnv.begin(m8InstallOp2, owner)
			if _, err := e.store.FinishActivation(e.ctx, m8InstallOp2, owner, m8ActFinish(domain.ProbeInstallFailed, "execution_failed")); err != nil {
				t.Fatal(err)
			}
			// canceled
			e.fresh(m8InstallOp3)
			if _, err := e.store.RequestActivationCancellation(e.ctx, m8InstallOp3); err != nil {
				t.Fatal(err)
			}
			// interrupted by the owner
			e.fresh(m8InstallOp4)
			e.m8ActEnv.begin(m8InstallOp4, owner)
			if _, err := e.store.FinishActivation(e.ctx, m8InstallOp4, owner, m8ActFinish(domain.ProbeInstallInterrupted, "owner_interrupted")); err != nil {
				t.Fatal(err)
			}
			for i, op := range []string{m8InstallOp1, m8InstallOp2, m8InstallOp3, m8InstallOp4} {
				if a := e.m8ActEnv.observe(op); !a.Terminal() {
					t.Fatalf("fixture activation %s must be terminal: %+v", op, a)
				}
				before := m8ActSnapshot(t, e.f, op)
				e.create(op, i+1)
				if m8ActSnapshot(t, e.f, op) != before {
					t.Fatalf("cleanup create modified terminal activation %s", op)
				}
			}
		}},
		{"CreateReconcilesExpiredActivation", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			owner := m8InstallOwnerA
			e.fresh(m8InstallOp1)
			e.m8ActEnv.begin(m8InstallOp1, owner)
			e.reserve(m8InstallOp1, owner, domain.ProbeActivationProbeInit)
			m8ActExpire(t, e.f, m8InstallOp1)
			e.create(m8InstallOp1, 1)
			a := m8ActRawRow(t, e.f, m8InstallOp1)
			steps := m8ActRawSteps(t, e.f, m8InstallOp1)
			if a.State != domain.ProbeActivationInterrupted || a.ErrorCode == nil || *a.ErrorCode != "deadline_expired" ||
				steps[0].State != domain.ProbeInstallResourceUnknown {
				t.Fatalf("expired activation was not reconciled by the create: %+v %+v", a, steps[0])
			}
		}},
		{"CreateRefusesNonterminalCleanup", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.create(m8InstallOp1, 1)
			for _, step := range []string{"queued", "running"} {
				if step == "running" {
					e.begin(1)
				}
				before := e.snapshot(1)
				c, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 2), m8InstallOwnerA)
				if created || c != nil {
					t.Fatalf("a second cleanup was admitted next to a %s one", step)
				}
				e.ineligible(step+" cleanup", err)
				e.rows(1)
				if e.snapshot(1) != before {
					t.Fatalf("refused create modified the %s cleanup", step)
				}
			}
		}},
		{"CreateRefusesAfterCleaned", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.running(m8InstallOp1, 1)
			cleaned := e.record(1, m8InstallApplied())
			c, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 2), m8InstallOwnerA)
			if created || c != nil {
				t.Fatal("a cleanup was admitted after the operation was cleaned")
			}
			e.ineligible("cleaned operation", err)
			// The finished cleanup's own exact retry still reads its history.
			again, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 1), m8InstallOwnerA)
			if err != nil || created {
				t.Fatalf("exact retry of the cleaned cleanup: created=%v err=%v", created, err)
			}
			e.same(again, cleaned, "retry after cleaned")
			e.rows(1)
		}},
		{"CreateAllowsNewCleanupAfterFailureOrInterruption", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.create(m8InstallOp1, 1)
			e.abandon(1) // interrupted/owner_interrupted
			e.create(m8InstallOp1, 2)
			e.begin(2)
			if c := e.record(2, m8InstallNotStartedOutcome()); c.State != domain.ProbeCleanupFailed || c.ErrorCode != "not_started" {
				t.Fatalf("not_started must fail the cleanup: %+v", c)
			}
			e.create(m8InstallOp1, 3)
			e.begin(3)
			if c := e.record(3, m8InstallUnknownOutcome()); c.State != domain.ProbeCleanupInterrupted || c.ErrorCode != "outcome_unknown" {
				t.Fatalf("unknown must interrupt the cleanup: %+v", c)
			}
			e.create(m8InstallOp1, 4)
			e.begin(4)
			e.record(4, m8InstallApplied())
			e.rows(4)
			if latest := e.latest(m8InstallOp1); latest.Identity.CleanupID != m8CleanID(4) || latest.State != domain.ProbeCleanupCleaned {
				t.Fatalf("latest must be the last, cleaned cleanup: %+v", latest)
			}
		}},
		{"CreateReconcilesExpiredCleanup", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.create(m8InstallOp1, 1)
			e.expire(1)
			e.create(m8InstallOp1, 2)
			e.expectRaw(1, domain.ProbeCleanupInterrupted, "deadline_expired", false)
			e.begin(2)
			e.expire(2)
			e.create(m8InstallOp1, 3)
			e.expectRaw(2, domain.ProbeCleanupInterrupted, "deadline_expired", true)
			e.expectRaw(3, domain.ProbeCleanupQueued, "", false)
		}},
		{"CreateCommitsAtMostOneWinner", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			const writers = 4
			type reply struct {
				cleanup *domain.ProbeCleanup
				created bool
				err     error
			}
			start := make(chan struct{})
			distinct := make(chan reply, writers)
			same := make(chan reply, writers)
			var wg sync.WaitGroup
			t.Cleanup(func() { e.cancel(); wg.Wait() })
			for i := range writers {
				peer := repository.NewProbeInstallCleanupStore(m8InstallSecondDB(t, e.f, ""))
				wg.Go(func() {
					<-start
					c, created, err := peer.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 10+i), m8InstallOwnerA)
					distinct <- reply{c, created, err}
				})
			}
			close(start)
			winners := 0
			for range writers {
				r := <-distinct
				switch {
				case r.err == nil && r.created:
					winners++
				case errors.Is(r.err, domain.ErrProbeCleanupIneligible) && !r.created && r.cleanup == nil:
				default:
					t.Fatalf("unexpected concurrent create result: %+v created=%v err=%v", r.cleanup, r.created, r.err)
				}
			}
			wg.Wait()
			if winners != 1 {
				t.Fatalf("distinct concurrent creates must yield exactly one cleanup, got %d", winners)
			}
			e.rows(1)
			// The very same cleanup ID raced by several writers is one creation and
			// otherwise exact retries.
			e2 := newM8CleanEnv(t, engine)
			e2.installReady(m8InstallOp1)
			start2 := make(chan struct{})
			var wg2 sync.WaitGroup
			t.Cleanup(func() { e2.cancel(); wg2.Wait() })
			for range writers {
				peer := repository.NewProbeInstallCleanupStore(m8InstallSecondDB(t, e2.f, ""))
				wg2.Go(func() {
					<-start2
					c, created, err := peer.CreateCleanup(e2.ctx, m8CleanIdentity(m8InstallOp1, 20), m8InstallOwnerA)
					same <- reply{c, created, err}
				})
			}
			close(start2)
			fresh := 0
			for range writers {
				r := <-same
				if r.err != nil || r.cleanup == nil {
					t.Fatalf("raced exact retry failed: %v", r.err)
				}
				if r.created {
					fresh++
				}
			}
			wg2.Wait()
			if fresh != 1 {
				t.Fatalf("raced identical creates must be one creation, got %d", fresh)
			}
			e2.rows(1)
		}},
		{"CreateActivationRefusedAfterCleanup", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			owner := m8InstallOwnerA
			for i, op := range []string{m8InstallOp1, m8InstallOp2, m8InstallOp3, m8InstallOp4} {
				e.installReady(op)
				e.create(op, i+1)
			}
			// Every cleanup state, live or settled, refuses a new activation.
			e.abandon(2)
			e.begin(3)
			e.begin(4)
			e.record(4, m8InstallApplied())
			e.expire(1)
			for _, op := range []string{m8InstallOp1, m8InstallOp2, m8InstallOp3, m8InstallOp4} {
				before := m8InstallSnapshot(t, e.f, op)
				a, created, err := e.store.CreateActivation(e.ctx, m8ActIdentity(op), owner)
				if created || a != nil || !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("activation over %s with a cleanup must conflict: created=%v err=%v", op, created, err)
				}
				if m8InstallSnapshot(t, e.f, op) != before {
					t.Fatalf("refused activation modified the install %s", op)
				}
			}
			if n := m8ActCount(t, e.f, "probe_install_activations"); n != 0 {
				t.Fatalf("refused activations left %d rows", n)
			}
			if n := m8ActCount(t, e.f, "probe_install_activation_steps"); n != 0 {
				t.Fatalf("refused activations left %d step rows", n)
			}
			// Control: an install with no cleanup still activates.
			e.installReady(m8InstallOp5)
			e.m8ActEnv.create(m8InstallOp5, owner)
			// An exact retry of an already stored terminal activation still reads
			// its history after a cleanup exists; it grants nothing.
			e.fresh(m8InstallOp6)
			canceled, err := e.store.RequestActivationCancellation(e.ctx, m8InstallOp6)
			if err != nil || !canceled.Terminal() {
				t.Fatalf("fixture activation: %+v err=%v", canceled, err)
			}
			e.create(m8InstallOp6, 6)
			again, created, err := e.store.CreateActivation(e.ctx, m8ActIdentity(m8InstallOp6), owner)
			if err != nil || created {
				t.Fatalf("exact activation retry after a cleanup: created=%v err=%v", created, err)
			}
			m8ActRequireSame(t, again, canceled, "retry after a cleanup")
		}},
		{"CreateActivationVersusCleanupSingleWinner", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			activations := repository.NewProbeInstallActivationStore(m8InstallSecondDB(t, e.f, ""))
			cleanups := repository.NewProbeInstallCleanupStore(m8InstallSecondDB(t, e.f, ""))
			start := make(chan struct{})
			var activationErr, cleanupErr error
			var activationFresh, cleanupFresh bool
			var wg sync.WaitGroup
			t.Cleanup(func() { e.cancel(); wg.Wait() })
			wg.Go(func() {
				<-start
				_, activationFresh, activationErr = activations.CreateActivation(e.ctx, m8ActIdentity(m8InstallOp1), m8InstallOwnerA)
			})
			wg.Go(func() {
				<-start
				_, cleanupFresh, cleanupErr = cleanups.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 1), m8InstallOwnerA)
			})
			close(start)
			wg.Wait()
			switch {
			case activationErr == nil && activationFresh && errors.Is(cleanupErr, domain.ErrProbeCleanupIneligible) && !cleanupFresh:
				e.rows(0)
			case cleanupErr == nil && cleanupFresh && errors.Is(activationErr, ports.ErrConflict) && !activationFresh:
				if n := m8ActCount(t, e.f, "probe_install_activations"); n != 0 {
					t.Fatalf("activation row exists next to a cleanup: %d", n)
				}
				e.rows(1)
			default:
				t.Fatalf("exactly one of activation and cleanup must win: activation fresh=%v err=%v cleanup fresh=%v err=%v",
					activationFresh, activationErr, cleanupFresh, cleanupErr)
			}
		}},
	}
}

func m8CleanRecordLeaf(name string, outcome domain.ProbeInstallResourceOutcome, state, code string, other domain.ProbeInstallResourceOutcome) m8CleanLeaf {
	return m8CleanLeaf{name, func(t *testing.T, engine string) {
		e := newM8CleanEnv(t, engine)
		e.installReady(m8InstallOp1)
		running := e.running(m8InstallOp1, 1)
		installBefore := m8InstallSnapshot(t, e.f, m8InstallOp1)
		got := e.record(1, outcome)
		if got.State != state || got.ErrorCode != code || got.AttemptID != m8CleanAttempt(1) || got.OwnerID != m8InstallOwnerA ||
			!got.CreatedAt.Equal(running.CreatedAt) || !got.DeadlineAt.Equal(running.DeadlineAt) || got.UpdatedAt.Before(running.UpdatedAt) {
			t.Fatalf("recorded receipt %+v does not settle %+v as %s/%s", got, running, state, code)
		}
		e.expectRaw(1, state, code, true)
		// An exact outcome retry reads immutable history without mutation, and a
		// different outcome can never rewrite it.
		e.same(e.record(1, outcome), got, "exact retry")
		if c, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1), other); c != nil || !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("a different outcome must conflict: %+v err=%v", c, err)
		}
		e.same(e.observe(1), got, "observe")
		e.same(e.latest(m8InstallOp1), got, "latest")
		// A settled cleanup can neither begin again nor be abandoned.
		if c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(2)); c != nil || !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("settled cleanup must not begin again: %+v err=%v", c, err)
		}
		if c, err := e.cstore.AbandonCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA); c != nil || !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("settled cleanup must not be abandoned: %+v err=%v", c, err)
		}
		if m8InstallSnapshot(t, e.f, m8InstallOp1) != installBefore {
			t.Fatal("recording a cleanup modified the install journal")
		}
		e.admissions(0)
	}}
}

func m8CleanLifecycleLeaves() []m8CleanLeaf {
	return []m8CleanLeaf{
		{"ObserveReconcilesExpiryAndKeepsTerminal", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			for _, op := range []string{m8InstallOp1, m8InstallOp2, m8InstallOp3, m8InstallOp4} {
				e.installReady(op)
			}
			e.create(m8InstallOp1, 1)
			e.running(m8InstallOp2, 2)
			e.running(m8InstallOp3, 3)
			e.record(3, m8InstallApplied())
			live := e.create(m8InstallOp4, 4)
			e.expire(1)
			e.expire(2)
			e.expire(3)
			queued := e.observe(1)
			if queued.State != domain.ProbeCleanupInterrupted || queued.ErrorCode != "deadline_expired" || queued.AttemptID != "" {
				t.Fatalf("expired queued cleanup: %+v", queued)
			}
			e.expectRaw(1, domain.ProbeCleanupInterrupted, "deadline_expired", false)
			e.same(e.observe(1), queued, "second observe")
			expired := e.observe(2)
			if expired.State != domain.ProbeCleanupInterrupted || expired.ErrorCode != "deadline_expired" || expired.AttemptID != m8CleanAttempt(2) {
				t.Fatalf("expired running cleanup must keep its attempt: %+v", expired)
			}
			e.expectRaw(2, domain.ProbeCleanupInterrupted, "deadline_expired", true)
			// Settled history is immutable even past its deadline.
			if cleaned := e.observe(3); cleaned.State != domain.ProbeCleanupCleaned || cleaned.ErrorCode != "" {
				t.Fatalf("a cleaned cleanup was rewritten by expiry: %+v", cleaned)
			}
			e.expectRaw(3, domain.ProbeCleanupCleaned, "", true)
			e.same(e.observe(4), live, "observe of a live cleanup")
		}},
		{"ObserveUnknownAndInvalidIDs", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			if c, err := e.cstore.ObserveCleanup(e.ctx, m8CleanID(9)); c != nil || !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("unknown observe: %+v err=%v", c, err)
			}
			if c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(9), m8InstallOwnerA, m8CleanAttempt(9)); c != nil || !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("unknown begin: %+v err=%v", c, err)
			}
			if c, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(9), m8InstallOwnerA, m8CleanAttempt(9), m8InstallApplied()); c != nil || !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("unknown record: %+v err=%v", c, err)
			}
			if c, err := e.cstore.AbandonCleanup(e.ctx, m8CleanID(9), m8InstallOwnerA); c != nil || !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("unknown abandon: %+v err=%v", c, err)
			}
			if _, err := e.cstore.ObserveCleanup(e.ctx, "x"); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid observe id: %v", err)
			}
			if _, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, "x"); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid begin attempt: %v", err)
			}
			if _, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(1), "x", m8CleanAttempt(1)); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid begin owner: %v", err)
			}
			if _, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1), domain.ProbeInstallResourceOutcome{State: "bogus"}); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid record outcome: %v", err)
			}
			if _, err := e.cstore.AbandonCleanup(e.ctx, "x", m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid abandon id: %v", err)
			}
			var nilStore *repository.ProbeInstallCleanupStore
			if _, err := nilStore.ObserveCleanup(e.ctx, m8CleanID(1)); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("nil store observe: %v", err)
			}
		}},
		{"LatestNotFoundWithoutCleanup", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			if c, err := e.cstore.LatestCleanup(e.ctx, m8InstallOp1); c != nil || !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("an install without cleanups: %+v err=%v", c, err)
			}
			if c, err := e.cstore.LatestCleanup(e.ctx, m8InstallOp9); c != nil || !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("an unknown install: %+v err=%v", c, err)
			}
			if _, err := e.cstore.LatestCleanup(e.ctx, "x"); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid operation id: %v", err)
			}
		}},
		{"LatestReturnsNewestAndReconciles", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.installReady(m8InstallOp2)
			e.create(m8InstallOp1, 1)
			e.abandon(1)
			newest := e.create(m8InstallOp1, 2)
			e.create(m8InstallOp2, 3)
			e.same(e.latest(m8InstallOp1), newest, "latest of the first operation")
			if got := e.latest(m8InstallOp2); got.Identity.CleanupID != m8CleanID(3) {
				t.Fatalf("latest must be scoped to its operation: %+v", got)
			}
			// Shift the whole history together so the newest stays the newest.
			e.expire(1)
			e.expire(2)
			got := e.latest(m8InstallOp1)
			if got.Identity.CleanupID != m8CleanID(2) || got.State != domain.ProbeCleanupInterrupted || got.ErrorCode != "deadline_expired" {
				t.Fatalf("latest did not reconcile the expired newest cleanup: %+v", got)
			}
			// Read the raw row BEFORE any other reader can reconcile it again.
			e.expectRaw(2, domain.ProbeCleanupInterrupted, "deadline_expired", false)
		}},
		{"LatestTieBreaksByCleanupID", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			// Cleanup 9 is created first but carries the greater cleanup ID.
			e.create(m8InstallOp1, 9)
			e.abandon(9)
			e.create(m8InstallOp1, 2)
			e.abandon(2)
			two := e.raw(2)
			place := func(offset time.Duration) {
				t.Helper()
				if _, err := e.f.db.ExecContext(e.ctx,
					"UPDATE probe_install_cleanups SET created_at = ?, updated_at = ?, deadline_at = ? WHERE cleanup_id = ?",
					two.CreatedAt.Add(offset), two.CreatedAt.Add(offset), two.DeadlineAt.Add(offset), m8CleanID(9)); err != nil {
					t.Fatalf("place cleanup 9: %v", err)
				}
			}
			// An exact created_at tie: the greater cleanup ID is the newest, not the
			// row the engine happens to return first.
			place(0)
			if got := e.latest(m8InstallOp1); got.Identity.CleanupID != m8CleanID(9) {
				t.Fatalf("tie must break by cleanup ID descending: %+v", got)
			}
			place(-time.Second)
			if got := e.latest(m8InstallOp1); got.Identity.CleanupID != m8CleanID(2) {
				t.Fatalf("an older created_at must lose: %+v", got)
			}
			place(time.Second)
			if got := e.latest(m8InstallOp1); got.Identity.CleanupID != m8CleanID(9) {
				t.Fatalf("a newer created_at must win: %+v", got)
			}
		}},
		{"LatestOrderingSurvivesClockRollback", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.create(m8InstallOp1, 1)
			e.abandon(1)
			// The DB clock is now an hour behind the first cleanup's stamps.
			e.shift(1, time.Hour)
			first := e.raw(1)
			second := e.create(m8InstallOp1, 2)
			if second.CreatedAt.Before(first.UpdatedAt) {
				t.Fatalf("a new cleanup was stamped before an earlier one: %v < %v", second.CreatedAt, first.UpdatedAt)
			}
			if got := e.latest(m8InstallOp1); got.Identity.CleanupID != m8CleanID(2) {
				t.Fatalf("latest must be the later cleanup even after a clock rollback: %+v", got)
			}
		}},
		{"BeginReservesAttempt", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.installReady(m8InstallOp2)
			created := e.create(m8InstallOp1, 1)
			e.create(m8InstallOp2, 2)
			installBefore := m8InstallSnapshot(t, e.f, m8InstallOp1)
			cleanupBefore := e.snapshot(1)
			if c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(1), m8InstallOwnerB, m8CleanAttempt(1)); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("another owner must not begin: %+v err=%v", c, err)
			}
			if e.snapshot(1) != cleanupBefore {
				t.Fatal("refused begin modified the cleanup")
			}
			begun := e.begin(1)
			if !begun.CreatedAt.Equal(created.CreatedAt) || !begun.DeadlineAt.Equal(created.DeadlineAt) || begun.UpdatedAt.Before(created.UpdatedAt) ||
				begun.OwnerID != m8InstallOwnerA || begun.Identity != created.Identity {
				t.Fatalf("begin changed more than the lifecycle: %+v vs %+v", begun, created)
			}
			e.expectRaw(1, domain.ProbeCleanupRunning, "", true)
			if m8InstallSnapshot(t, e.f, m8InstallOp1) != installBefore {
				t.Fatal("begin modified the install journal")
			}
			// The single begin can never dispatch twice.
			if c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1)); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("second begin must conflict: %+v err=%v", c, err)
			}
			// Attempt UUIDs are unique across every cleanup.
			if c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(2), m8InstallOwnerA, m8CleanAttempt(1)); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("cross-cleanup attempt reuse must conflict: %+v err=%v", c, err)
			}
			e.expectRaw(2, domain.ProbeCleanupQueued, "", false)
			e.admissions(0)
		}},
		{"BeginRefusesWrongStateAndOwner", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.installReady(m8InstallOp2)
			e.create(m8InstallOp1, 1)
			e.abandon(1)
			e.running(m8InstallOp2, 2)
			for _, n := range []int{1, 2} {
				before := e.snapshot(n)
				if c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(n), m8InstallOwnerA, m8CleanAttempt(10+n)); c != nil || !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("cleanup %d is not queued and must not begin: %+v err=%v", n, c, err)
				}
				if e.snapshot(n) != before {
					t.Fatalf("refused begin modified cleanup %d", n)
				}
			}
		}},
		{"BeginAfterDeadlineReconciles", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.create(m8InstallOp1, 1)
			e.expire(1)
			if c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1)); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("expired cleanup must not begin: %+v err=%v", c, err)
			}
			// The reconciliation commits through the refusal.
			e.expectRaw(1, domain.ProbeCleanupInterrupted, "deadline_expired", false)
		}},
		m8CleanRecordLeaf("RecordAppliedCleans", m8InstallApplied(), domain.ProbeCleanupCleaned, "", m8InstallUnknownOutcome()),
		m8CleanRecordLeaf("RecordNotStartedFails", m8InstallNotStartedOutcome(), domain.ProbeCleanupFailed, "not_started", m8InstallApplied()),
		m8CleanRecordLeaf("RecordUnknownInterrupts", m8InstallUnknownOutcome(), domain.ProbeCleanupInterrupted, "outcome_unknown", m8InstallApplied()),
		{"RecordWrongOwnerAttemptStateConflicts", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.create(m8InstallOp1, 1)
			// A queued cleanup has no attempt to record against.
			if c, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1), m8InstallApplied()); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("queued cleanup must not record: %+v err=%v", c, err)
			}
			e.begin(1)
			before := e.snapshot(1)
			if c, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerB, m8CleanAttempt(1), m8InstallApplied()); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("another owner must not record: %+v err=%v", c, err)
			}
			if c, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(2), m8InstallApplied()); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("another attempt must not record: %+v err=%v", c, err)
			}
			if _, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1), domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied, ErrorCode: "x"}); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("a nonsensical outcome must be a validation error: %v", err)
			}
			if e.snapshot(1) != before {
				t.Fatal("refused records modified the running cleanup")
			}
		}},
		{"RecordPastDeadlineReconcilesToExpired", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.running(m8InstallOp1, 1)
			e.expire(1)
			got, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1), m8InstallApplied())
			if err != nil {
				t.Fatalf("a late report must receive the expired receipt: %v", err)
			}
			e.check(got)
			if got.State != domain.ProbeCleanupInterrupted || got.ErrorCode != "deadline_expired" || got.AttemptID != m8CleanAttempt(1) {
				t.Fatalf("late report replaced expiry uncertainty: %+v", got)
			}
			e.expectRaw(1, domain.ProbeCleanupInterrupted, "deadline_expired", true)
			// A later, different late report still reads the same expiry history.
			again, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1), m8InstallUnknownOutcome())
			if err != nil {
				t.Fatalf("second late report: %v", err)
			}
			e.same(again, got, "second late report")
			// Only the exact owner and attempt read it.
			if c, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerB, m8CleanAttempt(1), m8InstallApplied()); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("another owner must conflict: %+v err=%v", c, err)
			}
			if c, err := e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(2), m8InstallApplied()); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("another attempt must conflict: %+v err=%v", c, err)
			}
		}},
		{"AbandonQueuedInterrupts", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			created := e.create(m8InstallOp1, 1)
			got := e.abandon(1)
			if got.State != domain.ProbeCleanupInterrupted || got.ErrorCode != "owner_interrupted" || got.AttemptID != "" ||
				!got.CreatedAt.Equal(created.CreatedAt) || got.UpdatedAt.Before(created.UpdatedAt) {
				t.Fatalf("abandon outcome: %+v", got)
			}
			e.expectRaw(1, domain.ProbeCleanupInterrupted, "owner_interrupted", false)
			e.same(e.abandon(1), got, "abandon retry")
			e.same(e.observe(1), got, "observe")
			if c, err := e.cstore.BeginCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1)); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("an abandoned cleanup must not begin: %+v err=%v", c, err)
			}
			// The operation is cleanable again, only through a new cleanup.
			e.create(m8InstallOp1, 2)
		}},
		{"AbandonRefusesRunningWrongOwnerAndSettled", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			for _, op := range []string{m8InstallOp1, m8InstallOp2, m8InstallOp3} {
				e.installReady(op)
			}
			e.running(m8InstallOp1, 1)
			e.create(m8InstallOp2, 2)
			e.running(m8InstallOp3, 3)
			e.record(3, m8InstallApplied())
			for _, n := range []int{1, 3} {
				before := e.snapshot(n)
				if c, err := e.cstore.AbandonCleanup(e.ctx, m8CleanID(n), m8InstallOwnerA); c != nil || !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("abandon of cleanup %d must conflict: %+v err=%v", n, c, err)
				}
				if e.snapshot(n) != before {
					t.Fatalf("refused abandon modified cleanup %d", n)
				}
			}
			before := e.snapshot(2)
			if c, err := e.cstore.AbandonCleanup(e.ctx, m8CleanID(2), m8InstallOwnerB); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("another owner must not abandon: %+v err=%v", c, err)
			}
			if e.snapshot(2) != before {
				t.Fatal("refused abandon modified the queued cleanup")
			}
			// The running cleanup still settles normally.
			e.record(1, m8InstallApplied())
		}},
		{"AbandonExpiredQueuedReturnsExpired", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.installReady(m8InstallOp2)
			e.create(m8InstallOp1, 1)
			e.create(m8InstallOp2, 2)
			e.expire(1)
			e.expire(2)
			// Another owner conflicts, and the reconciliation still commits.
			if c, err := e.cstore.AbandonCleanup(e.ctx, m8CleanID(1), m8InstallOwnerB); c != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("another owner must conflict: %+v err=%v", c, err)
			}
			e.expectRaw(1, domain.ProbeCleanupInterrupted, "deadline_expired", false)
			got := e.abandon(2)
			if got.State != domain.ProbeCleanupInterrupted || got.ErrorCode != "deadline_expired" || got.AttemptID != "" {
				t.Fatalf("an expired queued cleanup is interrupted by its deadline: %+v", got)
			}
			e.expectRaw(2, domain.ProbeCleanupInterrupted, "deadline_expired", false)
		}},
		{"ClockAfterAuthorityLock", m8CleanClockAfterAuthorityLock},
		{"CorruptRowRefusedBeforeReconcile", func(t *testing.T, engine string) {
			for _, action := range []string{"Observe", "Latest", "Begin", "Abandon", "Create", "Record", "Admit"} {
				t.Run(action, func(t *testing.T) {
					e := newM8CleanEnv(t, engine)
					e.installReady(m8InstallOp1)
					e.create(m8InstallOp1, 1)
					wantState, attempted := domain.ProbeCleanupQueued, false
					if action == "Record" || action == "Admit" {
						e.begin(1)
						wantState, attempted = domain.ProbeCleanupRunning, true
					}
					e.expire(1)
					// The fingerprint passes the DB CHECK but not the domain contract.
					if _, err := e.f.db.ExecContext(e.ctx, "UPDATE probe_install_cleanups SET client_key_fingerprint = 'SHA256:corrupt' WHERE cleanup_id = ?", m8CleanID(1)); err != nil {
						t.Fatal(err)
					}
					var err error
					switch action {
					case "Observe":
						_, err = e.cstore.ObserveCleanup(e.ctx, m8CleanID(1))
					case "Latest":
						_, err = e.cstore.LatestCleanup(e.ctx, m8InstallOp1)
					case "Begin":
						_, err = e.cstore.BeginCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1))
					case "Abandon":
						_, err = e.cstore.AbandonCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA)
					case "Create":
						_, _, err = e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 2), m8InstallOwnerA)
					case "Record":
						_, err = e.cstore.RecordCleanup(e.ctx, m8CleanID(1), m8InstallOwnerA, m8CleanAttempt(1), m8InstallApplied())
					case "Admit":
						_, err = e.cstore.AdmitCleanupAction(e.ctx, m8CleanRequest(m8HostAuthority(t, 53), m8InstallOp1, m8InstallOwnerA, 1))
					}
					if !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("a corrupt stored row must be refused with a validation error, got %v", err)
					}
					// Nothing was reconciled or written behind the refusal.
					e.expectRaw(1, wantState, "", attempted)
					e.rows(1)
					e.admissions(0)
				})
			}
		}},
		{"UTCAndClockRollback", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			e.installReady(m8InstallOp1)
			e.create(m8InstallOp1, 1)
			// The DB clock is an hour behind the stored stamps: no write may move a
			// stamp backwards, and nothing becomes valid for longer than its deadline.
			e.shift(1, time.Hour)
			shifted := e.raw(1)
			begun := e.begin(1)
			if !begun.UpdatedAt.Equal(shifted.UpdatedAt) {
				t.Fatalf("begin moved the causal stamp under clock rollback: %v want %v", begun.UpdatedAt, shifted.UpdatedAt)
			}
			recorded := e.record(1, m8InstallApplied())
			if recorded.UpdatedAt.Before(begun.UpdatedAt) {
				t.Fatalf("record moved the causal stamp backwards: %v < %v", recorded.UpdatedAt, begun.UpdatedAt)
			}
			if got := e.raw(1); !got.CreatedAt.Equal(shifted.CreatedAt) || !got.DeadlineAt.Equal(shifted.DeadlineAt) {
				t.Fatal("a transition rewrote the immutable creation or deadline stamp")
			}
		}},
	}
}

// m8CleanClockAfterAuthorityLock proves every cleanup transition samples the DB
// clock only after it holds the shared operation authority lock: a deadline that
// expires while an unchanged authority row is held by another transaction is
// refused (or reconciled) after the wait, never accepted on a pre-lock clock
// sample. InnoDB retry cannot mask it and neither host/VM clock agreement nor
// scheduling sleeps prove expiry: the test polls the DB clock.
func m8CleanClockAfterAuthorityLock(t *testing.T, engine string) {
	for _, action := range []string{"Create", "Begin", "Record", "Observe", "Latest", "Abandon", "Admit"} {
		t.Run(action, func(t *testing.T) {
			e := newM8CleanEnv(t, engine)
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			op, owner := m8InstallOp1, m8InstallOwnerA
			e.installReady(op)
			e.create(op, 1)
			if action == "Record" || action == "Admit" {
				e.begin(1)
			}
			seed, err := e.f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = seed.Rollback() }()
			deadline := m8StageClock(t, ctx, seed, engine).Add(1200 * time.Millisecond)
			if _, err := seed.ExecContext(ctx, "UPDATE probe_install_cleanups SET created_at = ?, deadline_at = ? WHERE cleanup_id = ?", deadline.Add(-domain.ProbeCleanupLifetime), deadline, m8CleanID(1)); err != nil {
				t.Fatal(err)
			}
			if err := seed.Commit(); err != nil {
				t.Fatal(err)
			}
			blocker, err := e.f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback() }()
			if engine == "mariadb" {
				var id string
				if err := blocker.NewRaw("SELECT operation_id FROM probe_install_operations WHERE operation_id = ? FOR UPDATE", op).Scan(ctx, &id); err != nil {
					t.Fatal(err)
				}
			} else if _, err := blocker.ExecContext(ctx, "UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", op); err != nil {
				t.Fatal(err)
			}
			peerDB := m8InstallSecondDB(t, e.f, "")
			hook := &m8JournalLockEntry{entered: make(chan struct{})}
			peerDB.AddQueryHook(hook)
			peer := repository.NewProbeInstallCleanupStore(peerDB)
			type reply struct {
				cleanup *domain.ProbeCleanup
				fresh   bool
				err     error
			}
			results := make(chan reply, 1)
			joined := make(chan struct{})
			request := m8CleanRequest(m8HostAuthority(t, 53), op, owner, 1)
			go func() {
				defer close(joined)
				var r reply
				switch action {
				case "Create":
					r.cleanup, r.fresh, r.err = peer.CreateCleanup(ctx, m8CleanIdentity(op, 2), owner)
				case "Begin":
					r.cleanup, r.err = peer.BeginCleanup(ctx, m8CleanID(1), owner, m8CleanAttempt(1))
				case "Record":
					r.cleanup, r.err = peer.RecordCleanup(ctx, m8CleanID(1), owner, m8CleanAttempt(1), m8InstallApplied())
				case "Observe":
					r.cleanup, r.err = peer.ObserveCleanup(ctx, m8CleanID(1))
				case "Latest":
					r.cleanup, r.err = peer.LatestCleanup(ctx, op)
				case "Abandon":
					r.cleanup, r.err = peer.AbandonCleanup(ctx, m8CleanID(1), owner)
				case "Admit":
					_, r.err = peer.AdmitCleanupAction(ctx, request)
				}
				results <- r
			}()
			defer func() { cancel(); _ = blocker.Rollback(); <-joined }()
			select {
			case <-hook.entered:
			case r := <-results:
				t.Fatalf("operation returned before held authority lock: %v", r.err)
			case <-ctx.Done():
				t.Fatal("operation never reached authority lock")
			}
			if !m8StageClock(t, ctx, blocker, engine).Before(deadline) {
				t.Fatal("fixture did not establish a live pre-lock deadline")
			}
			for m8StageClock(t, ctx, blocker, engine).Before(deadline) {
				select {
				case r := <-results:
					t.Fatalf("operation bypassed held authority: %v", r.err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			r := <-results
			switch action {
			case "Create":
				// The expired cleanup stops blocking only because the clock was
				// sampled after the lock: the create reconciles it, then proceeds.
				if r.err != nil || !r.fresh || r.cleanup == nil || r.cleanup.Identity.CleanupID != m8CleanID(2) {
					t.Fatalf("create after the lock wait: %+v fresh=%v err=%v", r.cleanup, r.fresh, r.err)
				}
			case "Begin", "Admit":
				if !errors.Is(r.err, ports.ErrConflict) || r.cleanup != nil {
					t.Fatalf("expired authority accepted after lock wait: action=%s err=%v", action, r.err)
				}
			default:
				if r.err != nil || r.cleanup == nil || r.cleanup.State != domain.ProbeCleanupInterrupted || r.cleanup.ErrorCode != "deadline_expired" {
					t.Fatalf("expired authority was not reconciled after lock: %+v err=%v", r.cleanup, r.err)
				}
			}
			// Read raw rows BEFORE anything can reconcile again: a rolled-back
			// expiry must not be hidden by a subsequent healing read.
			e.expectRaw(1, domain.ProbeCleanupInterrupted, "deadline_expired", action == "Record" || action == "Admit")
			if action == "Admit" {
				e.admissions(0)
			}
			if action == "Create" {
				e.expectRaw(2, domain.ProbeCleanupQueued, "", false)
			}
		})
	}
}

// m8CleanAdmEnv is one isolated engine fixture with a running cleanup of the
// first fixture operation and the production host-action authorization service
// wired over the cleanup store, exactly as production wiring would.
type m8CleanAdmEnv struct {
	*m8CleanEnv
	signer *m8HostSigner
	r      domain.ProbeHostActionAdmissionRequest
	svc    *services.ProbeHostActionAuthorizationService
}

func m8CleanServiceOver(t *testing.T, store *repository.ProbeInstallCleanupStore, signer ports.ProbeHostActionGrantSigner) *services.ProbeHostActionAuthorizationService {
	t.Helper()
	return m8HostService(t, m8HostRepositoryFunc(store.AdmitCleanupAction), signer)
}

func newM8CleanAdmEnv(t *testing.T, engine string) *m8CleanAdmEnv {
	t.Helper()
	base := newM8CleanEnv(t, engine)
	signer := m8HostAuthority(t, 53)
	e := &m8CleanAdmEnv{
		m8CleanEnv: base, signer: signer,
		r:   m8CleanRequest(signer, m8InstallOp1, m8InstallOwnerA, 1),
		svc: m8CleanServiceOver(t, base.cstore, signer),
	}
	e.installReady(m8InstallOp1)
	e.running(m8InstallOp1, 1)
	return e
}

func (e *m8CleanAdmEnv) refused(grant domain.ProbeHostActionGrant, err, want error) {
	e.t.Helper()
	if grant != (domain.ProbeHostActionGrant{}) || !errors.Is(err, want) {
		e.t.Fatalf("zero grant/fixed refusal missing (match=%v err=%v)", errors.Is(err, want), err)
	}
}

func (e *m8CleanAdmEnv) granted(grant domain.ProbeHostActionGrant, err error, r domain.ProbeHostActionAdmissionRequest) {
	e.t.Helper()
	if err != nil || grant.Request != r || !grant.ValidFor(e.signer.PublicKey()) {
		e.t.Fatalf("expected exact cryptographically verified cleanup grant: %v", err)
	}
}

func (e *m8CleanAdmEnv) noSigning() {
	e.t.Helper()
	if e.signer.calls.Load() != 0 {
		e.t.Fatal("refused admission was signed")
	}
}

// refusedDirect admits through the store and requires a zero admission, the
// wanted fixed error and no durable change to the cleanup or its admissions.
func (e *m8CleanAdmEnv) refusedDirect(what string, r domain.ProbeHostActionAdmissionRequest, want error) {
	e.t.Helper()
	before, rows := e.snapshot(1), m8ActCount(e.t, e.f, "probe_cleanup_admissions")
	a, err := e.cstore.AdmitCleanupAction(e.ctx, r)
	if a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, want) {
		e.t.Fatalf("%s must be refused with %v: admission=%+v err=%v", what, want, a, err)
	}
	if got := m8ActCount(e.t, e.f, "probe_cleanup_admissions"); got != rows || e.snapshot(1) != before {
		e.t.Fatalf("%s left durable residue", what)
	}
}

type m8CleanAdmLeaf struct {
	name string
	run  func(e *m8CleanAdmEnv)
}

func m8CleanAdmissionLeaves() []m8CleanLeaf {
	admissions := m8CleanAdmLeaves()
	leaves := make([]m8CleanLeaf, 0, len(admissions))
	for _, leaf := range admissions {
		leaves = append(leaves, m8CleanLeaf{leaf.name, func(t *testing.T, engine string) { leaf.run(newM8CleanAdmEnv(t, engine)) }})
	}
	return leaves
}

func m8CleanAdmLeaves() []m8CleanAdmLeaf {
	return []m8CleanAdmLeaf{
		{"AdmitGrantsVerifiedSignature", func(e *m8CleanAdmEnv) {
			before := e.raw(1)
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			if g.ValidFor(m8HostAuthority(e.t, 54).PublicKey()) {
				e.t.Fatal("grant verified under a foreign authority")
			}
			e.admissions(1)
			after := e.raw(1)
			if after.State != domain.ProbeCleanupRunning || after.AttemptID == nil || *after.AttemptID != m8CleanAttempt(1) ||
				!after.CreatedAt.Equal(before.CreatedAt) || !after.DeadlineAt.Equal(before.DeadlineAt) {
				e.t.Fatal("admission fabricated an outcome or rebound the attempt")
			}
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("exactly one signature expected")
			}
		}},
		{"AdmitStoreRecordsExactRow", func(e *m8CleanAdmEnv) {
			installBefore := m8InstallSnapshot(e.t, e.f, m8InstallOp1)
			a, err := e.cstore.AdmitCleanupAction(e.ctx, e.r)
			if err != nil || a.Request != e.r || !a.Valid() {
				e.t.Fatalf("direct admission refused: %v", err)
			}
			if a.AdmittedAt.Location() != time.UTC || a.AdmittedAt.Nanosecond()%1000 != 0 {
				e.t.Fatal("admission stamp is not UTC microseconds")
			}
			rows := e.rawAdmissions()
			if len(rows) != 1 {
				e.t.Fatalf("expected one admission row, got %d", len(rows))
			}
			row, intent := rows[0], e.r.Intent
			if row.AttemptID != intent.AttemptID || row.CleanupID != m8CleanID(1) || row.OwnerID != intent.OwnerID ||
				row.AuthorityKeyID != e.r.Challenge.AuthorityKeyID || row.NonceHex != hex.EncodeToString(e.r.Challenge.Nonce[:]) ||
				row.MarkerSHA256 != intent.MarkerSHA256() || !row.AdmittedAt.Equal(a.AdmittedAt) {
				e.t.Fatal("stored admission differs from the returned admission")
			}
			if c := e.raw(1); !c.UpdatedAt.Equal(a.AdmittedAt) || c.State != domain.ProbeCleanupRunning {
				e.t.Fatal("cleanup causal stamp was not advanced to the admission stamp")
			}
			// The cleanup admission never touches the install journal or any other
			// admission store.
			if m8InstallSnapshot(e.t, e.f, m8InstallOp1) != installBefore {
				e.t.Fatal("cleanup admission modified the install journal")
			}
			for _, table := range []string{"probe_host_action_admissions", "probe_activation_admissions", "probe_binary_stage_admissions"} {
				if m8ActCount(e.t, e.f, table) != 0 {
					e.t.Fatalf("cleanup admission wrote %s", table)
				}
			}
			// The receipt stays a valid cleanup after the causal stamp moved.
			e.observe(1)
		}},
		{"AdmitOnlyRunningCleanup", func(e *m8CleanAdmEnv) {
			// Cleanup 1 is running: settle it, then every non-running shape of
			// cleanup (queued, abandoned, cleaned, failed, interrupted) refuses.
			e.record(1, m8InstallApplied())
			for i, op := range []string{m8InstallOp2, m8InstallOp3, m8InstallOp4, m8InstallOp5} {
				e.installReady(op)
				n := i + 2
				e.create(op, n)
				switch n {
				case 3:
					e.abandon(n)
				case 4:
					e.begin(n)
					e.record(n, m8InstallNotStartedOutcome())
				case 5:
					e.begin(n)
					e.record(n, m8InstallUnknownOutcome())
				}
			}
			for n, op := range map[int]string{1: m8InstallOp1, 2: m8InstallOp2, 3: m8InstallOp3, 4: m8InstallOp4, 5: m8InstallOp5} {
				r := m8CleanRequest(e.signer, op, m8InstallOwnerA, n)
				before := e.snapshot(n)
				if a, err := e.cstore.AdmitCleanupAction(e.ctx, r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrConflict) {
					e.t.Fatalf("cleanup %d is not running and must not be admitted: %+v err=%v", n, a, err)
				}
				if e.snapshot(n) != before {
					e.t.Fatalf("refused admission modified cleanup %d", n)
				}
			}
			e.admissions(0)
		}},
		{"AdmitExactReplayConflict", func(e *m8CleanAdmEnv) {
			if _, err := e.cstore.AdmitCleanupAction(e.ctx, e.r); err != nil {
				e.t.Fatal(err)
			}
			e.refusedDirect("exact replay", e.r, ports.ErrConflict)
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.noSigning()
			e.admissions(1)
		}},
		{"AdmitDifferentChallengeNoRearm", func(e *m8CleanAdmEnv) {
			if _, err := e.cstore.AdmitCleanupAction(e.ctx, e.r); err != nil {
				e.t.Fatal(err)
			}
			again := e.r
			again.Challenge.Nonce = [32]byte{7}
			g, err := e.svc.Authorize(e.ctx, again)
			e.refused(g, err, ports.ErrConflict)
			e.noSigning()
			e.admissions(1)
		}},
		{"AdmitNonceReuseAcrossCleanups", func(e *m8CleanAdmEnv) {
			if _, err := e.cstore.AdmitCleanupAction(e.ctx, e.r); err != nil {
				e.t.Fatal(err)
			}
			// A second operation's running cleanup presents the very same
			// authority nonce: consumed nonces are never reusable.
			e.installReady(m8InstallOp2)
			e.running(m8InstallOp2, 2)
			other := m8CleanRequest(e.signer, m8InstallOp2, m8InstallOwnerA, 2)
			if other.Challenge.Nonce != e.r.Challenge.Nonce || other.Challenge.AuthorityKeyID != e.r.Challenge.AuthorityKeyID {
				e.t.Fatal("fixture must reuse the authority nonce")
			}
			before := e.snapshot(2)
			if a, err := e.cstore.AdmitCleanupAction(e.ctx, other); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrConflict) {
				e.t.Fatalf("a reused nonce must be refused: %+v err=%v", a, err)
			}
			if e.snapshot(2) != before {
				e.t.Fatal("a refused nonce reuse modified the second cleanup")
			}
			e.admissions(1)
			// With a fresh nonce the second cleanup is admitted independently.
			other.Challenge.Nonce = [32]byte{9}
			if _, err := e.cstore.AdmitCleanupAction(e.ctx, other); err != nil {
				e.t.Fatalf("a fresh nonce must be admitted: %v", err)
			}
			e.admissions(2)
		}},
		{"AdmitWrongKind", func(e *m8CleanAdmEnv) {
			install := m8CleanIdentity(m8InstallOp1, 1).Install
			for _, kind := range []string{domain.ProbeHostActionDataDirectory, domain.ProbeHostActionUnitFile, domain.ProbeHostActionService,
				domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart} {
				intent := domain.ProbeHostActionIntent{Identity: install, OwnerID: m8InstallOwnerA, AttemptID: m8CleanAttempt(1), Kind: kind}
				r := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: e.r.Challenge}
				r.Challenge.MarkerSHA256 = intent.MarkerSHA256()
				if !r.Valid() {
					e.t.Fatalf("fixture request for %s must be a valid admission request", kind)
				}
				e.refusedDirect("kind "+kind, r, domain.ErrValidation)
			}
		}},
		{"AdmitInvalidRequestRefused", func(e *m8CleanAdmEnv) {
			bad := e.r
			bad.Challenge.MarkerSHA256 = strings.Repeat("0", 64)
			e.refusedDirect("a marker that does not bind the intent", bad, domain.ErrValidation)
			var nilStore *repository.ProbeInstallCleanupStore
			if a, err := nilStore.AdmitCleanupAction(e.ctx, e.r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrInternal) {
				e.t.Fatalf("nil store must refuse: %+v err=%v", a, err)
			}
		}},
		{"AdmitIdentityMismatch", func(e *m8CleanAdmEnv) {
			intent := e.r.Intent
			intent.Identity.Binary.Version = "9.9.9"
			r := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: e.r.Challenge}
			r.Challenge.MarkerSHA256 = intent.MarkerSHA256()
			e.refusedDirect("a different install identity", r, ports.ErrConflict)
		}},
		{"AdmitWrongOwner", func(e *m8CleanAdmEnv) {
			e.refusedDirect("another owner", m8CleanRequest(e.signer, m8InstallOp1, m8InstallOwnerB, 1), ports.ErrConflict)
		}},
		{"AdmitWrongAttempt", func(e *m8CleanAdmEnv) {
			e.refusedDirect("an unreserved attempt", m8CleanRequest(e.signer, m8InstallOp1, m8InstallOwnerA, 7), ports.ErrConflict)
		}},
		{"AdmitAttemptOfAnotherOperation", func(e *m8CleanAdmEnv) {
			e.installReady(m8InstallOp2)
			e.running(m8InstallOp2, 2)
			// Operation 1's identity naming operation 2's reserved attempt.
			intent := m8CleanIdentity(m8InstallOp1, 1).Intent(m8InstallOwnerA, m8CleanAttempt(2))
			r := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: e.r.Challenge}
			r.Challenge.MarkerSHA256 = intent.MarkerSHA256()
			before := e.snapshot(2)
			e.refusedDirect("a foreign attempt", r, ports.ErrConflict)
			if e.snapshot(2) != before {
				e.t.Fatal("the foreign attempt's cleanup was modified")
			}
		}},
		{"AdmitUnknownOperationNotFound", func(e *m8CleanAdmEnv) {
			e.refusedDirect("an unknown operation", m8CleanRequest(e.signer, m8InstallOp9, m8InstallOwnerA, 1), ports.ErrNotFound)
		}},
		{"AdmitExpiredDeadlineDBClock", func(e *m8CleanAdmEnv) {
			e.expire(1)
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.noSigning()
			e.admissions(0)
			// Honest cold expiry, committed through the refusal.
			e.expectRaw(1, domain.ProbeCleanupInterrupted, "deadline_expired", true)
		}},
		{"AdmitCanceledContextRefused", func(e *m8CleanAdmEnv) {
			ctx, cancel := context.WithCancel(e.ctx)
			cancel()
			a, err := e.cstore.AdmitCleanupAction(ctx, e.r)
			if a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, context.Canceled) {
				e.t.Fatalf("canceled context must be refused: %+v err=%v", a, err)
			}
			e.admissions(0)
		}},
		{"AdmitConcurrentSingleGrant", func(e *m8CleanAdmEnv) {
			peer := m8InstallSecondDB(e.t, e.f, "")
			instances := []*services.ProbeHostActionAuthorizationService{e.svc, m8CleanServiceOver(e.t, repository.NewProbeInstallCleanupStore(peer), e.signer)}
			start := make(chan struct{})
			results := make(chan m8HostResult, len(instances))
			var wg sync.WaitGroup
			for _, instance := range instances {
				wg.Go(func() { <-start; g, err := instance.Authorize(e.ctx, e.r); results <- m8HostResult{g, err} })
			}
			e.t.Cleanup(func() { e.cancel(); wg.Wait() })
			close(start)
			success := 0
			for range instances {
				got := <-results
				if got.err == nil {
					e.granted(got.grant, nil, e.r)
					success++
				} else {
					e.refused(got.grant, got.err, ports.ErrConflict)
				}
			}
			wg.Wait()
			if success != 1 || e.signer.calls.Load() != 1 {
				e.t.Fatal("concurrent attempts did not yield exactly one grant")
			}
			e.admissions(1)
		}},
		{"AdmitSignFailureConsumesAdmission", func(e *m8CleanAdmEnv) {
			e.signer.before = func(context.Context, domain.ProbeHostActionAdmission) error {
				return errors.New("private fixture signing diagnostic")
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, domain.ErrInternal)
			e.admissions(1)
			e.signer.before = nil
			g, err = e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("sign failure rearmed the admission")
			}
			e.expectRaw(1, domain.ProbeCleanupRunning, "", true)
		}},
		{"AdmitIndependentReaderSeesAdmissionBeforeSign", func(e *m8CleanAdmEnv) {
			peer := m8InstallSecondDB(e.t, e.f, "")
			observed := false
			e.signer.before = func(call context.Context, a domain.ProbeHostActionAdmission) error {
				var nonce, marker, attempt, cleanup string
				if err := peer.NewRaw("SELECT nonce_hex, marker_sha256, attempt_id, cleanup_id FROM probe_cleanup_admissions WHERE attempt_id = ?", e.r.Intent.AttemptID).Scan(call, &nonce, &marker, &attempt, &cleanup); err != nil {
					return domain.ErrInternal
				}
				if nonce != hex.EncodeToString(e.r.Challenge.Nonce[:]) || marker != e.r.Intent.MarkerSHA256() || attempt != e.r.Intent.AttemptID ||
					cleanup != m8CleanID(1) || a.Request != e.r {
					return domain.ErrInternal
				}
				observed = true
				return nil
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			if !observed {
				e.t.Fatal("the admission was not durably visible before signing")
			}
		}},
		{"AdmitLostAckNoSign", func(e *m8CleanAdmEnv) { e.postCommitLoss(false) }},
		{"AdmitCallerCanceledAfterCommitNoGrant", func(e *m8CleanAdmEnv) { e.postCommitLoss(true) }},
		{"AdmitClockRollbackStamp", func(e *m8CleanAdmEnv) {
			// The DB clock is an hour behind the cleanup's stored stamps: the
			// admission stamp may never precede them.
			e.shift(1, time.Hour)
			stored := e.raw(1)
			a, err := e.cstore.AdmitCleanupAction(e.ctx, e.r)
			if err != nil {
				e.t.Fatalf("admission under clock rollback: %v", err)
			}
			if a.AdmittedAt.Before(stored.UpdatedAt) || !a.AdmittedAt.Equal(e.raw(1).UpdatedAt) {
				e.t.Fatalf("admission stamp %v precedes the stored causal stamp %v", a.AdmittedAt, stored.UpdatedAt)
			}
			e.observe(1)
		}},
		{"AdmitAuthorizationLifecycle", func(e *m8CleanAdmEnv) {
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			cleaned := e.record(1, m8InstallApplied())
			if cleaned.State != domain.ProbeCleanupCleaned {
				e.t.Fatalf("a granted cleanup must settle: %+v", cleaned)
			}
			// The settled cleanup and its consumed admission can never grant again,
			// not even to a cold process on an independent connection set.
			again, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(again, err, ports.ErrConflict)
			cold := m8CleanServiceOver(e.t, repository.NewProbeInstallCleanupStore(m8InstallSecondDB(e.t, e.f, "")), e.signer)
			again, err = cold.Authorize(e.t.Context(), e.r)
			e.refused(again, err, ports.ErrConflict)
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("the lifecycle signed more than once")
			}
			e.admissions(1)
			// A later cleanup of the operation is refused: it is already cleaned.
			if c, created, err := e.cstore.CreateCleanup(e.ctx, m8CleanIdentity(m8InstallOp1, 2), m8InstallOwnerA); created || c != nil {
				e.t.Fatal("a cleaned operation accepted another cleanup")
			} else {
				e.ineligible("cleaned operation", err)
			}
		}},
		{"AdmitSeparationFromOtherStores", func(e *m8CleanAdmEnv) {
			// Cleanup intents are consumable only here: the activation store and
			// the install journal's host-action store both refuse them.
			if a, err := e.store.AdmitActivationAction(e.ctx, e.r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
				e.t.Fatalf("activation store must refuse a cleanup intent: %+v err=%v", a, err)
			}
			installAdmissions := repository.NewProbeHostActionAdmissionStore(e.f.db)
			if a, err := installAdmissions.AdmitHostAction(e.ctx, e.r); a != (domain.ProbeHostActionAdmission{}) || err == nil {
				e.t.Fatalf("install host-action store must refuse a cleanup intent: %+v err=%v", a, err)
			}
			for _, table := range []string{"probe_host_action_admissions", "probe_activation_admissions", "probe_binary_stage_admissions", "probe_cleanup_admissions"} {
				if m8ActCount(e.t, e.f, table) != 0 {
					e.t.Fatalf("a refused cleanup intent wrote %s", table)
				}
			}
			// And the cleanup store refuses every install and activation intent.
			install := m8CleanIdentity(m8InstallOp1, 1).Install
			intent := domain.ProbeHostActionIntent{Identity: install, OwnerID: m8InstallOwnerA, AttemptID: m8CleanAttempt(1), Kind: domain.ProbeActivationProbeInit}
			r := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: e.r.Challenge}
			r.Challenge.MarkerSHA256 = intent.MarkerSHA256()
			e.refusedDirect("an activation intent", r, domain.ErrValidation)
		}},
	}
}

// postCommitLoss decorates the real store so the commit is observed from an
// independent connection, then loses the acknowledgment or the caller context.
func (e *m8CleanAdmEnv) postCommitLoss(cancelCaller bool) {
	e.t.Helper()
	peer := m8InstallSecondDB(e.t, e.f, "")
	observed := false
	callerCtx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	decorated := m8HostRepositoryFunc(func(call context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
		a, err := e.cstore.AdmitCleanupAction(call, request)
		if err != nil {
			return domain.ProbeHostActionAdmission{}, err
		}
		count, readErr := peer.NewSelect().Table("probe_cleanup_admissions").Count(e.ctx)
		if readErr != nil || count != 1 {
			return domain.ProbeHostActionAdmission{}, domain.ErrInternal
		}
		observed = true
		if cancelCaller {
			cancel()
			return a, nil
		}
		return domain.ProbeHostActionAdmission{}, domain.ErrInternal
	})
	g, err := m8HostService(e.t, decorated, e.signer).Authorize(callerCtx, e.r)
	want := domain.ErrInternal
	if cancelCaller {
		want = context.Canceled
	}
	e.refused(g, err, want)
	if !observed || e.signer.calls.Load() != 0 {
		e.t.Fatal("lost/canceled postcommit acknowledgment signed")
	}
	e.admissions(1)
	cold := m8CleanServiceOver(e.t, repository.NewProbeInstallCleanupStore(peer), e.signer)
	g, err = cold.Authorize(e.t.Context(), e.r)
	e.refused(g, err, ports.ErrConflict)
}

func m8CleanSchemaLeaves() []m8CleanLeaf {
	return []m8CleanLeaf{
		{"SchemaConstraints", m8CleanSchemaConstraints},
		{"NoSecretColumns", func(t *testing.T, engine string) {
			e := newM8CleanEnv(t, engine)
			columns := func(table string) []string {
				rows, err := e.f.db.DB.QueryContext(e.ctx, "SELECT * FROM "+table+" LIMIT 0")
				if err != nil {
					t.Fatalf("inspect %s: %v", table, err)
				}
				defer func() { _ = rows.Close() }()
				names, err := rows.Columns()
				if err != nil {
					t.Fatalf("columns of %s: %v", table, err)
				}
				slices.Sort(names)
				return names
			}
			// The deployment private key, key paths and remote output have no
			// column by construction: the stored columns are exactly these.
			wantCleanups := []string{"attempt_id", "cleanup_id", "client_key_fingerprint", "created_at", "deadline_at", "error_code", "operation_id", "owner_id", "requested_by", "state", "updated_at"}
			wantAdmissions := []string{"admitted_at", "attempt_id", "authority_key_id", "cleanup_id", "marker_sha256", "nonce_hex", "owner_id"}
			if got := columns("probe_install_cleanups"); !slices.Equal(got, wantCleanups) {
				t.Fatalf("cleanup columns changed: %v", got)
			}
			if got := columns("probe_cleanup_admissions"); !slices.Equal(got, wantAdmissions) {
				t.Fatalf("cleanup admission columns changed: %v", got)
			}
		}},
		{"DowngradeGuard", func(t *testing.T, engine string) {
			const migration = "088_probe_install_cleanups"
			e := newM8CleanEnv(t, engine)
			ctx := t.Context()
			t.Cleanup(func() {
				// Restore the latest schema (idempotent) for the shared MariaDB
				// database, and drop a guard table a refused downgrade left behind.
				_, _ = e.f.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS probe_install_cleanups_downgrade_guard")
				if err := runEngineMigration(t, e.f.db, engine, migration, "up"); err != nil {
					t.Errorf("restore the cleanup schema: %v", err)
				}
			})
			// 088 is the newest migration and nothing references it, so it is the
			// first one a rollback would remove. Empty, it goes away cleanly.
			if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err != nil {
				t.Fatalf("empty cleanup downgrade refused: %v", err)
			}
			for _, table := range []string{"probe_install_cleanups", "probe_cleanup_admissions"} {
				if _, err := e.f.db.NewSelect().Table(table).Count(ctx); err == nil {
					t.Fatalf("empty downgrade did not remove %s", table)
				}
			}
			if err := runEngineMigration(t, e.f.db, engine, migration, "up"); err != nil {
				t.Fatalf("cleanup re-upgrade failed: %v", err)
			}
			// Even one settled cleanup is retained evidence: refuse.
			e.installReady(m8InstallOp1)
			e.create(m8InstallOp1, 1)
			if got := e.abandon(1); !got.Terminal() {
				t.Fatalf("fixture cleanup must be terminal: %+v", got)
			}
			if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err == nil {
				t.Fatal("downgrade erased a settled cleanup")
			}
			// A running cleanup with an irreversible admission refuses too.
			e.installReady(m8InstallOp2)
			e.running(m8InstallOp2, 2)
			signer := m8HostAuthority(t, 53)
			request := m8CleanRequest(signer, m8InstallOp2, m8InstallOwnerA, 2)
			g, err := m8CleanServiceOver(t, e.cstore, signer).Authorize(ctx, request)
			if err != nil || !g.ValidFor(signer.PublicKey()) {
				t.Fatalf("fixture admission refused: %v", err)
			}
			if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err == nil {
				t.Fatal("downgrade erased an irreversible admission")
			}
			// Consumption and history survive the refused downgrade.
			e.admissions(1)
			again, err := m8CleanServiceOver(t, e.cstore, signer).Authorize(ctx, request)
			if err == nil || again != (domain.ProbeHostActionGrant{}) || !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("consumption did not survive the refused downgrade: %v", err)
			}
			if got := e.observe(1); got.State != domain.ProbeCleanupInterrupted || got.ErrorCode != "owner_interrupted" {
				t.Fatalf("history changed by the refused downgrade: %+v", got)
			}
			// Rows can only be removed child-first, and only by an explicit,
			// out-of-band cleanup, never by the migration itself.
			for _, table := range []string{"probe_cleanup_admissions", "probe_install_cleanups"} {
				if _, err := e.f.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
					t.Fatalf("explicit cleanup of %s: %v", table, err)
				}
			}
			if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err != nil {
				t.Fatalf("downgrade of an emptied schema refused: %v", err)
			}
			if err := runEngineMigration(t, e.f.db, engine, migration, "up"); err != nil {
				t.Fatalf("restore of the cleanup schema failed: %v", err)
			}
		}},
	}
}

// m8CleanSchemaConstraints covers the per-engine contracts that are not specific
// to one store operation: the schema CHECKs (encoding domain.ProbeCleanup.Valid()
// exactly), the unique attempt and nonce keys, the composite admission key and
// the restricting foreign keys.
func m8CleanSchemaConstraints(t *testing.T, engine string) {
	e := newM8CleanEnv(t, engine)
	ctx := t.Context()
	for _, op := range []string{m8InstallOp1, m8InstallOp2, m8InstallOp3} {
		e.installReady(op)
	}
	created := time.Now().UTC().Truncate(time.Microsecond)
	deadline := created.Add(domain.ProbeCleanupLifetime)
	columns := []string{"cleanup_id", "operation_id", "client_key_fingerprint", "requested_by", "owner_id", "state", "error_code", "attempt_id", "created_at", "updated_at", "deadline_at"}
	base := map[string]any{
		"cleanup_id": m8CleanID(1), "operation_id": m8InstallOp1, "client_key_fingerprint": m8InstallFingerprint('c'),
		"requested_by": int64(4242), "owner_id": m8InstallOwnerA, "state": "queued", "error_code": nil, "attempt_id": nil,
		"created_at": created, "updated_at": created, "deadline_at": deadline,
	}
	insert := func(override map[string]any) error {
		values := make([]any, 0, len(columns))
		for _, c := range columns {
			v := base[c]
			if o, ok := override[c]; ok {
				v = o
			}
			values = append(values, v)
		}
		_, err := e.f.db.ExecContext(ctx, "INSERT INTO probe_install_cleanups ("+strings.Join(columns, ", ")+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", values...)
		return err
	}
	attempt := m8CleanAttempt
	bad := map[string]map[string]any{
		"missing_install":                 {"operation_id": m8InstallOp9},
		"cleanup_id_invalid":              {"cleanup_id": "x"},
		"cleanup_id_zero":                 {"cleanup_id": "00000000-0000-0000-0000-000000000000"},
		"operation_id_invalid":            {"operation_id": "x"},
		"client_key":                      {"client_key_fingerprint": "bad"},
		"actor":                           {"requested_by": int64(0)},
		"owner_invalid":                   {"owner_id": "invalid"},
		"owner_zero":                      {"owner_id": "00000000-0000-0000-0000-000000000000"},
		"state_unknown":                   {"state": "bogus"},
		"state_install_state":             {"state": "resources_ready"},
		"state_activation_state":          {"state": "active"},
		"queued_with_error":               {"error_code": "not_started"},
		"queued_with_attempt":             {"attempt_id": attempt(1)},
		"queued_not_live":                 {"updated_at": deadline},
		"running_no_attempt":              {"state": "running"},
		"running_with_error":              {"state": "running", "attempt_id": attempt(1), "error_code": "outcome_unknown"},
		"cleaned_no_attempt":              {"state": "cleaned"},
		"cleaned_with_error":              {"state": "cleaned", "attempt_id": attempt(1), "error_code": "not_started"},
		"failed_no_error":                 {"state": "failed", "attempt_id": attempt(1)},
		"failed_wrong_code":               {"state": "failed", "attempt_id": attempt(1), "error_code": "outcome_unknown"},
		"failed_no_attempt":               {"state": "failed", "error_code": "not_started"},
		"interrupted_no_error":            {"state": "interrupted"},
		"interrupted_unknown_code":        {"state": "interrupted", "error_code": "bogus"},
		"interrupted_owner_with_attempt":  {"state": "interrupted", "error_code": "owner_interrupted", "attempt_id": attempt(1)},
		"interrupted_outcome_no_attempt":  {"state": "interrupted", "error_code": "outcome_unknown"},
		"interrupted_not_started":         {"state": "interrupted", "error_code": "not_started", "attempt_id": attempt(1)},
		"interrupted_expired_but_live":    {"state": "interrupted", "error_code": "deadline_expired"},
		"interrupted_owner_not_live":      {"state": "interrupted", "error_code": "owner_interrupted", "updated_at": deadline},
		"interrupted_outcome_not_live":    {"state": "interrupted", "error_code": "outcome_unknown", "attempt_id": attempt(1), "updated_at": deadline},
		"attempt_short":                   {"state": "running", "attempt_id": "short"},
		"attempt_zero":                    {"state": "running", "attempt_id": "00000000-0000-0000-0000-000000000000"},
		"deadline_not_after":              {"deadline_at": created},
		"updated_before_created":          {"updated_at": created.Add(-time.Second)},
		"running_not_live":                {"state": "running", "attempt_id": attempt(1), "updated_at": deadline},
		"cleaned_not_live":                {"state": "cleaned", "attempt_id": attempt(1), "updated_at": deadline},
		"failed_not_live":                 {"state": "failed", "attempt_id": attempt(1), "error_code": "not_started", "updated_at": deadline},
		"interrupted_expired_updated_old": {"state": "interrupted", "error_code": "deadline_expired", "attempt_id": attempt(1), "updated_at": created},
	}
	if engine == "mariadb" {
		// MariaDB additionally pins the exact fixed lifetime; SQLite cannot do
		// microsecond arithmetic on its stored text stamps, and the store's
		// receipt validation fails closed on such a row there.
		bad["deadline_not_fixed_lifetime"] = map[string]any{"deadline_at": created.Add(2 * domain.ProbeCleanupLifetime)}
	}
	for name, override := range bad {
		if err := insert(override); err == nil {
			t.Fatalf("schema accepted a noncanonical cleanup row (%s)", name)
		}
		if n := m8ActCount(t, e.f, "probe_install_cleanups"); n != 0 {
			t.Fatalf("rejected insert %s left %d rows", name, n)
		}
	}
	// Positive controls for every shape the store writes.
	controls := []map[string]any{
		nil,
		{"cleanup_id": m8CleanID(2)}, // another cleanup of the same operation
		{"cleanup_id": m8CleanID(3), "operation_id": m8InstallOp2, "state": "running", "attempt_id": attempt(3)},
		{"cleanup_id": m8CleanID(4), "operation_id": m8InstallOp2, "state": "cleaned", "attempt_id": attempt(4)},
		{"cleanup_id": m8CleanID(5), "operation_id": m8InstallOp3, "state": "failed", "attempt_id": attempt(5), "error_code": "not_started"},
		{"cleanup_id": m8CleanID(6), "operation_id": m8InstallOp3, "state": "interrupted", "error_code": "owner_interrupted"},
		{"cleanup_id": m8CleanID(7), "operation_id": m8InstallOp3, "state": "interrupted", "error_code": "deadline_expired", "updated_at": deadline},
		{"cleanup_id": m8CleanID(8), "operation_id": m8InstallOp3, "state": "interrupted", "error_code": "deadline_expired", "attempt_id": attempt(8), "updated_at": deadline},
		{"cleanup_id": m8CleanID(10), "operation_id": m8InstallOp3, "state": "interrupted", "error_code": "outcome_unknown", "attempt_id": attempt(10)},
	}
	for i, row := range controls {
		if err := insert(row); err != nil {
			t.Fatalf("control cleanup %d refused: %v", i, err)
		}
	}
	if err := insert(nil); err == nil {
		t.Fatal("schema accepted a duplicate cleanup ID")
	}
	if err := insert(map[string]any{"cleanup_id": m8CleanID(9), "operation_id": m8InstallOp3, "state": "running", "attempt_id": attempt(3)}); err == nil {
		t.Fatal("schema accepted a reused attempt across cleanups")
	}
	if n := m8ActCount(t, e.f, "probe_install_cleanups"); n != int64(len(controls)) {
		t.Fatalf("expected %d control cleanups, got %d", len(controls), n)
	}
	// Admissions.
	admissionColumns := []string{"attempt_id", "cleanup_id", "owner_id", "authority_key_id", "nonce_hex", "marker_sha256", "admitted_at"}
	nonce := hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
	admissionBase := map[string]any{
		"attempt_id": attempt(3), "cleanup_id": m8CleanID(3), "owner_id": m8InstallOwnerA,
		"authority_key_id": strings.Repeat("a", 64), "nonce_hex": nonce, "marker_sha256": strings.Repeat("c", 64), "admitted_at": created,
	}
	insertAdmission := func(override map[string]any) error {
		values := make([]any, 0, len(admissionColumns))
		for _, c := range admissionColumns {
			v := admissionBase[c]
			if o, ok := override[c]; ok {
				v = o
			}
			values = append(values, v)
		}
		_, err := e.f.db.ExecContext(ctx, "INSERT INTO probe_cleanup_admissions ("+strings.Join(admissionColumns, ", ")+") VALUES (?, ?, ?, ?, ?, ?, ?)", values...)
		return err
	}
	badAdmissions := map[string]map[string]any{
		"attempt_invalid":          {"attempt_id": "x"},
		"attempt_zero":             {"attempt_id": "00000000-0000-0000-0000-000000000000"},
		"cleanup_invalid":          {"cleanup_id": "x"},
		"owner_invalid":            {"owner_id": "invalid"},
		"owner_zero":               {"owner_id": "00000000-0000-0000-0000-000000000000"},
		"authority_upper":          {"authority_key_id": strings.Repeat("A", 64)},
		"authority_short":          {"authority_key_id": strings.Repeat("a", 63)},
		"nonce_zero":               {"nonce_hex": strings.Repeat("0", 64)},
		"nonce_upper":              {"nonce_hex": strings.Repeat("A", 64)},
		"marker":                   {"marker_sha256": "bad"},
		"missing_cleanup":          {"cleanup_id": m8CleanID(99)},
		"wrong_owner":              {"owner_id": m8InstallOwnerB},
		"queued_cleanup_attempt":   {"attempt_id": attempt(1), "cleanup_id": m8CleanID(1)},
		"attempt_of_other_cleanup": {"attempt_id": attempt(4)},
	}
	for name, override := range badAdmissions {
		if err := insertAdmission(override); err == nil {
			t.Fatalf("schema accepted noncanonical admission metadata (%s)", name)
		}
	}
	if n := m8ActCount(t, e.f, "probe_cleanup_admissions"); n != 0 {
		t.Fatalf("rejected admission inserts left %d rows", n)
	}
	if err := insertAdmission(nil); err != nil {
		t.Fatalf("control admission refused: %v", err)
	}
	if err := insertAdmission(nil); err == nil {
		t.Fatal("schema accepted a replayed admission")
	}
	// Each uniqueness key in isolation: another cleanup that reserved its own
	// attempt cannot reuse the consumed authority nonce.
	second := map[string]any{"attempt_id": attempt(4), "cleanup_id": m8CleanID(4)}
	if err := insertAdmission(second); err == nil {
		t.Fatal("schema accepted a reused authority nonce")
	}
	second["nonce_hex"] = hex.EncodeToString(bytes.Repeat([]byte{2}, 32))
	if err := insertAdmission(second); err != nil {
		t.Fatalf("control admission with a fresh nonce refused: %v", err)
	}
	if n := m8ActCount(t, e.f, "probe_cleanup_admissions"); n != 2 {
		t.Fatalf("expected two admissions, got %d", n)
	}
	// The restricting keys: a consumed admission pins its cleanup's attempt and
	// owner, and the install journal's row is pinned by its cleanups.
	for name, statement := range map[string]string{
		"delete_consumed_cleanup": "DELETE FROM probe_install_cleanups WHERE cleanup_id = '" + m8CleanID(3) + "'",
		"rebind_attempt":          "UPDATE probe_install_cleanups SET attempt_id = '" + attempt(9) + "' WHERE cleanup_id = '" + m8CleanID(3) + "'",
		"rebind_owner":            "UPDATE probe_install_cleanups SET owner_id = '" + m8InstallOwnerB + "' WHERE cleanup_id = '" + m8CleanID(3) + "'",
		"delete_install":          "DELETE FROM probe_install_operations WHERE operation_id = '" + m8InstallOp1 + "'",
	} {
		if _, err := e.f.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("the schema allowed %s", name)
		}
	}
	if n := m8ActCount(t, e.f, "probe_cleanup_admissions"); n != 2 {
		t.Fatalf("restricted statements changed admissions: %d", n)
	}
}
