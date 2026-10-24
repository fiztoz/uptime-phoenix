package repository_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// m8ActAdmissionKinds are the activation steps authorized as host actions.
var m8ActAdmissionKinds = []string{domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart}

func m8ActRequest(signer ports.ProbeHostActionGrantSigner, operationID, ownerID, kind string) domain.ProbeHostActionAdmissionRequest {
	intent := domain.ProbeHostActionIntent{Identity: m8ActIdentity(operationID).Install, OwnerID: ownerID, AttemptID: m8ActAttempt(operationID, kind), Kind: kind}
	return domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}}
}

const (
	// m8ActInFlight leaves every predecessor applied and the target reserved.
	m8ActInFlight = iota
	// m8ActPlanned leaves every predecessor applied and the target planned.
	m8ActPlanned
	// m8ActPredecessorOpen reserves, but never records, the immediate predecessor.
	m8ActPredecessorOpen
	// m8ActNone creates nothing; the leaf builds its own journal.
	m8ActNone
)

// m8ActAdmEnv is one isolated engine fixture for one admitted step kind.
type m8ActAdmEnv struct {
	*m8ActEnv
	ctx    context.Context
	cancel context.CancelFunc
	signer *m8HostSigner
	r      domain.ProbeHostActionAdmissionRequest
	svc    *services.ProbeHostActionAuthorizationService
	index  int
}

// m8ActServiceOver builds the production host-action authorization service over
// the activation store's admission, exactly as production wiring would.
func m8ActServiceOver(t *testing.T, store *repository.ProbeInstallActivationStore, signer ports.ProbeHostActionGrantSigner) *services.ProbeHostActionAuthorizationService {
	t.Helper()
	return m8HostService(t, m8HostRepositoryFunc(store.AdmitActivationAction), signer)
}

func newM8ActAdmEnv(t *testing.T, engine, kind string, stage int) *m8ActAdmEnv {
	t.Helper()
	base := newM8ActEnv(t, engine)
	signer := m8HostAuthority(t, 53)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	t.Cleanup(cancel)
	e := &m8ActAdmEnv{
		m8ActEnv: base, ctx: ctx, cancel: cancel, signer: signer,
		r:     m8ActRequest(signer, m8InstallOp1, m8InstallOwnerA, kind),
		svc:   m8ActServiceOver(t, base.store, signer),
		index: m8ActKindIndex(kind),
	}
	if stage != m8ActNone {
		e.prepareOp(m8InstallOp1, m8InstallOwnerA, kind, stage)
	}
	return e
}

// prepareOp reaches the requested state exclusively through production store
// entry points: a ready install, a created and begun activation, every
// predecessor reserved and then recorded applied.
func (e *m8ActAdmEnv) prepareOp(op, owner, kind string, stage int) {
	e.t.Helper()
	e.installReady(op)
	e.create(op, owner)
	e.begin(op, owner)
	target := m8ActKindIndex(kind)
	for i := 0; i < target; i++ {
		step := m8ActKinds[i]
		e.reserve(op, owner, step)
		if stage == m8ActPredecessorOpen && i == target-1 {
			break
		}
		e.record(op, owner, step, m8InstallApplied())
	}
	if stage == m8ActInFlight {
		e.reserve(op, owner, kind)
	}
}

func (e *m8ActAdmEnv) operationID() string { return e.r.Intent.Identity.OperationID }

func (e *m8ActAdmEnv) rows(want int64) {
	e.t.Helper()
	if got := m8ActCount(e.t, e.f, "probe_activation_admissions"); got != want {
		e.t.Fatalf("activation admission row count: got %d want %d", got, want)
	}
}

func (e *m8ActAdmEnv) refused(grant domain.ProbeHostActionGrant, err, want error) {
	e.t.Helper()
	if grant != (domain.ProbeHostActionGrant{}) || !errors.Is(err, want) {
		e.t.Fatalf("zero grant/fixed refusal missing (match=%v err=%v)", errors.Is(err, want), err)
	}
}

func (e *m8ActAdmEnv) granted(grant domain.ProbeHostActionGrant, err error, r domain.ProbeHostActionAdmissionRequest) {
	e.t.Helper()
	if err != nil || grant.Request != r || !grant.ValidFor(e.signer.PublicKey()) {
		e.t.Fatalf("expected exact cryptographically verified activation grant: %v", err)
	}
}

func (e *m8ActAdmEnv) noSigning() {
	e.t.Helper()
	if e.signer.calls.Load() != 0 {
		e.t.Fatal("refused admission was signed")
	}
}

func (e *m8ActAdmEnv) snapshot() string { return m8ActSnapshot(e.t, e.f, e.operationID()) }

// blockedCall starts one authorization on an independent connection set and
// reports when it reaches the shared operation authority lock.
func (e *m8ActAdmEnv) blockedCall() (<-chan m8HostResult, <-chan struct{}, <-chan struct{}, context.CancelFunc) {
	e.t.Helper()
	peer := m8InstallSecondDB(e.t, e.f, "")
	hook := &m8JournalLockEntry{entered: make(chan struct{})}
	peer.AddQueryHook(hook)
	svc := m8ActServiceOver(e.t, repository.NewProbeInstallActivationStore(peer), e.signer)
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

func (e *m8ActAdmEnv) await(result <-chan m8HostResult) m8HostResult {
	e.t.Helper()
	select {
	case r := <-result:
		return r
	case <-e.ctx.Done():
		e.t.Fatal("activation authorization did not join")
		return m8HostResult{}
	}
}

// expectDurablyExpired asserts the honest cold-expiry state: the activation is
// interrupted by the deadline and exactly the in-flight target became unknown.
func (e *m8ActAdmEnv) expectDurablyExpired() {
	e.t.Helper()
	a := m8ActRawRow(e.t, e.f, e.operationID())
	steps := m8ActRawSteps(e.t, e.f, e.operationID())
	if a.State != domain.ProbeActivationInterrupted || a.ErrorCode == nil || *a.ErrorCode != "deadline_expired" ||
		steps[e.index].State != domain.ProbeInstallResourceUnknown {
		e.t.Fatal("expired admission was not durably refused")
	}
	for i := range e.index {
		if steps[i].State != domain.ProbeInstallResourceApplied {
			e.t.Fatal("expiry rewrote an applied predecessor")
		}
	}
}

// postCommitLoss decorates the real store so the commit is observed from an
// independent connection, then loses the acknowledgment or the caller context.
func (e *m8ActAdmEnv) postCommitLoss(cancelCaller bool) {
	e.t.Helper()
	peer := m8InstallSecondDB(e.t, e.f, "")
	observed := false
	decorated := m8HostRepositoryFunc(func(call context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
		a, err := e.store.AdmitActivationAction(call, request)
		if err != nil {
			return domain.ProbeHostActionAdmission{}, err
		}
		count, readErr := peer.NewSelect().Table("probe_activation_admissions").Count(call)
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
	cold := m8ActServiceOver(e.t, repository.NewProbeInstallActivationStore(peer), e.signer)
	g, err = cold.Authorize(e.t.Context(), e.r)
	e.refused(g, err, ports.ErrConflict)
}

type m8ActAdmLeaf struct {
	name  string
	stage int
	// skip returns a non-empty reason for kinds where the scenario has no
	// meaning (for example a predecessor check on the first layout step). Such a
	// leaf is not generated for that kind at all.
	skip func(kind string) string
	run  func(e *m8ActAdmEnv)
}

func m8ActAdmLeaves() []m8ActAdmLeaf {
	return []m8ActAdmLeaf{
		{name: "AdmitsAndGrantsVerifiedSignature", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			before := m8ActRawSteps(e.t, e.f, e.operationID())
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			if g.ValidFor(m8HostAuthority(e.t, 54).PublicKey()) {
				e.t.Fatal("grant verified under a foreign authority")
			}
			e.rows(1)
			after := m8ActRawSteps(e.t, e.f, e.operationID())
			if len(after) != len(before) || after[e.index].State != domain.ProbeInstallResourceInFlight || after[e.index].FinishedAt != nil ||
				!after[e.index].StartedAt.Equal(*before[e.index].StartedAt) {
				e.t.Fatal("admission fabricated an applied step")
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
		{name: "StoreRecordsExactRow", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			installBefore := m8InstallSnapshot(e.t, e.f, e.operationID())
			a, err := e.store.AdmitActivationAction(e.ctx, e.r)
			if err != nil || a.Request != e.r || !a.Valid() {
				e.t.Fatalf("direct admission refused: %v", err)
			}
			if a.AdmittedAt.Location() != time.UTC || a.AdmittedAt.Nanosecond()%1000 != 0 {
				e.t.Fatal("admission stamp is not UTC microseconds")
			}
			rows := m8ActRawAdmissions(e.t, e.f)
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
			if act := m8ActRawRow(e.t, e.f, e.operationID()); !act.UpdatedAt.Equal(a.AdmittedAt) || act.State != domain.ProbeActivationRunning {
				e.t.Fatal("activation causal stamp was not advanced to the admission stamp")
			}
			// The activation admission never touches the install journal or its
			// host-action admissions.
			if m8InstallSnapshot(e.t, e.f, e.operationID()) != installBefore {
				e.t.Fatal("activation admission modified the install journal")
			}
			if n, err := m8HostCount(e.ctx, e.f.db); err != nil || n != 0 {
				e.t.Fatal("activation admission wrote an install host-action admission")
			}
			// The receipt stays a valid graph after the causal stamp moved.
			m8ActCheck(e.t, e.observe(e.operationID()))
		}},
		{name: "PredecessorNotApplied", stage: m8ActPredecessorOpen,
			skip: func(kind string) string {
				if kind == domain.ProbeActivationProbeInit {
					return "probe_init is the first layout step and has no predecessor"
				}
				return ""
			},
			run: func(e *m8ActAdmEnv) {
				before := e.snapshot()
				g, err := e.svc.Authorize(e.ctx, e.r)
				e.refused(g, err, ports.ErrConflict)
				e.rows(0)
				e.noSigning()
				if before != e.snapshot() {
					e.t.Fatal("refusal modified durable history")
				}
				// The journal enforces the same ordering, so no admission can become
				// eligible until the predecessor really commits applied.
				op, owner := e.operationID(), e.r.Intent.OwnerID
				if _, _, err := e.store.ReserveActivationStep(e.ctx, op, owner, e.r.Intent.Kind, e.r.Intent.AttemptID); err == nil {
					e.t.Fatal("journal reserved a step behind an unapplied predecessor")
				}
				previous := m8ActKinds[e.index-1]
				e.record(op, owner, previous, m8InstallApplied())
				e.reserve(op, owner, e.r.Intent.Kind)
				g, err = e.svc.Authorize(e.ctx, e.r)
				e.granted(g, err, e.r)
				e.rows(1)
			}},
		{name: "UncommittedPredecessorNotVisible", stage: m8ActPredecessorOpen, run: func(e *m8ActAdmEnv) {
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
				if _, err := blocker.ExecContext(e.ctx, "UPDATE probe_install_activation_steps SET state = 'applied', finished_at = started_at WHERE operation_id = ? AND ordinal = ?", op, e.index); err != nil {
					e.t.Fatal("construct uncommitted predecessor")
				}
			}
			if _, err := blocker.ExecContext(e.ctx, "UPDATE probe_install_activation_steps SET state = 'in_flight', attempt_id = ?, started_at = (SELECT updated_at FROM probe_install_activations WHERE operation_id = ?) WHERE operation_id = ? AND ordinal = ?", e.r.Intent.AttemptID, op, op, e.index+1); err != nil {
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
			steps := m8ActRawSteps(e.t, e.f, op)
			if e.signer.calls.Load() != 0 || steps[e.index].State != domain.ProbeInstallResourcePlanned || steps[e.index].AttemptID != nil ||
				(e.index > 0 && steps[e.index-1].State != domain.ProbeInstallResourceInFlight) {
				e.t.Fatal("uncommitted graph escaped rollback")
			}
			// The same request succeeds once the prerequisites really commit, so
			// the refusal left no consumption tombstone.
			owner := e.r.Intent.OwnerID
			if e.index > 0 {
				e.record(op, owner, m8ActKinds[e.index-1], m8InstallApplied())
			}
			e.reserve(op, owner, e.r.Intent.Kind)
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			e.rows(1)
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("committed graph did not sign exactly once")
			}
		}},
		{name: "StepNotYetReserved", stage: m8ActPlanned, run: func(e *m8ActAdmEnv) {
			before := e.snapshot()
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("planned-step refusal modified durable history")
			}
			e.reserve(e.operationID(), e.r.Intent.OwnerID, e.r.Intent.Kind)
			g, err = e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			e.rows(1)
		}},
		{name: "WrongAttempt", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			before := e.snapshot()
			// An unknown attempt and every attempt of the other layout steps must
			// each be refused for this step.
			attempts := []string{m8ActForeignAttempt}
			for _, kind := range m8ActKinds {
				if kind != e.r.Intent.Kind {
					attempts = append(attempts, m8ActAttempt(e.operationID(), kind))
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
		{name: "WrongKind", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			before := e.snapshot()
			// The other admitted kind carrying this step's attempt names a step
			// that is not in flight (or behind an unapplied predecessor).
			other := domain.ProbeActivationServiceStart
			if e.r.Intent.Kind == domain.ProbeActivationServiceStart {
				other = domain.ProbeActivationProbeInit
			}
			bad := e.r
			bad.Intent.Kind = other
			bad.Challenge.MarkerSHA256 = bad.Intent.MarkerSHA256()
			g, err := e.svc.Authorize(e.ctx, bad)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("wrong-kind refusal modified durable history")
			}
		}},
		{name: "WrongOwner", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
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
		{name: "IdentityMismatch", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
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
		{name: "StepAlreadyApplied", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			e.record(e.operationID(), e.r.Intent.OwnerID, e.r.Intent.Kind, m8InstallApplied())
			before := e.snapshot()
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			if before != e.snapshot() {
				e.t.Fatal("applied-step refusal modified durable history")
			}
		}},
		{name: "CancelRequestedRefused", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			if _, err := e.store.RequestActivationCancellation(e.ctx, e.operationID()); err != nil {
				e.t.Fatal("persist cancellation")
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			if e.signer.calls.Load() != 0 || m8ActRawRow(e.t, e.f, e.operationID()).State != domain.ProbeActivationCancelRequested {
				e.t.Fatal("cancellation fence lost")
			}
		}},
		{name: "TerminalActivationRefused", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			if _, err := e.store.FinishActivation(e.ctx, e.operationID(), e.r.Intent.OwnerID, m8ActFinish(domain.ProbeInstallInterrupted, "owner_interrupted")); err != nil {
				e.t.Fatal("interrupt activation")
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
		}},
		{name: "ExpiredDeadlineDBClock", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			m8ActExpire(e.t, e.f, e.operationID())
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			e.expectDurablyExpired()
		}},
		{name: "ExactReplayConflict", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			before := e.snapshot()
			g, err = e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			if a, err := e.store.AdmitActivationAction(e.ctx, e.r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrConflict) {
				e.t.Fatal("direct store replay returned an admission")
			}
			// A cold process on independent connections sees the same history.
			cold := m8ActServiceOver(e.t, repository.NewProbeInstallActivationStore(m8InstallSecondDB(e.t, e.f, "")), m8HostAuthority(e.t, 53))
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
		{name: "DifferentChallengeNoRearm", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			fresh := e.r
			fresh.Challenge.Nonce[1]++
			g, err = e.svc.Authorize(e.ctx, fresh)
			e.refused(g, err, ports.ErrConflict)
			rotated := m8HostAuthority(e.t, 54)
			fresh.Challenge.AuthorityKeyID = domain.ProbeBinaryStageAuthorityKeyID(rotated.PublicKey())
			g, err = m8ActServiceOver(e.t, e.store, rotated).Authorize(e.ctx, fresh)
			e.refused(g, err, ports.ErrConflict)
			if rotated.calls.Load() != 0 {
				e.t.Fatal("authority rotation rearmed a consumed attempt")
			}
			e.rows(1)
		}},
		{name: "NonceReuseAcrossAttempts", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			// A second operation, owner and attempt reuses the first challenge's
			// authority and nonce; only the (authority, nonce) uniqueness can stop it.
			second := m8ActRequest(e.signer, m8InstallOp2, m8InstallOwnerB, e.r.Intent.Kind)
			if second.Challenge.Nonce != e.r.Challenge.Nonce || second.Intent.AttemptID == e.r.Intent.AttemptID {
				e.t.Fatal("fixture must reuse the nonce with a different attempt")
			}
			e.prepareOp(m8InstallOp2, m8InstallOwnerB, e.r.Intent.Kind, m8ActInFlight)
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			before := m8ActSnapshot(e.t, e.f, m8InstallOp2)
			g, err = e.svc.Authorize(e.ctx, second)
			e.refused(g, err, ports.ErrConflict)
			e.rows(1)
			if before != m8ActSnapshot(e.t, e.f, m8InstallOp2) || e.signer.calls.Load() != 1 {
				e.t.Fatal("nonce-reuse refusal modified history or signed")
			}
			second.Challenge.Nonce[1]++
			g, err = e.svc.Authorize(e.ctx, second)
			e.granted(g, err, second)
			e.rows(2)
		}},
		{name: "CorruptGraphNoGrant", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			m8ActExpire(e.t, e.f, e.operationID())
			if _, err := e.f.db.ExecContext(e.ctx, "UPDATE probe_install_activation_steps SET reference = ? WHERE operation_id = ? AND kind = ?", "/foreign", e.operationID(), e.r.Intent.Kind); err != nil {
				e.t.Fatal("construct malformed graph")
			}
			before := e.snapshot()
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, domain.ErrInternal)
			if a, err := e.store.AdmitActivationAction(e.ctx, e.r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
				e.t.Fatalf("direct store admitted a corrupt graph: %v", err)
			}
			e.rows(0)
			if before != e.snapshot() {
				e.t.Fatal("corrupt graph was repaired")
			}
			e.noSigning()
		}},
		{name: "CanceledContextRefused", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			before := e.snapshot()
			canceled, cancel := context.WithCancel(e.ctx)
			cancel()
			if a, err := e.store.AdmitActivationAction(canceled, e.r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, context.Canceled) {
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
		{name: "LostAdmissionAckNoSign", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) { e.postCommitLoss(false) }},
		{name: "CallerCanceledAfterCommitNoGrant", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) { e.postCommitLoss(true) }},
		{name: "ConcurrentAdmissionSingleGrant", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			peer := m8InstallSecondDB(e.t, e.f, "")
			other := m8ActServiceOver(e.t, repository.NewProbeInstallActivationStore(peer), e.signer)
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
		{name: "SignFailureConsumesAdmission", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			e.signer.before = func(context.Context, domain.ProbeHostActionAdmission) error {
				return errors.New("private fixture signing diagnostic")
			}
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, domain.ErrInternal)
			e.rows(1)
			e.signer.before = nil
			g, err = e.svc.Authorize(e.ctx, e.r)
			e.refused(g, err, ports.ErrConflict)
			if e.signer.calls.Load() != 1 || m8ActRawSteps(e.t, e.f, e.operationID())[e.index].State != domain.ProbeInstallResourceInFlight {
				e.t.Fatal("sign failure rearmed or applied a step")
			}
		}},
		{name: "IndependentReaderSeesAdmissionBeforeSign", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			peer := m8InstallSecondDB(e.t, e.f, "")
			observed := false
			e.signer.before = func(call context.Context, a domain.ProbeHostActionAdmission) error {
				var nonce, marker, attempt, kind string
				if err := peer.NewRaw("SELECT nonce_hex, marker_sha256, attempt_id, kind FROM probe_activation_admissions WHERE operation_id = ?", e.operationID()).Scan(call, &nonce, &marker, &attempt, &kind); err != nil {
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
		{name: "ClockRollbackStamp", stage: m8ActInFlight, run: func(e *m8ActAdmEnv) {
			act := m8ActRawRow(e.t, e.f, e.operationID())
			floor := act.DeadlineAt.Add(-time.Minute)
			if _, err := e.f.db.ExecContext(e.ctx, "UPDATE probe_install_activations SET updated_at = ? WHERE operation_id = ?", floor, e.operationID()); err != nil {
				e.t.Fatal("set causal floor")
			}
			if _, err := e.f.db.ExecContext(e.ctx, "UPDATE probe_install_activation_steps SET started_at = ? WHERE operation_id = ? AND ordinal = ?", floor, e.operationID(), e.index+1); err != nil {
				e.t.Fatal("set step floor")
			}
			var stamp time.Time
			e.signer.before = func(_ context.Context, a domain.ProbeHostActionAdmission) error { stamp = a.AdmittedAt; return nil }
			g, err := e.svc.Authorize(e.ctx, e.r)
			e.granted(g, err, e.r)
			after := m8ActRawRow(e.t, e.f, e.operationID())
			if !stamp.Equal(floor) || stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 || !after.UpdatedAt.Equal(floor) || !after.DeadlineAt.Equal(act.DeadlineAt) {
				e.t.Fatal("admission rewound the causal stamp or renewed the deadline")
			}
			if rows := m8ActRawAdmissions(e.t, e.f); len(rows) != 1 || !rows[0].AdmittedAt.Equal(floor) {
				e.t.Fatal("stored admission stamp rewound")
			}
		}},
	}
}

// TestM8InstallActivationAdmission proves one-shot admission for every
// activation host-action kind on every engine, through the production host-action
// authorization service. The MariaDB leaves skip without TEST_MARIADB_DSN and must
// be exercised by the real-engine gate; a skip establishes nothing about InnoDB.
func TestM8InstallActivationAdmission(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, kind := range m8ActAdmissionKinds {
				t.Run(kind, func(t *testing.T) {
					for _, leaf := range m8ActAdmLeaves() {
						// A leaf with a documented reason is not generated for that
						// kind, so it never shows up as a misleading skip or pass.
						if leaf.skip != nil && leaf.skip(kind) != "" {
							continue
						}
						t.Run(leaf.name, func(t *testing.T) {
							leaf.run(newM8ActAdmEnv(t, engine, kind, leaf.stage))
						})
					}
				})
			}
		})
	}
}

// TestM8InstallActivationAuthorizationLifecycle runs one whole activation through
// the production authorization service: both host steps are granted exactly once
// with real Ed25519 signatures, the hub steps need no grant, and the activation
// ends active with its facts and two consumed admissions.
func TestM8InstallActivationAuthorizationLifecycle(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			e := newM8ActAdmEnv(t, engine, domain.ProbeActivationProbeInit, m8ActNone)
			op, owner := m8InstallOp1, m8InstallOwnerA
			e.installReady(op)
			e.create(op, owner)
			e.begin(op, owner)
			initRequest := m8ActRequest(e.signer, op, owner, domain.ProbeActivationProbeInit)
			startRequest := m8ActRequest(e.signer, op, owner, domain.ProbeActivationServiceStart)
			startRequest.Challenge.Nonce = [32]byte{2}
			// probe_init: reserved, admitted, signed, then reported with its facts.
			e.reserve(op, owner, domain.ProbeActivationProbeInit)
			g, err := e.svc.Authorize(e.ctx, initRequest)
			e.granted(g, err, initRequest)
			// The next host step cannot be admitted before its predecessors.
			g, err = e.svc.Authorize(e.ctx, startRequest)
			e.refused(g, err, ports.ErrConflict)
			e.record(op, owner, domain.ProbeActivationProbeInit, m8InstallApplied())
			// Hub steps carry no host grant.
			e.reserve(op, owner, domain.ProbeActivationRegistration)
			e.record(op, owner, domain.ProbeActivationRegistration, m8InstallApplied())
			e.reserve(op, owner, domain.ProbeActivationServiceStart)
			g, err = e.svc.Authorize(e.ctx, startRequest)
			e.granted(g, err, startRequest)
			e.record(op, owner, domain.ProbeActivationServiceStart, m8InstallApplied())
			e.reserve(op, owner, domain.ProbeActivationEnrollment)
			active := e.record(op, owner, domain.ProbeActivationEnrollment, m8InstallApplied())
			if active.State != domain.ProbeActivationActive || active.Facts != m8ActFacts(op) {
				t.Fatalf("lifecycle did not end active with its facts: %+v", active)
			}
			e.rows(2)
			// Every consumed attempt is gone for good, even after success.
			for _, r := range []domain.ProbeHostActionAdmissionRequest{initRequest, startRequest} {
				g, err = e.svc.Authorize(e.ctx, r)
				e.refused(g, err, ports.ErrConflict)
			}
			if e.signer.calls.Load() != 2 {
				t.Fatalf("expected exactly two signatures, got %d", e.signer.calls.Load())
			}
			if n, err := m8HostCount(e.ctx, e.f.db); err != nil || n != 0 {
				t.Fatal("activation admissions leaked into the install host-action table")
			}
		})
	}
}

// TestM8InstallActivationAdmissionSeparation proves the two admission stores are
// disjoint: the install journal's host-action store never consumes an activation
// intent, and the activation store never consumes an install host action or an
// intent that is not an activation host step.
func TestM8InstallActivationAdmissionSeparation(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("InstallStoreRefusesActivationIntents", func(t *testing.T) {
				e := newM8ActAdmEnv(t, engine, domain.ProbeActivationProbeInit, m8ActInFlight)
				hostStore := repository.NewProbeHostActionAdmissionStore(e.f.db)
				for _, kind := range m8ActAdmissionKinds {
					r := m8ActRequest(e.signer, m8InstallOp1, m8InstallOwnerA, kind)
					before, installBefore := e.snapshot(), m8InstallSnapshot(t, e.f, m8InstallOp1)
					// The install is resources_ready: not a running install.
					a, err := hostStore.AdmitHostAction(e.ctx, r)
					if a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("install host-action store consumed %s on a resources_ready install: %v", kind, err)
					}
					if n, err := m8HostCount(e.ctx, e.f.db); err != nil || n != 0 {
						t.Fatalf("install host-action table gained %d rows", n)
					}
					if before != e.snapshot() || installBefore != m8InstallSnapshot(t, e.f, m8InstallOp1) {
						t.Fatal("refused install-store admission changed durable history")
					}
				}
				e.rows(0)
				// A RUNNING install whose last resource is still in flight has no
				// probe_init/service_start resource at all: invalid, never consumed.
				istore := m8InstallStore(e.f)
				m8InstallCreate(t, istore, m8ActIdentity(m8InstallOp3).Install, m8InstallOwnerA)
				m8InstallBeginOK(t, istore, m8InstallOp3, m8InstallOwnerA)
				for i, kind := range m8InstallKinds(domain.ProbeInstallSystemd) {
					attempt := m8ActInstallAttempt(m8InstallOp3, i+1)
					m8InstallReserveOK(t, istore, m8InstallOp3, m8InstallOwnerA, kind, attempt)
					if i < 3 {
						m8InstallRecordOK(t, istore, m8InstallOp3, m8InstallOwnerA, kind, attempt, m8InstallApplied())
					}
				}
				for _, kind := range m8ActAdmissionKinds {
					r := m8ActRequest(e.signer, m8InstallOp3, m8InstallOwnerA, kind)
					before := m8InstallSnapshot(t, e.f, m8InstallOp3)
					a, err := hostStore.AdmitHostAction(e.ctx, r)
					if a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("install host-action store consumed %s on a running install: %v", kind, err)
					}
					if n, err := m8HostCount(e.ctx, e.f.db); err != nil || n != 0 {
						t.Fatalf("install host-action table gained %d rows", n)
					}
					if before != m8InstallSnapshot(t, e.f, m8InstallOp3) {
						t.Fatal("refused install-store admission changed the install journal")
					}
				}
			})
			t.Run("ActivationStoreRefusesNonActivationIntents", func(t *testing.T) {
				e := newM8ActAdmEnv(t, engine, domain.ProbeActivationProbeInit, m8ActInFlight)
				before := e.snapshot()
				// Install host actions are valid host-action requests but never
				// consumable through the activation store.
				for _, kind := range []string{domain.ProbeHostActionDataDirectory, domain.ProbeHostActionUnitFile, domain.ProbeHostActionService} {
					intent := domain.ProbeHostActionIntent{Identity: m8ActIdentity(m8InstallOp1).Install, OwnerID: m8InstallOwnerA, AttemptID: m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit), Kind: kind}
					r := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
						AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(e.signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
					}}
					if !r.Valid() {
						t.Fatalf("fixture %s must be a valid install host action", kind)
					}
					if a, err := e.store.AdmitActivationAction(e.ctx, r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("activation store consumed install host action %s: %v", kind, err)
					}
				}
				// Hub steps are not host actions: no valid intent can name them.
				for _, kind := range []string{domain.ProbeActivationRegistration, domain.ProbeActivationEnrollment, "artifact_directory", "bogus", ""} {
					intent := domain.ProbeHostActionIntent{Identity: m8ActIdentity(m8InstallOp1).Install, OwnerID: m8InstallOwnerA, AttemptID: m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit), Kind: kind}
					r := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
						AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(e.signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: strings.Repeat("ab", 32),
					}}
					if r.Valid() || intent.Valid() {
						t.Fatalf("domain accepted %q as a host action", kind)
					}
					if a, err := e.store.AdmitActivationAction(e.ctx, r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("activation store consumed %q: %v", kind, err)
					}
				}
				// A non-root identity is not an activation identity.
				nonRoot := m8ActRequest(e.signer, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				nonRoot.Intent.Identity.Host.User = "phoenix"
				nonRoot.Challenge.MarkerSHA256 = nonRoot.Intent.MarkerSHA256()
				if a, err := e.store.AdmitActivationAction(e.ctx, nonRoot); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("activation store consumed a non-root intent: %v", err)
				}
				// An unknown operation is not found, never a conflict.
				missing := m8ActRequest(e.signer, m8InstallOp9, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				if a, err := e.store.AdmitActivationAction(e.ctx, missing); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("unknown operation was not refused as not found: %v", err)
				}
				e.rows(0)
				if before != e.snapshot() || e.signer.calls.Load() != 0 {
					t.Fatal("refused intents modified durable history or signed")
				}
				// The positive control still admits.
				g, err := e.svc.Authorize(e.ctx, e.r)
				e.granted(g, err, e.r)
			})
		})
	}
}

// TestM8InstallActivationClockAfterAuthorityLock proves every activation mutation
// samples the DB clock only after it holds the shared operation authority lock: a
// deadline that expires while an unchanged authority row is held by another
// transaction is refused (or reconciled) after the wait, never accepted on a
// pre-lock clock sample. InnoDB retry cannot mask it and neither host/VM clock
// agreement nor scheduling sleeps prove expiry: the test polls the DB clock.
func TestM8InstallActivationClockAfterAuthorityLock(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, action := range []string{"Begin", "Reserve", "Record", "Finish", "Observe", "Cancel", "Admit"} {
				t.Run(action, func(t *testing.T) {
					e := newM8ActEnv(t, engine)
					ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
					defer cancel()
					op, owner := m8InstallOp1, m8InstallOwnerA
					kind := domain.ProbeActivationProbeInit
					e.fresh(op)
					if action != "Begin" {
						e.begin(op, owner)
					}
					if action == "Record" || action == "Admit" {
						e.reserve(op, owner, kind)
					}
					seed, err := e.f.db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = seed.Rollback() }()
					deadline := m8StageClock(t, ctx, seed, engine).Add(1200 * time.Millisecond)
					if _, err := seed.ExecContext(ctx, "UPDATE probe_install_activations SET created_at = ?, deadline_at = ? WHERE operation_id = ?", deadline.Add(-domain.ProbeActivationLifetime), deadline, op); err != nil {
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
					peer := repository.NewProbeInstallActivationStore(peerDB)
					type reply struct {
						activation *domain.ProbeActivation
						fresh      bool
						err        error
					}
					results := make(chan reply, 1)
					joined := make(chan struct{})
					request := m8ActRequest(m8HostAuthority(t, 53), op, owner, kind)
					go func() {
						defer close(joined)
						var r reply
						switch action {
						case "Begin":
							r.activation, r.err = peer.BeginActivation(ctx, op, owner)
						case "Reserve":
							r.activation, r.fresh, r.err = peer.ReserveActivationStep(ctx, op, owner, kind, m8ActAttempt(op, kind))
						case "Record":
							r.activation, r.err = peer.RecordActivationStep(ctx, op, owner, kind, m8ActAttempt(op, kind), m8InstallApplied(), m8ActFacts(op))
						case "Finish":
							r.activation, r.err = peer.FinishActivation(ctx, op, owner, m8ActFinish(domain.ProbeInstallFailed, "execution_failed"))
						case "Observe":
							r.activation, r.err = peer.ObserveActivation(ctx, op)
						case "Cancel":
							r.activation, r.err = peer.RequestActivationCancellation(ctx, op)
						case "Admit":
							_, r.err = peer.AdmitActivationAction(ctx, request)
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
					if action == "Observe" || action == "Cancel" {
						if r.err != nil || r.activation == nil || r.activation.State != domain.ProbeActivationInterrupted || r.activation.ErrorCode != "deadline_expired" {
							t.Fatalf("expired authority was not reconciled after lock: %v", r.err)
						}
					} else if !errors.Is(r.err, ports.ErrConflict) || r.fresh || r.activation != nil {
						t.Fatalf("expired authority accepted after lock wait: action=%s fresh=%v err=%v", action, r.fresh, r.err)
					}
					// Read raw rows BEFORE Observe can reconcile again: a rolled-back
					// expiry must not be hidden by a subsequent healing read.
					raw := m8ActRawRow(t, e.f, op)
					if raw.State != domain.ProbeActivationInterrupted || raw.ErrorCode == nil || *raw.ErrorCode != "deadline_expired" {
						t.Fatal("expiry reconciliation did not commit before observation")
					}
					if action == "Record" || action == "Admit" {
						steps := m8ActRawSteps(t, e.f, op)
						if steps[0].State != domain.ProbeInstallResourceUnknown || steps[0].ErrorCode == nil || *steps[0].ErrorCode != "deadline_expired" {
							t.Fatal("uncertain step reconciliation did not commit before observation")
						}
					}
					if action == "Admit" && m8ActCount(t, e.f, "probe_activation_admissions") != 0 {
						t.Fatal("an expired authority consumed an admission")
					}
					a, err := e.store.ObserveActivation(ctx, op)
					if err != nil || a.State != domain.ProbeActivationInterrupted || a.ErrorCode != "deadline_expired" {
						t.Fatalf("expiry reconciliation did not commit: %v", err)
					}
					if (action == "Record" || action == "Admit") && a.Steps[0].State != domain.ProbeInstallResourceUnknown {
						t.Fatal("late report replaced uncertain step")
					}
				})
			}
		})
	}
}

// TestM8InstallActivationSchema covers the per-engine contracts that are not
// specific to one store operation: schema constraints of all three tables, the
// restricting foreign keys, and the irreversible downgrade guard.
func TestM8InstallActivationSchema(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("SchemaConstraints", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.installReady(m8InstallOp1)
				e.installReady(m8InstallOp2)
				e.installReady(m8InstallOp3)
				e.installReady(m8InstallOp4)
				facts := m8ActFacts(m8InstallOp1)
				created := time.Now().UTC().Truncate(time.Microsecond)
				actBase := map[string]any{
					"operation_id": m8InstallOp1, "probe_key": "edge-eu-1", "name": "Edge", "location": "", "listen_port": 8443,
					"client_key_fingerprint": m8InstallFingerprint('b'), "requested_by": int64(4242), "owner_id": m8InstallOwnerA,
					"state": "queued", "error_code": nil, "probe_id": nil, "stream_id": nil, "certificate_fingerprint": nil,
					"created_at": created, "updated_at": created, "deadline_at": created.Add(domain.ProbeActivationLifetime),
				}
				columns := []string{"operation_id", "probe_key", "name", "location", "listen_port", "client_key_fingerprint", "requested_by", "owner_id", "state", "error_code", "probe_id", "stream_id", "certificate_fingerprint", "created_at", "updated_at", "deadline_at"}
				insertActivation := func(override map[string]any) error {
					values := make([]any, 0, len(columns))
					for _, c := range columns {
						v := actBase[c]
						if o, ok := override[c]; ok {
							v = o
						}
						values = append(values, v)
					}
					_, err := e.f.db.ExecContext(ctx, "INSERT INTO probe_install_activations ("+strings.Join(columns, ", ")+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", values...)
					return err
				}
				// No row exists yet: each bad insert must be rejected by its own
				// constraint, not by a duplicate claim.
				for name, bad := range map[string]map[string]any{
					"missing_install":     {"operation_id": m8InstallOp9},
					"probe_key_local":     {"probe_key": "local"},
					"probe_key_empty":     {"probe_key": ""},
					"name_empty":          {"name": ""},
					"port_low":            {"listen_port": 1023},
					"port_high":           {"listen_port": 65536},
					"client_key":          {"client_key_fingerprint": "bad"},
					"actor":               {"requested_by": int64(0)},
					"owner_invalid":       {"owner_id": "invalid"},
					"owner_zero":          {"owner_id": "00000000-0000-0000-0000-000000000000"},
					"state_install_state": {"state": "resources_ready"},
					"state_unknown":       {"state": "bogus"},
					"queued_with_error":   {"error_code": "internal"},
					"running_with_error":  {"state": "running", "error_code": "internal"},
					"active_with_error":   {"state": "active", "error_code": "internal"},
					"failed_no_error":     {"state": "failed"},
					"failed_canceled":     {"state": "failed", "error_code": "canceled"},
					"failed_unknown_code": {"state": "failed", "error_code": "bogus"},
					"canceled_other":      {"state": "canceled", "error_code": "internal"},
					"interrupted_other":   {"state": "interrupted", "error_code": "canceled"},
					"interrupted_none":    {"state": "interrupted"},
					"facts_partial":       {"state": "running", "probe_id": facts.ProbeID},
					"facts_bad_probe":     {"state": "running", "probe_id": strings.ToUpper(facts.ProbeID), "stream_id": facts.StreamID, "certificate_fingerprint": facts.CertificateFingerprint},
					"facts_bad_stream":    {"state": "running", "probe_id": facts.ProbeID, "stream_id": "x", "certificate_fingerprint": facts.CertificateFingerprint},
					"facts_bad_pin":       {"state": "running", "probe_id": facts.ProbeID, "stream_id": facts.StreamID, "certificate_fingerprint": strings.Repeat("AB", 32)},
					"facts_short_pin":     {"state": "running", "probe_id": facts.ProbeID, "stream_id": facts.StreamID, "certificate_fingerprint": strings.Repeat("ab", 31)},
					"queued_with_facts":   {"probe_id": facts.ProbeID, "stream_id": facts.StreamID, "certificate_fingerprint": facts.CertificateFingerprint},
					"deadline_not_after":  {"deadline_at": created},
					"updated_before":      {"updated_at": created.Add(-time.Second)},
				} {
					if err := insertActivation(bad); err == nil {
						t.Fatalf("schema accepted a noncanonical activation row (%s)", name)
					}
					if n := m8ActCount(t, e.f, "probe_install_activations"); n != 0 {
						t.Fatalf("rejected insert %s left %d rows", name, n)
					}
				}
				// Positive controls for the shapes the store writes.
				if err := insertActivation(nil); err != nil {
					t.Fatalf("control activation refused: %v", err)
				}
				if err := insertActivation(nil); err == nil {
					t.Fatal("schema accepted a second activation for one operation")
				}
				withFacts := map[string]any{"operation_id": m8InstallOp2, "state": "running", "probe_id": facts.ProbeID, "stream_id": facts.StreamID, "certificate_fingerprint": facts.CertificateFingerprint}
				if err := insertActivation(withFacts); err != nil {
					t.Fatalf("running activation with facts refused: %v", err)
				}
				for _, row := range []map[string]any{
					{"operation_id": m8InstallOp3, "state": "failed", "error_code": "execution_failed"},
					{"operation_id": m8InstallOp4, "state": "canceled", "error_code": "canceled"},
				} {
					if err := insertActivation(row); err != nil {
						t.Fatalf("terminal activation %v refused: %v", row["state"], err)
					}
				}
				if n := m8ActCount(t, e.f, "probe_install_activations"); n != 4 {
					t.Fatalf("expected four control activations, got %d", n)
				}
				// Steps.
				stepColumns := []string{"operation_id", "kind", "ordinal", "reference", "owned", "state", "attempt_id", "error_code", "started_at", "finished_at"}
				stepBase := map[string]any{
					"operation_id": m8InstallOp1, "kind": "probe_init", "ordinal": 1, "reference": "/var/lib/x", "owned": 1,
					"state": "planned", "attempt_id": nil, "error_code": nil, "started_at": nil, "finished_at": nil,
				}
				insertStep := func(override map[string]any) error {
					values := make([]any, 0, len(stepColumns))
					for _, c := range stepColumns {
						v := stepBase[c]
						if o, ok := override[c]; ok {
							v = o
						}
						values = append(values, v)
					}
					_, err := e.f.db.ExecContext(ctx, "INSERT INTO probe_install_activation_steps ("+strings.Join(stepColumns, ", ")+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", values...)
					return err
				}
				started, finished := created, created.Add(time.Second)
				for name, bad := range map[string]map[string]any{
					"missing_activation": {"operation_id": m8InstallOp5},
					"kind_unknown":       {"kind": "bogus"},
					"kind_install":       {"kind": "data_directory"},
					"ordinal_mismatch":   {"ordinal": 2},
					"ordinal_range":      {"ordinal": 5},
					"owned_mismatch":     {"owned": 0},
					"reference_empty":    {"reference": ""},
					"state_unknown":      {"state": "bogus"},
					"planned_attempt":    {"attempt_id": m8ActForeignAttempt},
					"planned_started":    {"started_at": started},
					"in_flight_bare":     {"state": "in_flight"},
					"in_flight_finished": {"state": "in_flight", "attempt_id": m8ActForeignAttempt, "started_at": started, "finished_at": finished},
					"applied_error":      {"state": "applied", "attempt_id": m8ActForeignAttempt, "started_at": started, "finished_at": finished, "error_code": "internal"},
					"applied_unfinished": {"state": "applied", "attempt_id": m8ActForeignAttempt, "started_at": started},
					"applied_backwards":  {"state": "applied", "attempt_id": m8ActForeignAttempt, "started_at": finished, "finished_at": started},
					"not_started_code":   {"state": "not_started", "attempt_id": m8ActForeignAttempt, "started_at": started, "finished_at": finished, "error_code": "outcome_unknown"},
					"unknown_code":       {"state": "unknown", "attempt_id": m8ActForeignAttempt, "started_at": started, "finished_at": finished, "error_code": "not_started"},
					"unknown_no_code":    {"state": "unknown", "attempt_id": m8ActForeignAttempt, "started_at": started, "finished_at": finished},
					"attempt_short":      {"state": "in_flight", "attempt_id": "short", "started_at": started},
				} {
					if err := insertStep(bad); err == nil {
						t.Fatalf("schema accepted a noncanonical activation step (%s)", name)
					}
				}
				if n := m8ActCount(t, e.f, "probe_install_activation_steps"); n != 0 {
					t.Fatalf("rejected step inserts left %d rows", n)
				}
				plan := []map[string]any{
					{"kind": "probe_init", "ordinal": 1, "owned": 1},
					{"kind": "registration", "ordinal": 2, "owned": 0, "reference": "wss://192.0.2.10:8443/ws/probe/v1"},
					{"kind": "service_start", "ordinal": 3, "owned": 1},
					{"kind": "enrollment", "ordinal": 4, "owned": 0, "reference": "wss://192.0.2.10:8443/ws/probe/v1"},
				}
				for _, op := range []string{m8InstallOp1, m8InstallOp2} {
					for _, row := range plan {
						override := map[string]any{"operation_id": op}
						for k, v := range row {
							override[k] = v
						}
						if err := insertStep(override); err != nil {
							t.Fatalf("control step %v refused: %v", row["kind"], err)
						}
					}
				}
				if err := insertStep(nil); err == nil {
					t.Fatal("schema accepted a second step for one operation and kind")
				}
				// Attempt UUIDs are unique across steps; set them with UPDATE so the
				// state CHECK and the unique key are exercised in isolation.
				setAttempt := func(op, kind, attempt string) error {
					_, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_activation_steps SET state = 'in_flight', attempt_id = ?, started_at = ? WHERE operation_id = ? AND kind = ?", attempt, started, op, kind)
					return err
				}
				if err := setAttempt(m8InstallOp1, "probe_init", m8ActForeignAttempt); err != nil {
					t.Fatalf("control attempt refused: %v", err)
				}
				if err := setAttempt(m8InstallOp2, "probe_init", m8ActForeignAttempt); err == nil {
					t.Fatal("schema accepted a reused attempt across operations")
				}
				if err := setAttempt(m8InstallOp1, "registration", m8ActForeignAttempt); err == nil {
					t.Fatal("schema accepted a reused attempt inside one graph")
				}
				// Admissions.
				admissionColumns := []string{"operation_id", "owner_id", "attempt_id", "kind", "authority_key_id", "nonce_hex", "marker_sha256", "admitted_at"}
				authority := strings.Repeat("a", 64)
				admissionBase := map[string]any{
					"operation_id": m8InstallOp1, "owner_id": m8InstallOwnerA, "attempt_id": m8ActAttempt(m8InstallOp1, "probe_init"), "kind": "probe_init",
					"authority_key_id": authority, "nonce_hex": hex.EncodeToString(bytes.Repeat([]byte{1}, 32)),
					"marker_sha256": strings.Repeat("c", 64), "admitted_at": created,
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
					_, err := e.f.db.ExecContext(ctx, "INSERT INTO probe_activation_admissions ("+strings.Join(admissionColumns, ", ")+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)", values...)
					return err
				}
				for name, bad := range map[string]map[string]any{
					"owner_invalid":     {"owner_id": "invalid"},
					"owner_zero":        {"owner_id": "00000000-0000-0000-0000-000000000000"},
					"operation_invalid": {"operation_id": "not-a-uuid"},
					"attempt_invalid":   {"attempt_id": "x"},
					"authority_upper":   {"authority_key_id": strings.Repeat("A", 64)},
					"nonce_zero":        {"nonce_hex": strings.Repeat("0", 64)},
					"marker":            {"marker_sha256": "bad"},
					"kind_registration": {"kind": "registration"},
					"kind_enrollment":   {"kind": "enrollment"},
					"kind_install":      {"kind": "data_directory"},
					"kind_unit_file":    {"kind": "unit_file"},
					"kind_unknown":      {"kind": "bogus"},
					"kind_uppercase":    {"kind": "PROBE_INIT"},
					"missing_step":      {"operation_id": m8InstallOp3},
				} {
					if err := insertAdmission(bad); err == nil {
						t.Fatalf("schema accepted noncanonical admission metadata (%s)", name)
					}
				}
				if n := m8ActCount(t, e.f, "probe_activation_admissions"); n != 0 {
					t.Fatalf("rejected admission inserts left %d rows", n)
				}
				if err := insertAdmission(nil); err != nil {
					t.Fatalf("control admission refused: %v", err)
				}
				if err := insertAdmission(map[string]any{"kind": "service_start", "attempt_id": m8ActAttempt(m8InstallOp1, "service_start"), "nonce_hex": hex.EncodeToString(bytes.Repeat([]byte{2}, 32))}); err != nil {
					t.Fatalf("service_start admission refused: %v", err)
				}
				// A second operation owns free (operation, kind) pairs, so each
				// uniqueness constraint is isolated: only the targeted one can fail.
				freshNonce := hex.EncodeToString(bytes.Repeat([]byte{9}, 32))
				if insertAdmission(map[string]any{"attempt_id": m8ActAttempt(m8InstallOp2, "probe_init"), "nonce_hex": freshNonce}) == nil {
					t.Fatal("schema accepted a second admission for one operation and kind")
				}
				if insertAdmission(map[string]any{"operation_id": m8InstallOp2, "attempt_id": m8ActAttempt(m8InstallOp1, "probe_init"), "nonce_hex": freshNonce}) == nil {
					t.Fatal("schema accepted a reused attempt")
				}
				if insertAdmission(map[string]any{"operation_id": m8InstallOp2, "attempt_id": m8ActAttempt(m8InstallOp2, "probe_init")}) == nil {
					t.Fatal("schema accepted a reused authority nonce")
				}
				if n := m8ActCount(t, e.f, "probe_activation_admissions"); n != 2 {
					t.Fatalf("expected two admissions, got %d", n)
				}
				if err := insertAdmission(map[string]any{"operation_id": m8InstallOp2, "attempt_id": m8ActAttempt(m8InstallOp2, "probe_init"), "nonce_hex": freshNonce}); err != nil {
					t.Fatalf("control insert with fresh keys refused: %v", err)
				}
				// The restricting FKs: a consumed admission pins its step, the steps
				// pin their activation and the activation pins the install journal.
				if _, err := e.f.db.ExecContext(ctx, "DELETE FROM probe_install_activation_steps WHERE operation_id = ? AND kind = 'probe_init'", m8InstallOp1); err == nil {
					t.Fatal("deleted a step that has a consumed admission")
				}
				if _, err := e.f.db.ExecContext(ctx, "DELETE FROM probe_install_activations WHERE operation_id = ?", m8InstallOp1); err == nil {
					t.Fatal("deleted an activation that still has steps")
				}
				if _, err := e.f.db.ExecContext(ctx, "DELETE FROM probe_install_operations WHERE operation_id = ?", m8InstallOp3); err == nil {
					t.Fatal("deleted an install that still has an activation")
				}
				if n := m8ActCount(t, e.f, "probe_activation_admissions"); n != 3 {
					t.Fatalf("restricted deletes changed admissions: %d", n)
				}
			})
			t.Run("DowngradeGuard", func(t *testing.T) {
				const migration = "087_probe_install_activations"
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				// 087 is the newest migration and nothing references it, so it is the
				// first one a rollback would remove. Empty, it goes away cleanly.
				if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err != nil {
					t.Fatalf("empty activation downgrade refused: %v", err)
				}
				for _, table := range []string{"probe_install_activations", "probe_install_activation_steps", "probe_activation_admissions"} {
					if _, err := e.f.db.NewSelect().Table(table).Count(ctx); err == nil {
						t.Fatalf("empty downgrade did not remove %s", table)
					}
				}
				if err := runEngineMigration(t, e.f.db, engine, migration, "up"); err != nil {
					t.Fatalf("activation re-upgrade failed: %v", err)
				}
				t.Cleanup(func() {
					_, _ = e.f.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS probe_install_activations_downgrade_guard")
				})
				// Even one terminal activation is retained evidence: refuse.
				e.fresh(m8InstallOp1)
				if _, err := e.store.RequestActivationCancellation(ctx, m8InstallOp1); err != nil {
					t.Fatal(err)
				}
				if a := e.observe(m8InstallOp1); !a.Terminal() {
					t.Fatalf("fixture activation must be terminal: %+v", a)
				}
				if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err == nil {
					t.Fatal("downgrade erased a terminal activation")
				}
				// A nonterminal activation with an irreversible admission refuses too.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				e.reserve(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				signer := m8HostAuthority(t, 53)
				g, err := m8ActServiceOver(t, e.store, signer).Authorize(ctx, m8ActRequest(signer, m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit))
				if err != nil || !g.ValidFor(signer.PublicKey()) {
					t.Fatalf("fixture admission refused: %v", err)
				}
				if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err == nil {
					t.Fatal("downgrade erased an irreversible admission")
				}
				// Consumption and history survive the refused downgrade.
				if n := m8ActCount(t, e.f, "probe_activation_admissions"); n != 1 {
					t.Fatalf("refused downgrade changed admissions: %d", n)
				}
				again, err := m8ActServiceOver(t, e.store, signer).Authorize(ctx, m8ActRequest(signer, m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit))
				if err == nil || again != (domain.ProbeHostActionGrant{}) || !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("consumption did not survive the refused downgrade: %v", err)
				}
				if a := e.observe(m8InstallOp1); a.State != domain.ProbeActivationCanceled {
					t.Fatalf("history changed by the refused downgrade: %+v", a)
				}
				// Rows can only be removed child-first, and only by an explicit,
				// out-of-band cleanup, never by the migration itself.
				for _, table := range []string{"probe_activation_admissions", "probe_install_activation_steps", "probe_install_activations"} {
					if _, err := e.f.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
						t.Fatalf("explicit cleanup of %s: %v", table, err)
					}
				}
				if err := runEngineMigration(t, e.f.db, engine, migration, "down"); err != nil {
					t.Fatalf("downgrade of an emptied schema refused: %v", err)
				}
				if err := runEngineMigration(t, e.f.db, engine, migration, "up"); err != nil {
					t.Fatalf("restore of the activation schema failed: %v", err)
				}
			})
		})
	}
}
