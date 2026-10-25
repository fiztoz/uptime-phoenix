package repository_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"slices"
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

// m8HostLayout is the frozen root/systemd resource order. A host action's layout
// index is its position here; its ordinal is index+1.
var m8HostLayout = []string{"data_directory", "artifact_directory", "unit_file", "service"}

var m8HostKinds = []string{domain.ProbeHostActionDataDirectory, domain.ProbeHostActionUnitFile, domain.ProbeHostActionService}

// m8HostAttempt returns a journal-unique attempt UUID per operation and kind.
// Attempt UUIDs are global across the whole journal, so the two fixture
// operations use disjoint sets.
func m8HostAttempt(operationID, kind string) string {
	first := []string{m8InstallAttempt1, m8InstallAttempt2, m8InstallAttempt3, m8InstallAttempt4}
	second := []string{m8InstallAttempt5, m8InstallAttempt6, m8InstallAttempt7, m8InstallAttempt8}
	index := slices.Index(m8HostLayout, kind)
	if operationID == m8InstallOp2 {
		return second[index]
	}
	return first[index]
}

type m8HostRepositoryFunc func(context.Context, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error)

func (f m8HostRepositoryFunc) AdmitHostAction(ctx context.Context, r domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
	return f(ctx, r)
}

// m8HostSigner wraps the production Ed25519 authority so tests can count and
// intercept signing without replacing the real signature.
type m8HostSigner struct {
	inner  *provisioning.Ed25519ProbeStageGrantSigner
	calls  atomic.Int32
	before func(context.Context, domain.ProbeHostActionAdmission) error
}

var _ ports.ProbeHostActionGrantSigner = (*m8HostSigner)(nil)

func (s *m8HostSigner) PublicKey() [32]byte { return s.inner.PublicKey() }
func (s *m8HostSigner) SignHostActionAdmission(ctx context.Context, a domain.ProbeHostActionAdmission) (domain.ProbeHostActionGrant, error) {
	s.calls.Add(1)
	if s.before != nil {
		if err := s.before(ctx, a); err != nil {
			return domain.ProbeHostActionGrant{}, err
		}
	}
	return s.inner.SignHostActionAdmission(ctx, a)
}

func m8HostAuthority(t *testing.T, seed byte) *m8HostSigner {
	t.Helper()
	inner, err := provisioning.NewEd25519ProbeStageGrantSigner(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32)))
	if err != nil {
		t.Fatal("construct fixture-only host action authority")
	}
	return &m8HostSigner{inner: inner}
}

func m8HostRequest(signer ports.ProbeHostActionGrantSigner, operationID, ownerID, kind string) domain.ProbeHostActionAdmissionRequest {
	identity := m8InstallIdentity(domain.ProbeInstallSystemd, operationID)
	identity.Host.User = "root"
	intent := domain.ProbeHostActionIntent{Identity: identity, OwnerID: ownerID, AttemptID: m8HostAttempt(operationID, kind), Kind: kind}
	return domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}}
}

func m8HostService(t *testing.T, repo ports.ProbeHostActionAdmissionRepository, signer ports.ProbeHostActionGrantSigner) *services.ProbeHostActionAuthorizationService {
	t.Helper()
	svc, err := services.NewProbeHostActionAuthorizationService(repo, signer)
	if err != nil {
		t.Fatal("construct host action authorization service")
	}
	return svc
}

const (
	// m8HostInFlight leaves every predecessor applied and the target reserved.
	m8HostInFlight = iota
	// m8HostPlanned leaves every predecessor applied and the target planned.
	m8HostPlanned
	// m8HostPredecessorOpen reserves, but never records, the immediate predecessor.
	m8HostPredecessorOpen
	// m8HostNone creates nothing; the leaf builds its own journal.
	m8HostNone
)

// m8HostPrepare reaches the requested state exclusively through production
// journal entry points: every predecessor is reserved and then recorded applied.
func m8HostPrepare(t *testing.T, f probeRegistryFixture, r domain.ProbeHostActionAdmissionRequest, stage int) *services.ProbeInstallJournalService {
	t.Helper()
	svc := m8JournalService(t, f)
	id, owner := r.Intent.Identity, r.Intent.OwnerID
	if _, fresh, err := svc.Create(t.Context(), id, owner); err != nil || !fresh {
		t.Fatal("create fixture intent")
	}
	if _, err := svc.Begin(t.Context(), id.OperationID, owner); err != nil {
		t.Fatal("begin fixture intent")
	}
	target := slices.Index(m8HostLayout, r.Intent.Kind)
	for i := range target {
		kind, attempt := m8HostLayout[i], m8HostAttempt(id.OperationID, m8HostLayout[i])
		if _, fresh, err := svc.ReserveResource(t.Context(), id.OperationID, owner, kind, attempt); err != nil || !fresh {
			t.Fatalf("reserve predecessor %s", kind)
		}
		if stage == m8HostPredecessorOpen && i == target-1 {
			break
		}
		if _, err := svc.RecordResource(t.Context(), id.OperationID, owner, kind, attempt, m8InstallApplied()); err != nil {
			t.Fatalf("record joined predecessor %s", kind)
		}
	}
	if stage == m8HostInFlight {
		if _, fresh, err := svc.ReserveResource(t.Context(), id.OperationID, owner, r.Intent.Kind, r.Intent.AttemptID); err != nil || !fresh {
			t.Fatal("reserve target")
		}
	}
	return svc
}

type m8HostRawAdmission struct {
	bun.BaseModel  `bun:"table:probe_host_action_admissions"`
	OperationID    string    `bun:"operation_id"`
	Kind           string    `bun:"kind"`
	OwnerID        string    `bun:"owner_id"`
	AttemptID      string    `bun:"attempt_id"`
	AuthorityKeyID string    `bun:"authority_key_id"`
	NonceHex       string    `bun:"nonce_hex"`
	MarkerSHA256   string    `bun:"marker_sha256"`
	AdmittedAt     time.Time `bun:"admitted_at"`
}

func m8HostCount(ctx context.Context, db *bun.DB) (int64, error) {
	return db.NewSelect().Table("probe_host_action_admissions").Count(ctx)
}

type m8HostResult struct {
	grant domain.ProbeHostActionGrant
	err   error
}

// m8HostEnv is one isolated engine fixture for one action kind.
type m8HostEnv struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	f       probeRegistryFixture
	engine  string
	signer  *m8HostSigner
	r       domain.ProbeHostActionAdmissionRequest
	store   *repository.ProbeHostActionAdmissionStore
	svc     *services.ProbeHostActionAuthorizationService
	journal *services.ProbeInstallJournalService
	index   int
}

func newM8HostEnv(t *testing.T, engine, kind string, stage int) *m8HostEnv {
	t.Helper()
	f := newProbeRegistryFixture(t, engine)
	signer := m8HostAuthority(t, 53)
	r := m8HostRequest(signer, m8InstallOp1, m8InstallOwnerA, kind)
	store := repository.NewProbeHostActionAdmissionStore(f.db)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	t.Cleanup(cancel)
	e := &m8HostEnv{t: t, ctx: ctx, cancel: cancel, f: f, engine: engine, signer: signer, r: r, store: store,
		svc: m8HostService(t, store, signer), index: slices.Index(m8HostLayout, kind)}
	if stage != m8HostNone {
		e.journal = m8HostPrepare(t, f, r, stage)
	}
	return e
}

func (e *m8HostEnv) operationID() string { return e.r.Intent.Identity.OperationID }

func (e *m8HostEnv) rows(want int64) {
	e.t.Helper()
	count, err := m8HostCount(e.t.Context(), e.f.db)
	if err != nil || count != want {
		e.t.Fatalf("admission row count: got %d want %d (err=%v)", count, want, err)
	}
}

func (e *m8HostEnv) refused(grant domain.ProbeHostActionGrant, err, want error) {
	e.t.Helper()
	if grant != (domain.ProbeHostActionGrant{}) || !errors.Is(err, want) {
		e.t.Fatalf("zero grant/fixed refusal missing (match=%v)", errors.Is(err, want))
	}
}

func (e *m8HostEnv) granted(grant domain.ProbeHostActionGrant, err error, r domain.ProbeHostActionAdmissionRequest) {
	e.t.Helper()
	if err != nil || grant.Request != r || !grant.ValidFor(e.signer.PublicKey()) {
		e.t.Fatal("expected exact cryptographically verified host action grant")
	}
}

func (e *m8HostEnv) noSigning() {
	e.t.Helper()
	if e.signer.calls.Load() != 0 {
		e.t.Fatal("refused admission was signed")
	}
}

func (e *m8HostEnv) snapshot() string {
	return m8InstallSnapshot(e.t, e.f, e.operationID())
}

func (e *m8HostEnv) rawAdmissions() []m8HostRawAdmission {
	e.t.Helper()
	var rows []m8HostRawAdmission
	if err := e.f.db.NewSelect().Model(&rows).Order("operation_id ASC", "kind ASC").Scan(e.t.Context()); err != nil {
		e.t.Fatalf("read admissions: %v", err)
	}
	return rows
}

// blockedCall starts one authorization on an independent connection set and
// reports when it reaches the operation authority lock.
func (e *m8HostEnv) blockedCall() (<-chan m8HostResult, <-chan struct{}, <-chan struct{}, context.CancelFunc) {
	e.t.Helper()
	peer := m8InstallSecondDB(e.t, e.f, "")
	hook := &m8JournalLockEntry{entered: make(chan struct{})}
	attachInPlaceQueryHook(peer, hook)
	svc := m8HostService(e.t, repository.NewProbeHostActionAdmissionStore(peer), e.signer)
	call, cancel := context.WithCancel(e.ctx)
	result := make(chan m8HostResult, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		g, err := svc.Authorize(call, e.r)
		result <- m8HostResult{g, err}
	}()
	return result, hook.entered, joined, cancel
}

func (e *m8HostEnv) await(result <-chan m8HostResult) m8HostResult {
	e.t.Helper()
	select {
	case r := <-result:
		return r
	case <-e.ctx.Done():
		e.t.Fatal("host action authorization did not join")
		return m8HostResult{}
	}
}

type m8HostLeaf struct {
	name  string
	stage int
	// skip returns a non-empty reason for kinds where the scenario has no
	// meaning (for example a predecessor check on the first layout resource).
	// Such a leaf is not generated for that kind at all.
	skip func(kind string) string
	run  func(e *m8HostEnv)
}

func m8HostLeaves() []m8HostLeaf {
	return []m8HostLeaf{
		{name: "AdmitsAndGrantsVerifiedSignature", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			before := m8InstallRawResources(e.t, e.f, e.operationID())
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			if g.ValidFor(m8HostAuthority(e.t, 54).PublicKey()) {
				e.t.Fatal("grant verified under a foreign authority")
			}
			e.rows(1)
			after := m8InstallRawResources(e.t, e.f, e.operationID())
			if len(after) != len(before) || after[e.index].State != domain.ProbeInstallResourceInFlight || after[e.index].FinishedAt != nil ||
				!after[e.index].StartedAt.Equal(*before[e.index].StartedAt) {
				e.t.Fatal("admission fabricated an applied resource")
			}
			for i := range e.index {
				if after[i].State != domain.ProbeInstallResourceApplied {
					e.t.Fatal("admission disturbed an applied predecessor")
				}
			}
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("exactly one signature expected")
			}
		}},
		{name: "StoreRecordsExactRow", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			a, err := e.store.AdmitHostAction(e.ctx, e.r)
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
			if row.OperationID != intent.Identity.OperationID || row.Kind != intent.Kind || row.OwnerID != intent.OwnerID ||
				row.AttemptID != intent.AttemptID || row.AuthorityKeyID != e.r.Challenge.AuthorityKeyID ||
				row.NonceHex != hex.EncodeToString(e.r.Challenge.Nonce[:]) || row.MarkerSHA256 != intent.MarkerSHA256() ||
				!row.AdmittedAt.Equal(a.AdmittedAt) {
				e.t.Fatal("stored admission differs from the returned admission")
			}
			if op := m8InstallRawOp(e.t, e.f, e.operationID()); !op.UpdatedAt.Equal(a.AdmittedAt) || op.State != domain.ProbeInstallRunning {
				e.t.Fatal("operation causal stamp was not advanced to the admission stamp")
			}
		}},
		{name: "PredecessorNotApplied", stage: m8HostPredecessorOpen,
			skip: func(kind string) string {
				if kind == domain.ProbeHostActionDataDirectory {
					return "data_directory is the first layout resource and has no predecessor"
				}
				return ""
			},
			run: func(e *m8HostEnv) {
				before := e.snapshot()
				g, err := e.svc.Authorize(e.ctx, e.r)
				e.refused(g, err, ports.ErrConflict)
				e.rows(0)
				e.noSigning()
				if before != e.snapshot() {
					e.t.Fatal("refusal modified durable history")
				}
				// The journal enforces the same ordering, so no admission can
				// become eligible until the predecessor really commits applied.
				if _, _, err := e.journal.ReserveResource(e.ctx, e.operationID(), e.r.Intent.OwnerID, e.r.Intent.Kind, e.r.Intent.AttemptID); err == nil {
					e.t.Fatal("journal reserved a resource behind an unapplied predecessor")
				}
				previous := m8HostLayout[e.index-1]
				if _, err := e.journal.RecordResource(e.ctx, e.operationID(), e.r.Intent.OwnerID, previous, m8HostAttempt(e.operationID(), previous), m8InstallApplied()); err != nil {
					e.t.Fatal("commit predecessor positive control")
				}
				if _, fresh, err := e.journal.ReserveResource(e.ctx, e.operationID(), e.r.Intent.OwnerID, e.r.Intent.Kind, e.r.Intent.AttemptID); err != nil || !fresh {
					e.t.Fatal("commit target positive control")
				}
				g, err = e.svc.Authorize(e.ctx, e.r)
				e.granted(g, err, e.r)
				e.rows(1)
			}},
		{name: "UncommittedPredecessorNotVisible", stage: m8HostPredecessorOpen, run: func(e *m8HostEnv) {
			// Present a complete eligible graph inside a held transaction: a dirty
			// reader must not authorize it, and a rollback must leave no residue.
			blocker, err := e.f.db.BeginTx(e.ctx, nil)
			if err != nil {
				e.t.Fatal("begin held transaction")
			}
			defer func() { _ = blocker.Rollback() }()
			op := e.operationID()
			if _, err := blocker.ExecContext(e.ctx, "UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", op); err != nil {
				e.t.Fatal("hold operation lock")
			}
			if e.index > 0 {
				if _, err := blocker.ExecContext(e.ctx, "UPDATE probe_install_resources SET state = 'applied', finished_at = started_at WHERE operation_id = ? AND ordinal = ?", op, e.index); err != nil {
					e.t.Fatal("construct uncommitted predecessor")
				}
			}
			if _, err := blocker.ExecContext(e.ctx, "UPDATE probe_install_resources SET state = 'in_flight', attempt_id = ?, started_at = (SELECT updated_at FROM probe_install_operations WHERE operation_id = ?) WHERE operation_id = ? AND ordinal = ?", e.r.Intent.AttemptID, op, op, e.index+1); err != nil {
				e.t.Fatal("construct uncommitted target reservation")
			}
			result, entered, joined, cancel := e.blockedCall()
			defer func() { cancel(); _ = blocker.Rollback(); <-joined }()
			select {
			case <-entered:
			case <-e.ctx.Done():
				e.t.Fatal("admission never reached operation lock")
			}
			select {
			case <-result:
				e.t.Fatal("admission bypassed held operation")
			default:
			}
			if err := blocker.Rollback(); err != nil {
				e.t.Fatal("rollback uncommitted graph")
			}
			got := e.await(result)
			e.refused(got.grant, got.err, ports.ErrConflict)
			e.rows(0)
			resources := m8InstallRawResources(e.t, e.f, op)
			if e.signer.calls.Load() != 0 || resources[e.index].State != domain.ProbeInstallResourcePlanned || resources[e.index].AttemptID != nil ||
				(e.index > 0 && resources[e.index-1].State != domain.ProbeInstallResourceInFlight) {
				e.t.Fatal("uncommitted graph escaped rollback")
			}
			// The same request succeeds once the prerequisites really commit, so
			// the refusal left no consumption tombstone.
			if e.index > 0 {
				previous := m8HostLayout[e.index-1]
				if _, err := e.journal.RecordResource(e.ctx, op, e.r.Intent.OwnerID, previous, m8HostAttempt(op, previous), m8InstallApplied()); err != nil {
					e.t.Fatal("commit predecessor positive control")
				}
			}
			if _, fresh, err := e.journal.ReserveResource(e.ctx, op, e.r.Intent.OwnerID, e.r.Intent.Kind, e.r.Intent.AttemptID); err != nil || !fresh {
				e.t.Fatal("commit target positive control")
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			e.rows(1)
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("committed graph did not sign exactly once")
			}
		}},
		{name: "WrongAttempt", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			before := e.snapshot()
			// An unknown attempt and every attempt of the other layout resources
			// must each be refused for this resource.
			attempts := []string{"aaaa0009-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
			for _, kind := range m8HostLayout {
				if kind != e.r.Intent.Kind {
					attempts = append(attempts, m8HostAttempt(e.operationID(), kind))
				}
			}
			for _, attempt := range attempts {
				bad := e.r
				bad.Intent.AttemptID = attempt
				bad.Challenge.MarkerSHA256 = bad.Intent.MarkerSHA256()
				g, err := e.svc.Authorize(e.ctx, bad)
				e.refused(g, err, ports.ErrConflict)
			}
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("wrong-attempt refusal modified durable history")
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
		}},
		{name: "WrongOwner", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			before := e.snapshot()
			bad := e.r
			bad.Intent.OwnerID = m8InstallOwnerB
			bad.Challenge.MarkerSHA256 = bad.Intent.MarkerSHA256()
			g, err := e.svc.Authorize(e.ctx, bad)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("wrong-owner refusal modified durable history")
			}
		}},
		{name: "IdentityMismatch", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			before := e.snapshot()
			for _, change := range []func(*domain.ProbeHostActionAdmissionRequest){
				func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.Identity.RequestedBy++ },
				func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.Identity.Host.Address = "192.0.2.11:22" },
				func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.Identity.Binary.Version = "1.2.4" },
				func(r *domain.ProbeHostActionAdmissionRequest) {
					r.Intent.Identity.HostKeyFingerprint = m8InstallFingerprint('c')
				},
				func(r *domain.ProbeHostActionAdmissionRequest) {
					r.Intent.Identity.ClientKeyFingerprint = m8InstallFingerprint('d')
				},
			} {
				bad := e.r
				change(&bad)
				bad.Challenge.MarkerSHA256 = bad.Intent.MarkerSHA256()
				g, err := e.svc.Authorize(e.ctx, bad)
				e.refused(g, err, ports.ErrConflict)
			}
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("identity-mismatch refusal modified durable history")
			}
		}},
		{name: "ResourceNotInFlight", stage: m8HostPlanned, run: func(e *m8HostEnv) {
			before := e.snapshot()
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("planned-resource refusal modified durable history")
			}
			if _, fresh, err := e.journal.ReserveResource(e.ctx, e.operationID(), e.r.Intent.OwnerID, e.r.Intent.Kind, e.r.Intent.AttemptID); err != nil || !fresh {
				e.t.Fatal("precommit refusal consumed reservation")
			}
			g, err = e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			e.rows(1)
		}},
		{name: "ResourceAlreadyApplied", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			if _, err := e.journal.RecordResource(e.ctx, e.operationID(), e.r.Intent.OwnerID, e.r.Intent.Kind, e.r.Intent.AttemptID, m8InstallApplied()); err != nil {
				e.t.Fatal("record joined target report")
			}
			before := e.snapshot()
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("applied-resource refusal modified durable history")
			}
		}},
		{name: "CancelRequestedRefused", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			if _, err := e.journal.Cancel(e.ctx, e.operationID()); err != nil {
				e.t.Fatal("persist cancellation")
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			if e.signer.calls.Load() != 0 || m8InstallRawOp(e.t, e.f, e.operationID()).State != domain.ProbeInstallCancelRequested {
				e.t.Fatal("cancellation fence lost")
			}
		}},
		{name: "ExpiredDeadlineDBClock", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			m8InstallExpire(e.t, e.f, e.operationID())
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			e.expectDurablyExpired()
		}},
		{name: "ExpiryAfterAuthorityLock", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			seed, err := e.f.db.BeginTx(e.ctx, nil)
			if err != nil {
				e.t.Fatal("begin deadline fixture")
			}
			defer func() { _ = seed.Rollback() }()
			deadline := m8StageClock(e.t, e.ctx, seed, e.engine).Add(1200 * time.Millisecond)
			if _, err := seed.ExecContext(e.ctx, "UPDATE probe_install_operations SET created_at = ?, deadline_at = ? WHERE operation_id = ?", deadline.Add(-domain.ProbeInstallLifetime), deadline, e.operationID()); err != nil {
				e.t.Fatal("set live deadline")
			}
			if err := seed.Commit(); err != nil {
				e.t.Fatal("commit live deadline")
			}
			blocker, err := e.f.db.BeginTx(e.ctx, nil)
			if err != nil {
				e.t.Fatal("begin authority blocker")
			}
			defer func() { _ = blocker.Rollback() }()
			if _, err := blocker.ExecContext(e.ctx, "UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", e.operationID()); err != nil {
				e.t.Fatal("hold authority lock")
			}
			result, entered, joined, cancel := e.blockedCall()
			defer func() { cancel(); _ = blocker.Rollback(); <-joined }()
			select {
			case <-entered:
			case <-e.ctx.Done():
				e.t.Fatal("admission never reached authority lock")
			}
			if !m8StageClock(e.t, e.ctx, blocker, e.engine).Before(deadline) {
				e.t.Fatal("fixture was not live at lock entry")
			}
			for m8StageClock(e.t, e.ctx, blocker, e.engine).Before(deadline) {
				select {
				case <-result:
					e.t.Fatal("admission bypassed held lock")
				case <-time.After(20 * time.Millisecond):
				case <-e.ctx.Done():
					e.t.Fatal("deadline observation timed out")
				}
			}
			if err := blocker.Commit(); err != nil {
				e.t.Fatal("release authority lock after expiry")
			}
			got := e.await(result)
			e.refused(got.grant, got.err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			e.expectDurablyExpired()
		}},
		{name: "ExactReplayConflict", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			before := e.snapshot()
			g, err = e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			if a, err := e.store.AdmitHostAction(e.ctx, e.r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrConflict) {
				e.t.Fatal("direct store replay returned an admission")
			}
			// A cold process on independent connections sees the same history.
			cold := m8HostService(e.t, repository.NewProbeHostActionAdmissionStore(m8InstallSecondDB(e.t, e.f, "")), m8HostAuthority(e.t, 53))
			g, err = cold.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			if before != e.snapshot() {
				e.t.Fatal("replay modified durable history")
			}
			e.rows(1)
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("replay signed again")
			}
		}},
		{name: "DifferentChallengeNoRearm", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			fresh := e.r
			fresh.Challenge.Nonce[1]++
			g, err = e.svc.Authorize(e.ctx, fresh)
			e.refused(g, err, ports.ErrConflict)
			rotated := m8HostAuthority(e.t, 54)
			fresh.Challenge.AuthorityKeyID = domain.ProbeBinaryStageAuthorityKeyID(rotated.PublicKey())
			g, err = m8HostService(e.t, e.store, rotated).Authorize(e.ctx, fresh)
			e.refused(g, err, ports.ErrConflict)
			if rotated.calls.Load() != 0 {
				e.t.Fatal("authority rotation rearmed a consumed attempt")
			}
			e.rows(1)
		}},
		{name: "NonceReuseAcrossAttempts", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			// A second operation, owner and attempt reuses the first challenge's
			// authority and nonce; only the (authority, nonce) uniqueness can stop it.
			second := m8HostRequest(e.signer, m8InstallOp2, m8InstallOwnerB, e.r.Intent.Kind)
			if second.Challenge.Nonce != e.r.Challenge.Nonce || second.Intent.AttemptID == e.r.Intent.AttemptID {
				e.t.Fatal("fixture must reuse the nonce with a different attempt")
			}
			m8HostPrepare(e.t, e.f, second, m8HostInFlight)
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			before := m8InstallSnapshot(e.t, e.f, m8InstallOp2)
			g, err = e.svc.Authorize(e.ctx, second)
			e.refused(g, err, ports.ErrConflict)
			e.rows(1)
			if before != m8InstallSnapshot(e.t, e.f, m8InstallOp2) || e.signer.calls.Load() != 1 {
				e.t.Fatal("nonce-reuse refusal modified history or signed")
			}
			second.Challenge.Nonce[1]++
			g, err = e.svc.Authorize(e.ctx, second)
			e.granted(g, err, second)
			e.rows(2)
		}},
		{name: "CorruptGraphNoGrant", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			m8InstallExpire(e.t, e.f, e.operationID())
			if _, err := e.f.db.ExecContext(e.ctx, "UPDATE probe_install_resources SET reference = ? WHERE operation_id = ? AND kind = ?", "/foreign", e.operationID(), e.r.Intent.Kind); err != nil {
				e.t.Fatal("construct malformed graph")
			}
			before := e.snapshot()
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, domain.ErrInternal)
			e.rows(0)
			if before != e.snapshot() {
				e.t.Fatal("corrupt graph was repaired")
			}
			e.noSigning()
		}},
		{name: "CanceledContextRefused", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			before := e.snapshot()
			canceled, cancel := context.WithCancel(e.ctx)
			cancel()
			if a, err := e.store.AdmitHostAction(canceled, e.r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, context.Canceled) {
				e.t.Fatal("canceled store call admitted")
			}
			g, err := e.svc.Authorize(canceled, e.r)
			e.refused(g, err, context.Canceled)
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("canceled call modified durable history")
			}
			g, err = e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
		}},
		{name: "LostAdmissionAckNoSign", stage: m8HostInFlight, run: func(e *m8HostEnv) { e.postCommitLoss(false) }},
		{name: "CallerCanceledAfterCommitNoGrant", stage: m8HostInFlight, run: func(e *m8HostEnv) { e.postCommitLoss(true) }},
		{name: "ConcurrentAdmissionSingleGrant", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			peer := m8InstallSecondDB(e.t, e.f, "")
			other := m8HostService(e.t, repository.NewProbeHostActionAdmissionStore(peer), e.signer)
			start := make(chan struct{})
			results := make(chan m8HostResult, 2)
			var wg sync.WaitGroup
			for _, instance := range []*services.ProbeHostActionAuthorizationService{e.svc, other} {
				wg.Go(func() { <-start; g, err := instance.Authorize(e.ctx, e.r); results <- m8HostResult{g, err} })
			}
			e.t.Cleanup(func() { e.cancel(); wg.Wait() })
			close(start)
			success := 0
			for range 2 {
				got := e.await(results)
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
			e.rows(1)
		}},
		{name: "SignFailureConsumesAdmission", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			e.signer.before = func(context.Context, domain.ProbeHostActionAdmission) error {
				return errors.New("private fixture signing diagnostic")
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, domain.ErrInternal)
			e.rows(1)
			e.signer.before = nil
			g, err = e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			if e.signer.calls.Load() != 1 || m8InstallRawResources(e.t, e.f, e.operationID())[e.index].State != domain.ProbeInstallResourceInFlight {
				e.t.Fatal("sign failure rearmed or applied a resource")
			}
		}},
		{name: "IndependentReaderSeesAdmissionBeforeSign", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			peer := m8InstallSecondDB(e.t, e.f, "")
			observed := false
			e.signer.before = func(call context.Context, a domain.ProbeHostActionAdmission) error {
				var nonce, marker, attempt, kind string
				if err := peer.NewRaw("SELECT nonce_hex, marker_sha256, attempt_id, kind FROM probe_host_action_admissions WHERE operation_id = ?", e.operationID()).Scan(call, &nonce, &marker, &attempt, &kind); err != nil {
					return domain.ErrInternal
				}
				if nonce != hex.EncodeToString(e.r.Challenge.Nonce[:]) || marker != e.r.Intent.MarkerSHA256() || attempt != e.r.Intent.AttemptID ||
					kind != e.r.Intent.Kind || a.Request != e.r {
					return domain.ErrInternal
				}
				observed = true
				return nil
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			if !observed {
				e.t.Fatal("signer did not independently observe the committed admission")
			}
		}},
		{name: "ClockRollbackStamp", stage: m8HostInFlight, run: func(e *m8HostEnv) {
			op := m8InstallRawOp(e.t, e.f, e.operationID())
			floor := op.DeadlineAt.Add(-time.Minute)
			if _, err := e.f.db.ExecContext(e.ctx, "UPDATE probe_install_operations SET updated_at = ? WHERE operation_id = ?", floor, e.operationID()); err != nil {
				e.t.Fatal("set causal floor")
			}
			if _, err := e.f.db.ExecContext(e.ctx, "UPDATE probe_install_resources SET started_at = ? WHERE operation_id = ? AND ordinal = ?", floor, e.operationID(), e.index+1); err != nil {
				e.t.Fatal("set resource floor")
			}
			var stamp time.Time
			e.signer.before = func(_ context.Context, a domain.ProbeHostActionAdmission) error { stamp = a.AdmittedAt; return nil }
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			after := m8InstallRawOp(e.t, e.f, e.operationID())
			if !stamp.Equal(floor) || stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 || !after.UpdatedAt.Equal(floor) || !after.DeadlineAt.Equal(op.DeadlineAt) {
				e.t.Fatal("admission rewound the causal stamp or renewed the deadline")
			}
			if rows := e.rawAdmissions(); len(rows) != 1 || !rows[0].AdmittedAt.Equal(floor) {
				e.t.Fatal("stored admission stamp rewound")
			}
		}},
	}
}

// expectDurablyExpired asserts the honest cold-expiry state: the operation is
// interrupted by the deadline and exactly the in-flight target became unknown.
func (e *m8HostEnv) expectDurablyExpired() {
	e.t.Helper()
	op := m8InstallRawOp(e.t, e.f, e.operationID())
	resources := m8InstallRawResources(e.t, e.f, e.operationID())
	if op.State != domain.ProbeInstallInterrupted || op.ErrorCode == nil || *op.ErrorCode != "deadline_expired" ||
		resources[e.index].State != domain.ProbeInstallResourceUnknown {
		e.t.Fatal("expired admission was not durably refused")
	}
	for i := range e.index {
		if resources[i].State != domain.ProbeInstallResourceApplied {
			e.t.Fatal("expiry rewrote an applied predecessor")
		}
	}
}

// postCommitLoss decorates the real store so the commit is observed from an
// independent connection, then loses the acknowledgment or the caller context.
func (e *m8HostEnv) postCommitLoss(cancelCaller bool) {
	e.t.Helper()
	peer := m8InstallSecondDB(e.t, e.f, "")
	observed := false
	decorated := m8HostRepositoryFunc(func(call context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
		a, err := e.store.AdmitHostAction(call, request)
		if err != nil {
			return domain.ProbeHostActionAdmission{}, err
		}
		count, readErr := m8HostCount(call, peer)
		if readErr != nil || count != 1 {
			return domain.ProbeHostActionAdmission{}, domain.ErrInternal
		}
		observed = true
		if cancelCaller {
			e.cancel()
			return a, nil
		}
		return domain.ProbeHostActionAdmission{}, domain.ErrInternal
	})
	g, err := m8HostService(e.t, decorated, e.signer).Authorize(e.ctx, e.r)
	want := domain.ErrInternal
	if cancelCaller {
		want = context.Canceled
	}
	e.refused(g, err, want)
	if !observed || e.signer.calls.Load() != 0 {
		e.t.Fatal("lost/canceled postcommit acknowledgment signed")
	}
	e.rows(1)
	g, err = m8HostService(e.t, repository.NewProbeHostActionAdmissionStore(peer), e.signer).Authorize(e.t.Context(), e.r)
	e.refused(g, err, ports.ErrConflict)
}

// TestM8HostActionAuthorization proves one-shot admission for every host-action
// kind on every engine. The MariaDB leaves skip without TEST_MARIADB_DSN and must
// be exercised by the real-engine gate; a skip establishes nothing about InnoDB.
func TestM8HostActionAuthorization(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, kind := range m8HostKinds {
				t.Run(kind, func(t *testing.T) {
					for _, leaf := range m8HostLeaves() {
						// A leaf with a documented reason is not generated for that
						// kind, so it never shows up as a misleading skip or pass.
						if leaf.skip != nil && leaf.skip(kind) != "" {
							continue
						}
						t.Run(leaf.name, func(t *testing.T) {
							leaf.run(newM8HostEnv(t, engine, kind, leaf.stage))
						})
					}
				})
			}
		})
	}
}

// TestM8HostActionAdmissionSchema covers the per-engine contracts that are not
// specific to one kind: schema constraints, the artifact_directory refusal, an
// unknown operation, and the irreversible downgrade guard.
func TestM8HostActionAdmissionSchema(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("SchemaConstraints", func(t *testing.T) {
				e := newM8HostEnv(t, engine, domain.ProbeHostActionDataDirectory, m8HostPlanned)
				r := e.r
				base := map[string]any{
					"operation_id": r.Intent.Identity.OperationID, "kind": "data_directory", "owner_id": r.Intent.OwnerID,
					"attempt_id": m8InstallAttempt1, "authority_key_id": r.Challenge.AuthorityKeyID,
					"nonce_hex": hex.EncodeToString(r.Challenge.Nonce[:]), "marker_sha256": r.Challenge.MarkerSHA256,
				}
				insert := func(override map[string]any) error {
					values := map[string]any{}
					for k, v := range base {
						values[k] = v
					}
					for k, v := range override {
						values[k] = v
					}
					_, err := e.f.db.ExecContext(e.ctx, "INSERT INTO probe_host_action_admissions (operation_id, kind, owner_id, attempt_id, authority_key_id, nonce_hex, marker_sha256, admitted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
						values["operation_id"], values["kind"], values["owner_id"], values["attempt_id"], values["authority_key_id"], values["nonce_hex"], values["marker_sha256"],
						time.Now().UTC().Truncate(time.Microsecond))
					return err
				}
				// No row exists yet: each bad insert must be rejected by its own
				// schema constraint, not by a duplicate claim.
				for name, bad := range map[string]map[string]any{
					"owner_id":          {"owner_id": "invalid"},
					"zero_owner":        {"owner_id": "00000000-0000-0000-0000-000000000000"},
					"operation_id":      {"operation_id": "not-a-uuid"},
					"authority_upper":   {"authority_key_id": string(bytes.Repeat([]byte{'A'}, 64))},
					"nonce_zero":        {"nonce_hex": string(bytes.Repeat([]byte{'0'}, 64))},
					"marker":            {"marker_sha256": "bad"},
					"artifact_kind":     {"kind": "artifact_directory"},
					"unknown_kind":      {"kind": "bogus"},
					"uppercase_kind":    {"kind": "UNIT_FILE"},
					"missing_operation": {"operation_id": m8InstallOp3},
				} {
					if err := insert(bad); err == nil {
						t.Fatalf("schema accepted noncanonical admission metadata (%s)", name)
					}
					e.rows(0)
				}
				// Each allowed kind is accepted for its existing resource row.
				for i, kind := range m8HostKinds {
					if err := insert(map[string]any{"kind": kind, "attempt_id": m8HostAttempt(r.Intent.Identity.OperationID, kind), "nonce_hex": hex.EncodeToString(bytes.Repeat([]byte{byte(i + 1)}, 32))}); err != nil {
						t.Fatalf("schema refused allowed kind %s: %v", kind, err)
					}
				}
				e.rows(3)
				// A second operation owns free (operation, kind) pairs, so each
				// uniqueness constraint is isolated: only the targeted one can fail.
				second := m8HostRequest(e.signer, m8InstallOp2, m8InstallOwnerB, domain.ProbeHostActionDataDirectory)
				m8HostPrepare(t, e.f, second, m8HostPlanned)
				firstNonce := hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
				freshNonce := hex.EncodeToString(bytes.Repeat([]byte{9}, 32))
				if insert(map[string]any{"kind": "unit_file", "attempt_id": m8InstallAttempt8, "nonce_hex": freshNonce}) == nil {
					t.Fatal("schema accepted a second admission for one operation and kind")
				}
				if insert(map[string]any{"operation_id": m8InstallOp2, "kind": "unit_file", "attempt_id": m8InstallAttempt1, "nonce_hex": freshNonce}) == nil {
					t.Fatal("schema accepted a reused attempt")
				}
				if insert(map[string]any{"operation_id": m8InstallOp2, "kind": "unit_file", "attempt_id": m8InstallAttempt8, "nonce_hex": firstNonce}) == nil {
					t.Fatal("schema accepted a reused authority nonce")
				}
				e.rows(3)
				if err := insert(map[string]any{"operation_id": m8InstallOp2, "kind": "unit_file", "attempt_id": m8InstallAttempt8, "nonce_hex": freshNonce}); err != nil {
					t.Fatalf("control insert with fresh keys refused: %v", err)
				}
				e.rows(4)
				// The FK restricts deleting a resource that has a consumed admission.
				if _, err := e.f.db.ExecContext(e.ctx, "DELETE FROM probe_install_resources WHERE operation_id = ? AND kind = 'unit_file'", r.Intent.Identity.OperationID); err == nil {
					t.Fatal("deleted a resource that has a consumed admission")
				}
				e.rows(4)
			})
			t.Run("ArtifactDirectoryRefused", func(t *testing.T) {
				// The artifact attempt is fully eligible in the journal. Its real
				// stage marker and a plausible challenge still cannot be consumed
				// through the host-action store, in any table.
				e := newM8HostEnv(t, engine, domain.ProbeHostActionUnitFile, m8HostNone)
				stage := m8StageRequest(m8StageAuthority(t, 53))
				m8StagePrepare(t, e.f, stage, true)
				before := e.snapshot()
				bad := domain.ProbeHostActionAdmissionRequest{
					Intent: domain.ProbeHostActionIntent{Identity: stage.Intent.Identity, OwnerID: stage.Intent.OwnerID, AttemptID: stage.Intent.AttemptID, Kind: "artifact_directory"},
					Challenge: domain.ProbeBinaryStageChallenge{
						AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(e.signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: stage.Challenge.MarkerSHA256,
					},
				}
				if bad.Valid() || bad.Intent.Valid() {
					t.Fatal("domain accepted artifact_directory as a host action")
				}
				a, err := e.store.AdmitHostAction(e.ctx, bad)
				if a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
					t.Fatal("host-action store consumed an artifact_directory attempt")
				}
				g, err := e.svc.Authorize(e.ctx, bad)
				e.refused(g, err, domain.ErrValidation)
				e.rows(0)
				if count, err := m8StageCount(t.Context(), e.f.db); err != nil || count != 0 {
					t.Fatal("artifact refusal wrote a stage admission")
				}
				e.noSigning()
				if before != e.snapshot() {
					t.Fatal("artifact refusal modified durable history")
				}
			})
			t.Run("UnknownOperationNotFound", func(t *testing.T) {
				e := newM8HostEnv(t, engine, domain.ProbeHostActionDataDirectory, m8HostNone)
				missing := m8HostRequest(e.signer, m8InstallOp3, m8InstallOwnerA, domain.ProbeHostActionDataDirectory)
				a, err := e.store.AdmitHostAction(e.ctx, missing)
				if a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("unknown operation was not refused as not found: %v", err)
				}
				e.rows(0)
			})
			t.Run("DowngradeGuard", func(t *testing.T) {
				const migration = "086_probe_host_action_admissions"
				const widening = "089_probe_docker_host_actions"
				e := newM8HostEnv(t, engine, domain.ProbeHostActionDataDirectory, m8HostNone)
				// 086 references 084's resources and 089 only widens 086's table, so
				// 089 is the first migration a rollback removes and the last one a
				// restore reapplies. The shared MariaDB schema must end at 089's shape.
				for _, name := range []string{widening, migration} {
					if err := runEngineMigration(t, e.f.db, engine, name, "down"); err != nil {
						t.Fatalf("empty admission downgrade of %s refused: %v", name, err)
					}
				}
				if _, err := m8HostCount(e.ctx, e.f.db); err == nil {
					t.Fatal("empty downgrade did not remove the table")
				}
				for _, name := range []string{migration, widening} {
					if err := runEngineMigration(t, e.f.db, engine, name, "up"); err != nil {
						t.Fatalf("admission re-upgrade of %s failed: %v", name, err)
					}
				}
				m8HostPrepare(t, e.f, e.r, m8HostInFlight)
				g, err := e.svc.Authorize(e.ctx, e.r)
				e.granted(g, err, e.r)
				t.Cleanup(func() {
					_, _ = e.f.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS probe_host_action_admissions_downgrade_guard")
				})
				// 089 alone would narrow over a legacy-kind admission, so the
				// refusal under test is 086's: its guard runs before it drops anything.
				if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err == nil {
					t.Fatal("downgrade erased an irreversible admission")
				}
				e.rows(1)
				// Consumption survives the refused downgrade.
				g, err = e.svc.Authorize(e.ctx, e.r)
				e.refused(g, err, ports.ErrConflict)
			})
		})
	}
}
