package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	opInstallOwnerA  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	opInstallOwnerB  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	opInstallID      = "11111111-1111-4111-8111-111111111111"
	opInstallIDTwo   = "22222222-2222-4222-8222-222222222222"
	opInstallIDThree = "33333333-3333-4333-8333-333333333333"
	// opInstallHexID contains letters, so its uppercase form is a distinct string.
	opInstallHexID = "abcdef01-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

	// opInstallKeyMaterial stands in for the transient deployment key; it must
	// never surface in an identity, a receipt or an error.
	opInstallKeyMaterial = "transient-deployment-key-material"

	opInstallWait  = 10 * time.Second
	opInstallQuiet = 50 * time.Millisecond
)

var opInstallV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type opInstallCtxKey struct{}

func opInstallFingerprint(fill byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

// opInstallClientFingerprint is the PUBLIC fingerprint the fake factory reports
// for the transient key.
var opInstallClientFingerprint = opInstallFingerprint(0x5a)

func opInstallPin() domain.ProbeBinaryPin {
	return domain.ProbeBinaryPin{
		Version:      "1.2.3",
		Architecture: "amd64",
		SHA256:       strings.Repeat("ab", 32),
		Size:         4096,
	}
}

func opInstallSubmission(opID string, requestedBy int64) domain.ProbeInstallSubmission {
	return domain.ProbeInstallSubmission{
		OperationID:        opID,
		Host:               domain.ProbeSSHHost{Address: "edge.example:22", User: "root"},
		HostKeyFingerprint: opInstallFingerprint(0x01),
		PrivateKey:         []byte(opInstallKeyMaterial),
		Binary:             opInstallPin(),
		RequestedBy:        requestedBy,
	}
}

// opInstallIdentityFor derives the identity the service must hand the journal.
func opInstallIdentityFor(request domain.ProbeInstallSubmission, clientFingerprint string) domain.ProbeInstallIdentity {
	return domain.ProbeInstallIdentity{
		OperationID:          request.OperationID,
		Host:                 request.Host,
		HostKeyFingerprint:   request.HostKeyFingerprint,
		ClientKeyFingerprint: clientFingerprint,
		RequestedBy:          request.RequestedBy,
		LayoutVersion:        1,
		Mode:                 domain.ProbeInstallSystemd,
		Binary:               request.Binary,
	}
}

// opInstallUUID returns the i-th distinct canonical operation UUID (i >= 1).
func opInstallUUID(i int) string {
	return fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
}

// ---------------------------------------------------------------------------
// Shared ordered event log: every fake appends here, so "no I/O happened" is
// "the log is empty" and ordering claims are asserted on one timeline.
// ---------------------------------------------------------------------------

type opInstallEvents struct {
	mu   sync.Mutex
	list []string
}

func (e *opInstallEvents) add(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, fmt.Sprintf(format, args...))
}

func (e *opInstallEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

func (e *opInstallEvents) indexOf(name string) int {
	for i, event := range e.snapshot() {
		if event == name {
			return i
		}
	}
	return -1
}

func (e *opInstallEvents) count(name string) int {
	n := 0
	for _, event := range e.snapshot() {
		if event == name {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Fake session and factory.
// ---------------------------------------------------------------------------

type opInstallSession struct {
	id     int
	events *opInstallEvents

	// applyHook, when set, replaces the default "applied" report.
	applyHook func(ctx context.Context, kind string) (domain.ProbeInstallResourceOutcome, error)
	// preflightHook, when set, replaces the default qualifying report.
	preflightHook func(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error)

	mu               sync.Mutex
	preflights       []domain.ProbeSSHHost
	applied          []string
	closes           int
	closeErr         error
	closeCtxErr      error
	closeCalledAt    time.Time
	closeDeadline    time.Time
	closeHasDeadline bool
	closed           chan struct{}
}

func newOpInstallSession(id int, events *opInstallEvents) *opInstallSession {
	return &opInstallSession{id: id, events: events, closed: make(chan struct{})}
}

func (s *opInstallSession) ApplyResource(ctx context.Context, _ domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
	s.mu.Lock()
	s.applied = append(s.applied, kind)
	hook := s.applyHook
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx, kind)
	}
	return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
}

// opInstallQualifyingReport is a clean amd64 systemd host.
func opInstallQualifyingReport() domain.ProbeInstallHostReport {
	return domain.ProbeInstallHostReport{Kernel: "Linux", Machine: "x86_64", Systemd: true, Root: true, Authority: true}
}

func (s *opInstallSession) Preflight(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error) {
	s.mu.Lock()
	s.preflights = append(s.preflights, host)
	hook := s.preflightHook
	s.mu.Unlock()
	s.events.add("preflight")
	if hook != nil {
		return hook(ctx, host)
	}
	return opInstallQualifyingReport(), nil
}

func (s *opInstallSession) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closes++
	first := s.closes == 1
	if first {
		s.closeCtxErr = ctx.Err()
		s.closeCalledAt = time.Now()
		s.closeDeadline, s.closeHasDeadline = ctx.Deadline()
	}
	err := s.closeErr
	s.mu.Unlock()
	s.events.add("session_close:%d", s.id)
	if first {
		close(s.closed)
	}
	return err
}

func (s *opInstallSession) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func (s *opInstallSession) appliedKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.applied...)
}

type opInstallFactoryCall struct {
	key         []byte
	hostFP      string
	pin         domain.ProbeBinaryPin
	calledAt    time.Time
	deadline    time.Time
	hasDeadline bool
}

type opInstallFactory struct {
	events *opInstallEvents

	mu              sync.Mutex
	calls           []opInstallFactoryCall
	sessions        []*opInstallSession
	fingerprint     string
	err             error
	nilSession      bool
	typedNilSession bool
	closeErr        error
	gate            func(ctx context.Context) error
	applyHook       func(ctx context.Context, kind string) (domain.ProbeInstallResourceOutcome, error)
	preflightHook   func(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error)
}

func (f *opInstallFactory) NewInstallSession(ctx context.Context, privateKey []byte, hostFingerprint string, pin domain.ProbeBinaryPin) (ports.ProbeInstallSession, string, error) {
	f.events.add("factory")
	call := opInstallFactoryCall{key: bytes.Clone(privateKey), hostFP: hostFingerprint, pin: pin, calledAt: time.Now()}
	call.deadline, call.hasDeadline = ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, call)
	gate, err, fingerprint := f.gate, f.err, f.fingerprint
	nilSession, typedNil, applyHook, closeErr, preflightHook := f.nilSession, f.typedNilSession, f.applyHook, f.closeErr, f.preflightHook
	f.mu.Unlock()
	if gate != nil {
		if gateErr := gate(ctx); gateErr != nil {
			return nil, "", gateErr
		}
	}
	if err != nil {
		return nil, "", err
	}
	if nilSession {
		return nil, fingerprint, nil
	}
	if typedNil {
		var session *opInstallSession
		return session, fingerprint, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session := newOpInstallSession(len(f.sessions)+1, f.events)
	session.applyHook = applyHook
	session.preflightHook = preflightHook
	session.closeErr = closeErr
	f.sessions = append(f.sessions, session)
	return session, fingerprint, nil
}

func (f *opInstallFactory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *opInstallFactory) callAt(t *testing.T, i int) opInstallFactoryCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.calls) {
		t.Fatalf("factory call %d does not exist (have %d)", i, len(f.calls))
	}
	return f.calls[i]
}

func (f *opInstallFactory) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

// sessionAt is 0-based.
func (f *opInstallFactory) sessionAt(t *testing.T, i int) *opInstallSession {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.sessions) {
		t.Fatalf("session %d does not exist (have %d)", i, len(f.sessions))
	}
	return f.sessions[i]
}

func (f *opInstallFactory) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *opInstallFactory) setFingerprint(fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fingerprint = fingerprint
}

// ---------------------------------------------------------------------------
// Fake journal: an effectful in-memory store with the production exact-retry /
// conflict semantics, so tests assert persisted rows and not call counts alone.
// ---------------------------------------------------------------------------

type opInstallCreateCall struct {
	identity domain.ProbeInstallIdentity
	ownerID  string
}

type opInstallJournalCall struct {
	ctx context.Context
	id  string
}

type opInstallJournal struct {
	events *opInstallEvents

	mu       sync.Mutex
	rows     map[string]*domain.ProbeInstallOperation
	creates  []opInstallCreateCall
	observes []opInstallJournalCall
	cancels  []opInstallJournalCall

	createErr      error
	createCommits  bool
	createOverride func(domain.ProbeInstallIdentity, string) (*domain.ProbeInstallOperation, bool, error)
	createGate     func(ctx context.Context) error
	observeErr     error
	observeResult  *domain.ProbeInstallOperation
	cancelErr      error
	cancelResult   *domain.ProbeInstallOperation
}

func newOpInstallJournal(events *opInstallEvents) *opInstallJournal {
	return &opInstallJournal{events: events, rows: map[string]*domain.ProbeInstallOperation{}}
}

func opInstallCopy(o *domain.ProbeInstallOperation) *domain.ProbeInstallOperation {
	if o == nil {
		return nil
	}
	copied := *o
	copied.Resources = append([]domain.ProbeInstallResource(nil), o.Resources...)
	return &copied
}

func opInstallNewRow(identity domain.ProbeInstallIdentity, ownerID string) *domain.ProbeInstallOperation {
	now := time.Now().UTC().Truncate(time.Microsecond)
	specs := identity.Resources()
	resources := make([]domain.ProbeInstallResource, len(specs))
	for i, spec := range specs {
		resources[i] = domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned}
	}
	return &domain.ProbeInstallOperation{
		Identity:   identity,
		OwnerID:    ownerID,
		State:      domain.ProbeInstallQueued,
		CreatedAt:  now,
		UpdatedAt:  now,
		DeadlineAt: now.Add(domain.ProbeInstallLifetime),
		Resources:  resources,
	}
}

func (j *opInstallJournal) Create(ctx context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
	j.events.add("create")
	j.mu.Lock()
	j.creates = append(j.creates, opInstallCreateCall{identity: request, ownerID: ownerID})
	gate, override, createErr, commits := j.createGate, j.createOverride, j.createErr, j.createCommits
	j.mu.Unlock()
	if gate != nil {
		if err := gate(ctx); err != nil {
			return nil, false, err
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if override != nil {
		return override(request, ownerID)
	}
	if createErr != nil {
		if _, exists := j.rows[request.OperationID]; commits && !exists {
			j.rows[request.OperationID] = opInstallNewRow(request, ownerID)
		}
		return nil, false, createErr
	}
	if existing, ok := j.rows[request.OperationID]; ok {
		if existing.Identity != request {
			return nil, false, ports.ErrConflict
		}
		// The owner is not part of duplicate binding: the original is kept.
		return opInstallCopy(existing), false, nil
	}
	row := opInstallNewRow(request, ownerID)
	if !row.Valid() {
		return nil, false, domain.ErrValidation
	}
	j.rows[request.OperationID] = row
	return opInstallCopy(row), true, nil
}

func (j *opInstallJournal) Observe(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	j.events.add("observe")
	j.mu.Lock()
	defer j.mu.Unlock()
	j.observes = append(j.observes, opInstallJournalCall{ctx: ctx, id: operationID})
	if j.observeErr != nil {
		return nil, j.observeErr
	}
	if j.observeResult != nil {
		return j.observeResult, nil
	}
	row, ok := j.rows[operationID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return opInstallCopy(row), nil
}

func (j *opInstallJournal) Cancel(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	j.events.add("cancel")
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cancels = append(j.cancels, opInstallJournalCall{ctx: ctx, id: operationID})
	if j.cancelErr != nil {
		return nil, j.cancelErr
	}
	if j.cancelResult != nil {
		return j.cancelResult, nil
	}
	row, ok := j.rows[operationID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	if !row.Terminal() {
		row.State = domain.ProbeInstallCancelRequested
	}
	return opInstallCopy(row), nil
}

// seed commits a row as another process would have, without a Create call.
func (j *opInstallJournal) seed(identity domain.ProbeInstallIdentity, ownerID, state string) *domain.ProbeInstallOperation {
	j.mu.Lock()
	defer j.mu.Unlock()
	row := opInstallNewRow(identity, ownerID)
	row.State = state
	j.rows[identity.OperationID] = row
	return opInstallCopy(row)
}

func (j *opInstallJournal) mutate(operationID string, fn func(*domain.ProbeInstallOperation)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if row, ok := j.rows[operationID]; ok {
		fn(row)
	}
}

func (j *opInstallJournal) row(operationID string) *domain.ProbeInstallOperation {
	j.mu.Lock()
	defer j.mu.Unlock()
	return opInstallCopy(j.rows[operationID])
}

func (j *opInstallJournal) rowCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.rows)
}

func (j *opInstallJournal) createCalls() []opInstallCreateCall {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]opInstallCreateCall(nil), j.creates...)
}

func (j *opInstallJournal) observeCalls() []opInstallJournalCall {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]opInstallJournalCall(nil), j.observes...)
}

func (j *opInstallJournal) cancelCalls() []opInstallJournalCall {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]opInstallJournalCall(nil), j.cancels...)
}

func (j *opInstallJournal) clearFaults() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.createErr, j.createCommits, j.createOverride, j.createGate = nil, false, nil, nil
	j.observeErr, j.cancelErr = nil, nil
}

// ---------------------------------------------------------------------------
// Fake runner.
// ---------------------------------------------------------------------------

type opInstallRunCall struct {
	ctx           context.Context
	expected      domain.ProbeInstallIdentity
	ownerID       string
	attempts      []string
	ctxErrAtStart error
}

type opInstallRunner struct {
	events *opInstallEvents

	// hold, when non-nil, keeps Run from returning until released (or, unless
	// ignoreCancel is set, until the run context is canceled).
	hold         chan struct{}
	ignoreCancel bool
	// afterCancel, when non-nil, models a runner that records its honest
	// outcome after cancellation: Run waits for finish() before returning.
	afterCancel chan struct{}
	result      *domain.ProbeInstallOperation
	err         error

	started  chan struct{}
	canceled chan struct{}
	done     chan struct{}

	mu                              sync.Mutex
	calls                           []opInstallRunCall
	startOnce, cancelOnce, doneOnce sync.Once
	holdOnce, afterOnce             sync.Once
}

func newOpInstallRunner(events *opInstallEvents) *opInstallRunner {
	return &opInstallRunner{
		events:   events,
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (r *opInstallRunner) Run(ctx context.Context, expected domain.ProbeInstallIdentity, ownerID string, attempts []string) (*domain.ProbeInstallOperation, error) {
	r.mu.Lock()
	r.calls = append(r.calls, opInstallRunCall{
		ctx: ctx, expected: expected, ownerID: ownerID,
		attempts: append([]string(nil), attempts...), ctxErrAtStart: ctx.Err(),
	})
	r.mu.Unlock()
	r.events.add("run_start")
	r.startOnce.Do(func() { close(r.started) })
	if r.hold != nil {
		if r.ignoreCancel {
			<-r.hold
		} else {
			select {
			case <-r.hold:
			case <-ctx.Done():
				r.cancelOnce.Do(func() { close(r.canceled) })
				if r.afterCancel != nil {
					<-r.afterCancel
				}
			}
		}
	}
	r.events.add("run_return")
	r.doneOnce.Do(func() { close(r.done) })
	return r.result, r.err
}

func (r *opInstallRunner) release() {
	if r.hold != nil {
		r.holdOnce.Do(func() { close(r.hold) })
	}
}

func (r *opInstallRunner) finish() {
	if r.afterCancel != nil {
		r.afterOnce.Do(func() { close(r.afterCancel) })
	}
}

func (r *opInstallRunner) releaseAll() {
	r.release()
	r.finish()
}

func (r *opInstallRunner) runCalls() []opInstallRunCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]opInstallRunCall(nil), r.calls...)
}

// ---------------------------------------------------------------------------
// Environment.
// ---------------------------------------------------------------------------

type opInstallEnv struct {
	svc     *ProbeInstallOperationService
	journal *opInstallJournal
	factory *opInstallFactory
	events  *opInstallEvents

	mu           sync.Mutex
	runners      []*opInstallRunner
	executors    []ports.ProbeInstallResourceExecutor
	mkRunner     func(n int) *opInstallRunner
	newRunnerErr error
	newRunnerNil bool
}

func (e *opInstallEnv) newRunner(executor ports.ProbeInstallResourceExecutor) (probeInstallRunner, error) {
	e.events.add("new_runner")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.executors = append(e.executors, executor)
	if e.newRunnerErr != nil {
		return nil, e.newRunnerErr
	}
	if e.newRunnerNil {
		var runner *opInstallRunner
		return runner, nil
	}
	runner := e.mkRunner(len(e.runners))
	e.runners = append(e.runners, runner)
	return runner, nil
}

func (e *opInstallEnv) setRunners(mk func(n int) *opInstallRunner) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mkRunner = mk
}

func (e *opInstallEnv) setNewRunnerFault(err error, typedNil bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.newRunnerErr, e.newRunnerNil = err, typedNil
}

// runnerAt is 0-based.
func (e *opInstallEnv) runnerAt(t *testing.T, i int) *opInstallRunner {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= len(e.runners) {
		t.Fatalf("runner %d was never constructed (have %d)", i, len(e.runners))
	}
	return e.runners[i]
}

func (e *opInstallEnv) runnerCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.runners)
}

func (e *opInstallEnv) executorCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.executors)
}

func (e *opInstallEnv) active() int {
	e.svc.mu.Lock()
	defer e.svc.mu.Unlock()
	return e.svc.active
}

// totalRuns counts Run invocations across every constructed runner.
func (e *opInstallEnv) totalRuns() int {
	e.mu.Lock()
	runners := append([]*opInstallRunner(nil), e.runners...)
	e.mu.Unlock()
	total := 0
	for _, runner := range runners {
		total += len(runner.runCalls())
	}
	return total
}

func opInstallEnvFor(t *testing.T, ctx context.Context, capacity int) *opInstallEnv {
	t.Helper()
	events := &opInstallEvents{}
	env := &opInstallEnv{
		journal: newOpInstallJournal(events),
		factory: &opInstallFactory{events: events, fingerprint: opInstallClientFingerprint},
		events:  events,
	}
	env.mkRunner = func(int) *opInstallRunner { return newOpInstallRunner(events) }
	svc, err := newProbeInstallOperationService(ctx, env.journal, env.newRunner, env.factory, opInstallOwnerA, capacity)
	if err != nil || svc == nil {
		t.Fatalf("constructing service: %v", err)
	}
	env.svc = svc
	t.Cleanup(func() {
		env.mu.Lock()
		runners := append([]*opInstallRunner(nil), env.runners...)
		env.mu.Unlock()
		for _, runner := range runners {
			runner.releaseAll()
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), opInstallWait)
		defer cancel()
		if err := svc.Close(closeCtx); err != nil {
			t.Errorf("closing service: %v", err)
		}
	})
	return env
}

func opInstallAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(opInstallWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// opInstallStaysOpen reports whether ch is still open after a short bounded
// wait. It backs only negative assertions ("has not happened yet").
func opInstallStaysOpen(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	case <-time.After(opInstallQuiet):
		return true
	}
}

func opInstallPoll(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(opInstallWait)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func opInstallAwaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(opInstallWait):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// assertNoIO proves nothing at all was attempted: no factory, journal, runner
// or session activity, and no slot is held.
func (e *opInstallEnv) assertNoIO(t *testing.T) {
	t.Helper()
	if events := e.events.snapshot(); len(events) != 0 {
		t.Fatalf("expected no I/O, got events %v", events)
	}
	if e.active() != 0 {
		t.Fatalf("slot held after a refused request: active=%d", e.active())
	}
}

// assertSessionClosedOnce checks the 0-based session was closed exactly once.
func (e *opInstallEnv) assertSessionClosedOnce(t *testing.T, i int) {
	t.Helper()
	if closes := e.factory.sessionAt(t, i).closeCount(); closes != 1 {
		t.Fatalf("session %d closed %d times, want exactly 1", i, closes)
	}
}

// assertNothingDispatched checks no runner was built and no run started.
func (e *opInstallEnv) assertNothingDispatched(t *testing.T) {
	t.Helper()
	if n := e.executorCalls(); n != 0 {
		t.Fatalf("runner constructed %d times, want 0", n)
	}
	if n := e.events.count("run_start"); n != 0 {
		t.Fatalf("Run invoked %d times, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 1. Constructor.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceConstructorRefusals(t *testing.T) {
	events := &opInstallEvents{}
	journal := newOpInstallJournal(events)
	factory := &opInstallFactory{events: events, fingerprint: opInstallClientFingerprint}
	runnerFn := func(ports.ProbeInstallResourceExecutor) (probeInstallRunner, error) {
		return newOpInstallRunner(events), nil
	}
	var typedNilJournal *opInstallJournal
	var typedNilFactory *opInstallFactory
	var nilCtx context.Context

	for _, tc := range []struct {
		name      string
		ctx       context.Context
		journal   probeInstallJournal
		newRunner func(ports.ProbeInstallResourceExecutor) (probeInstallRunner, error)
		sessions  ports.ProbeInstallSessionFactory
		ownerID   string
		capacity  int
	}{
		{"nil_context", nilCtx, journal, runnerFn, factory, opInstallOwnerA, 2},
		{"nil_journal", t.Context(), nil, runnerFn, factory, opInstallOwnerA, 2},
		{"typed_nil_journal", t.Context(), typedNilJournal, runnerFn, factory, opInstallOwnerA, 2},
		{"nil_runner_factory", t.Context(), journal, nil, factory, opInstallOwnerA, 2},
		{"nil_session_factory", t.Context(), journal, runnerFn, nil, opInstallOwnerA, 2},
		{"typed_nil_session_factory", t.Context(), journal, runnerFn, typedNilFactory, opInstallOwnerA, 2},
		{"empty_owner", t.Context(), journal, runnerFn, factory, "", 2},
		{"malformed_owner", t.Context(), journal, runnerFn, factory, "not-a-uuid", 2},
		{"uppercase_owner", t.Context(), journal, runnerFn, factory, strings.ToUpper(opInstallOwnerA), 2},
		{"reserved_local_owner", t.Context(), journal, runnerFn, factory, domain.LocalProbeID, 2},
		{"zero_capacity", t.Context(), journal, runnerFn, factory, opInstallOwnerA, 0},
		{"negative_capacity", t.Context(), journal, runnerFn, factory, opInstallOwnerA, -1},
		{"excessive_capacity", t.Context(), journal, runnerFn, factory, opInstallOwnerA, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built, err := newProbeInstallOperationService(tc.ctx, tc.journal, tc.newRunner, tc.sessions, tc.ownerID, tc.capacity)
			if built != nil || !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("service=%v error=%v, want nil + ErrValidation", built, err)
			}
		})
	}
	if events := events.snapshot(); len(events) != 0 {
		t.Fatalf("refused constructors performed I/O: %v", events)
	}
}

func TestProbeInstallOperationServiceConstructorAcceptsCapacityOneToFour(t *testing.T) {
	for capacity := 1; capacity <= 4; capacity++ {
		t.Run(fmt.Sprintf("capacity_%d", capacity), func(t *testing.T) {
			events := &opInstallEvents{}
			svc, err := newProbeInstallOperationService(
				context.Background(),
				newOpInstallJournal(events),
				func(ports.ProbeInstallResourceExecutor) (probeInstallRunner, error) {
					return newOpInstallRunner(events), nil
				},
				&opInstallFactory{events: events, fingerprint: opInstallClientFingerprint},
				opInstallOwnerA, capacity,
			)
			if err != nil || svc == nil {
				t.Fatalf("service=%v error=%v", svc, err)
			}
			if svc.capacity != capacity || svc.ownerID != opInstallOwnerA {
				t.Fatalf("capacity=%d owner=%q", svc.capacity, svc.ownerID)
			}
			if err := svc.rootCtx.Err(); err != nil {
				t.Fatalf("a fresh service must be admitting: %v", err)
			}
			if err := svc.Close(t.Context()); err != nil {
				t.Fatalf("close: %v", err)
			}
		})
	}
}

func TestProbeInstallOperationServiceExportedConstructorRefusesNilJournal(t *testing.T) {
	factory := &opInstallFactory{events: &opInstallEvents{}, fingerprint: opInstallClientFingerprint}
	if built, err := NewProbeInstallOperationService(t.Context(), nil, factory, opInstallOwnerA, 2); built != nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("service=%v error=%v, want nil + ErrValidation", built, err)
	}
}

// ---------------------------------------------------------------------------
// 2. Submit validates before any I/O.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceSubmitValidatesBeforeAnyIO(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ProbeInstallSubmission)
	}{
		{"user_not_root", func(r *domain.ProbeInstallSubmission) { r.Host.User = "deploy" }},
		{"user_case_variant", func(r *domain.ProbeInstallSubmission) { r.Host.User = "Root" }},
		{"user_empty", func(r *domain.ProbeInstallSubmission) { r.Host.User = "" }},
		{"user_trailing_space", func(r *domain.ProbeInstallSubmission) { r.Host.User = "root " }},
		{"host_address_empty", func(r *domain.ProbeInstallSubmission) { r.Host.Address = "" }},
		{"host_address_without_port", func(r *domain.ProbeInstallSubmission) { r.Host.Address = "edge.example" }},
		{"operation_id_malformed", func(r *domain.ProbeInstallSubmission) { r.OperationID = "not-a-uuid" }},
		{"operation_id_empty", func(r *domain.ProbeInstallSubmission) { r.OperationID = "" }},
		{"operation_id_uppercase", func(r *domain.ProbeInstallSubmission) { r.OperationID = strings.ToUpper(opInstallHexID) }},
		{"operation_id_nil_uuid", func(r *domain.ProbeInstallSubmission) { r.OperationID = "00000000-0000-0000-0000-000000000000" }},
		{"operation_id_reserved_local", func(r *domain.ProbeInstallSubmission) { r.OperationID = domain.LocalProbeID }},
		{"host_key_fingerprint_empty", func(r *domain.ProbeInstallSubmission) { r.HostKeyFingerprint = "" }},
		{"host_key_fingerprint_wrong_scheme", func(r *domain.ProbeInstallSubmission) { r.HostKeyFingerprint = "MD5:00:11" }},
		{"host_key_fingerprint_truncated", func(r *domain.ProbeInstallSubmission) { r.HostKeyFingerprint = "SHA256:AAAA" }},
		{"pin_zero_value", func(r *domain.ProbeInstallSubmission) { r.Binary = domain.ProbeBinaryPin{} }},
		{"pin_bad_version", func(r *domain.ProbeInstallSubmission) { r.Binary.Version = "v1" }},
		{"pin_bad_architecture", func(r *domain.ProbeInstallSubmission) { r.Binary.Architecture = "riscv64" }},
		{"pin_short_sha256", func(r *domain.ProbeInstallSubmission) { r.Binary.SHA256 = "abcd" }},
		{"pin_uppercase_sha256", func(r *domain.ProbeInstallSubmission) { r.Binary.SHA256 = strings.ToUpper(r.Binary.SHA256) }},
		{"pin_non_hex_sha256", func(r *domain.ProbeInstallSubmission) { r.Binary.SHA256 = strings.Repeat("zz", 32) }},
		{"pin_zero_size", func(r *domain.ProbeInstallSubmission) { r.Binary.Size = 0 }},
		{"pin_negative_size", func(r *domain.ProbeInstallSubmission) { r.Binary.Size = -1 }},
		{"pin_oversized", func(r *domain.ProbeInstallSubmission) { r.Binary.Size = domain.MaxProbeBinarySize + 1 }},
		{"key_nil", func(r *domain.ProbeInstallSubmission) { r.PrivateKey = nil }},
		{"key_empty", func(r *domain.ProbeInstallSubmission) { r.PrivateKey = []byte{} }},
		{"key_oversized", func(r *domain.ProbeInstallSubmission) {
			r.PrivateKey = make([]byte, domain.MaxProbeInstallPrivateKeySize+1)
		}},
		{"requested_by_zero", func(r *domain.ProbeInstallSubmission) { r.RequestedBy = 0 }},
		{"requested_by_negative", func(r *domain.ProbeInstallSubmission) { r.RequestedBy = -7 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 4)
			request := opInstallSubmission(opInstallID, 7)
			tc.mutate(&request)
			operation, err := env.svc.Submit(t.Context(), request)
			if operation != nil || !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("operation=%+v error=%v, want nil + ErrValidation", operation, err)
			}
			env.assertNoIO(t)
		})
	}
}

func TestProbeInstallOperationServiceSubmitRefusesUnusableContextBeforeAnyIO(t *testing.T) {
	var nilCtx context.Context
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"nil_context", nilCtx, domain.ErrValidation},
		{"canceled_context", canceled, context.Canceled},
		{"expired_context", expired, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 4)
			operation, err := env.svc.Submit(tc.ctx, opInstallSubmission(opInstallID, 7))
			if operation != nil || !errors.Is(err, tc.want) {
				t.Fatalf("operation=%+v error=%v, want nil + %v", operation, err, tc.want)
			}
			env.assertNoIO(t)
		})
	}
}

func TestProbeInstallOperationServiceSubmitAcceptsMaximumKeySize(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	request := opInstallSubmission(opInstallID, 7)
	request.PrivateKey = bytes.Repeat([]byte{'k'}, domain.MaxProbeInstallPrivateKeySize)
	operation, err := env.svc.Submit(t.Context(), request)
	if err != nil || operation == nil {
		t.Fatalf("operation=%+v error=%v: a key of exactly the maximum size must be admitted", operation, err)
	}
	if call := env.factory.callAt(t, 0); len(call.key) != domain.MaxProbeInstallPrivateKeySize {
		t.Fatalf("factory saw a %d byte key", len(call.key))
	}
	opInstallAwait(t, env.runnerAt(t, 0).done, "the run to finish")
}

// ---------------------------------------------------------------------------
// 3. Session failures create nothing.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceSessionFailuresCreateNothing(t *testing.T) {
	canary := errors.New("canary /home/ops/.ssh/id_ed25519 passphrase=hunter2")
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"artifact_rejected", domain.ErrProbeInstallArtifact, domain.ErrProbeInstallArtifact},
		{"ssh_input_rejected", domain.ErrProbeSSHInput, domain.ErrProbeSSHInput},
		{"wrapped_artifact_with_canary", fmt.Errorf("verify %s: %w", canary, domain.ErrProbeInstallArtifact), domain.ErrProbeInstallArtifact},
		{"joined_ssh_input_with_canary", errors.Join(canary, domain.ErrProbeSSHInput), domain.ErrProbeSSHInput},
		{"arbitrary_error_with_canary", canary, domain.ErrInternal},
		{"wrapped_arbitrary_error", fmt.Errorf("open artifact: %w", canary), domain.ErrInternal},
		{"not_found_is_not_leaked", ports.ErrNotFound, domain.ErrInternal},
		{"conflict_is_not_leaked", ports.ErrConflict, domain.ErrInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 1)
			env.factory.setErr(tc.err)
			operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
			if operation != nil || !errors.Is(err, tc.want) {
				t.Fatalf("operation=%+v error=%v, want nil + %v", operation, err, tc.want)
			}
			if err != tc.want {
				t.Fatalf("error %q must be the bare sentinel %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("session failure leaked its message: %q", err)
			}
			if errors.Is(err, canary) {
				t.Fatal("session failure still wraps the arbitrary error")
			}
			// Nothing durable and nothing dispatched.
			if got := env.journal.createCalls(); len(got) != 0 {
				t.Fatalf("journal Create called for a failed session: %+v", got)
			}
			if got := env.journal.rowCount(); got != 0 {
				t.Fatalf("a row exists after a session failure: %d", got)
			}
			env.assertNothingDispatched(t)
			if env.active() != 0 {
				t.Fatalf("slot leaked: active=%d", env.active())
			}
			// The slot was released: with capacity 1 the next submit is admitted.
			env.factory.setErr(nil)
			next, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7))
			if err != nil || next == nil {
				t.Fatalf("follow-up submit: operation=%+v error=%v", next, err)
			}
			opInstallAwait(t, env.runnerAt(t, 0).done, "the follow-up run to finish")
		})
	}
}

func TestProbeInstallOperationServiceSessionFactoryContextErrorsPassThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"deadline_exceeded", context.DeadlineExceeded},
		{"validation", domain.ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 2)
			env.factory.setErr(tc.err)
			if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != tc.err {
				t.Fatalf("error=%v, want exactly %v", err, tc.err)
			}
			if env.journal.rowCount() != 0 || env.active() != 0 {
				t.Fatalf("rows=%d active=%d", env.journal.rowCount(), env.active())
			}
		})
	}
}

func TestProbeInstallOperationServiceUnusableSessionIsInternalAndCreatesNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setup       func(*opInstallFactory)
		wantSession bool // a real session was handed out and must be closed
	}{
		{"nil_session_without_error", func(f *opInstallFactory) { f.nilSession = true }, false},
		{"typed_nil_session_without_error", func(f *opInstallFactory) { f.typedNilSession = true }, false},
		{"malformed_client_fingerprint", func(f *opInstallFactory) { f.fingerprint = "SHA256:not-a-fingerprint" }, true},
		{"empty_client_fingerprint", func(f *opInstallFactory) { f.fingerprint = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 1)
			tc.setup(env.factory)
			operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
			if operation != nil || !errors.Is(err, domain.ErrInternal) {
				t.Fatalf("operation=%+v error=%v, want nil + ErrInternal", operation, err)
			}
			if got := env.journal.createCalls(); len(got) != 0 {
				t.Fatalf("journal Create called with an invalid identity: %+v", got)
			}
			env.assertNothingDispatched(t)
			if env.active() != 0 {
				t.Fatalf("slot leaked: active=%d", env.active())
			}
			if tc.wantSession {
				env.assertSessionClosedOnce(t, 0)
			} else if env.factory.sessionCount() != 0 {
				t.Fatalf("unexpected session count %d", env.factory.sessionCount())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Fresh create.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceFreshCreateDispatchesExactlyOneRun(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	held := newOpInstallRunner(env.events)
	held.hold = make(chan struct{})
	env.setRunners(func(int) *opInstallRunner { return held })

	request := opInstallSubmission(opInstallID, 7)
	wantIdentity := opInstallIdentityFor(request, opInstallClientFingerprint)
	operation, err := env.svc.Submit(t.Context(), request)
	if err != nil || operation == nil {
		t.Fatalf("operation=%+v error=%v", operation, err)
	}
	// The receipt is the journal's committed row.
	if operation.Identity != wantIdentity || operation.State != domain.ProbeInstallQueued || operation.OwnerID != opInstallOwnerA {
		t.Fatalf("receipt %+v", operation)
	}
	opInstallAwait(t, held.started, "the run to start")

	// Session factory input: the exact key, host pin and binary pin, on a
	// bounded admission context.
	if env.factory.callCount() != 1 {
		t.Fatalf("factory called %d times, want 1", env.factory.callCount())
	}
	call := env.factory.callAt(t, 0)
	if string(call.key) != opInstallKeyMaterial || call.hostFP != request.HostKeyFingerprint || call.pin != request.Binary {
		t.Fatalf("factory input %+v", call)
	}
	if remaining := call.deadline.Sub(call.calledAt); !call.hasDeadline || remaining <= 0 || remaining > probeInstallSubmitTTL {
		t.Fatalf("admission context deadline %v outside (0, %v]", remaining, probeInstallSubmitTTL)
	}

	// The identity committed to the journal.
	creates := env.journal.createCalls()
	if len(creates) != 1 || creates[0].ownerID != opInstallOwnerA {
		t.Fatalf("journal creates %+v", creates)
	}
	got := creates[0].identity
	if got != wantIdentity {
		t.Fatalf("identity\n got  %+v\n want %+v", got, wantIdentity)
	}
	if got.Mode != domain.ProbeInstallSystemd || got.LayoutVersion != 1 {
		t.Fatalf("mode=%q layout=%d", got.Mode, got.LayoutVersion)
	}
	if got.ClientKeyFingerprint != opInstallClientFingerprint || got.RequestedBy != 7 ||
		got.Host != request.Host || got.Binary != request.Binary || got.Image != (domain.ProbeImageSelection{}) {
		t.Fatalf("identity fields %+v", got)
	}
	if strings.Contains(fmt.Sprintf("%+v %+v", got, operation), opInstallKeyMaterial) {
		t.Fatal("key material reached the identity or the receipt")
	}
	if row := env.journal.row(opInstallID); row == nil || row.Identity != wantIdentity {
		t.Fatalf("persisted row %+v", row)
	}

	// The runner was built over exactly this session.
	session := env.factory.sessionAt(t, 0)
	if env.executorCalls() != 1 {
		t.Fatalf("runner constructed %d times, want 1", env.executorCalls())
	}
	env.mu.Lock()
	executor := env.executors[0]
	env.mu.Unlock()
	if executor != ports.ProbeInstallResourceExecutor(session) {
		t.Fatalf("runner built over %v, want the created session", executor)
	}

	// Run: once, service owner, exact identity, four distinct canonical attempts.
	runs := held.runCalls()
	if len(runs) != 1 {
		t.Fatalf("Run invoked %d times, want 1", len(runs))
	}
	run := runs[0]
	if run.ownerID != opInstallOwnerA || run.expected != wantIdentity || run.ctxErrAtStart != nil {
		t.Fatalf("run call %+v", run)
	}
	if len(run.attempts) != 4 || len(run.attempts) != len(wantIdentity.Resources()) {
		t.Fatalf("attempts %v, want 4 (one per resource)", run.attempts)
	}
	seen := map[string]bool{}
	for _, attempt := range run.attempts {
		if !opInstallV4.MatchString(attempt) || !domain.ValidProbeOperationID(attempt) || seen[attempt] || attempt == opInstallID {
			t.Fatalf("attempt %q is not a distinct canonical v4 UUID in %v", attempt, run.attempts)
		}
		seen[attempt] = true
	}

	// While the run is held: the session is still open and the slot is taken.
	if session.closeCount() != 0 {
		t.Fatalf("session closed %d times before Run returned", session.closeCount())
	}
	if env.active() != 1 {
		t.Fatalf("active=%d while the run is held, want 1", env.active())
	}
	// Capacity 1: a second distinct submit is Busy and creates nothing.
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7)); !errors.Is(err, domain.ErrProbeInstallBusy) {
		t.Fatalf("second submit while held: %v, want ErrProbeInstallBusy", err)
	}
	if row := env.journal.row(opInstallIDTwo); row != nil {
		t.Fatalf("a busy submit committed a row: %+v", row)
	}

	held.release()
	opInstallAwait(t, held.done, "the run to return")
	opInstallAwait(t, session.closed, "the session to close")

	// Closed exactly once, strictly after Run returned.
	opInstallPoll(t, "the slot to be released", func() bool { return env.active() == 0 })
	if closes := session.closeCount(); closes != 1 {
		t.Fatalf("session closed %d times, want 1", closes)
	}
	runReturn, sessionClose := env.events.indexOf("run_return"), env.events.indexOf("session_close:1")
	if runReturn < 0 || sessionClose < 0 || runReturn > sessionClose {
		t.Fatalf("session must close after Run returns: %v", env.events.snapshot())
	}
	if env.events.count("run_start") != 1 || env.events.count("new_runner") != 1 {
		t.Fatalf("dispatch events %v", env.events.snapshot())
	}
	if remaining := session.closeDeadline.Sub(session.closeCalledAt); session.closeCtxErr != nil || !session.closeHasDeadline ||
		remaining <= 0 || remaining > probeInstallCloseTTL {
		t.Fatalf("session closed on a stale or unbounded context: err=%v remaining=%v", session.closeCtxErr, remaining)
	}
	// The slot freed by the finished run admits the next distinct request.
	next, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7))
	if err != nil || next == nil || next.Identity.OperationID != opInstallIDTwo {
		t.Fatalf("submit after release: operation=%+v error=%v", next, err)
	}
}

func TestProbeInstallOperationServiceRequestContextDoesNotBoundTheRun(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 2)
	held := newOpInstallRunner(env.events)
	held.hold = make(chan struct{})
	env.setRunners(func(int) *opInstallRunner { return held })

	requestCtx, disconnect := context.WithCancel(t.Context())
	if _, err := env.svc.Submit(requestCtx, opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, held.started, "the run to start")
	disconnect()
	if !opInstallStaysOpen(held.canceled) {
		t.Fatal("client disconnect canceled the run")
	}
	if err := held.runCalls()[0].ctx.Err(); err != nil {
		t.Fatalf("run context canceled by the request: %v", err)
	}
	if env.factory.sessionAt(t, 0).closeCount() != 0 {
		t.Fatal("client disconnect closed the session while the run was live")
	}
	held.release()
	opInstallAwait(t, env.factory.sessionAt(t, 0).closed, "the session to close")
}

// ---------------------------------------------------------------------------
// 5. Exact retry (created=false).
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceExactRetryReturnsStoredReceiptWithoutDispatch(t *testing.T) {
	for _, state := range []string{
		domain.ProbeInstallQueued,
		domain.ProbeInstallRunning,
		domain.ProbeInstallCancelRequested,
		domain.ProbeInstallResourcesReady,
		domain.ProbeInstallFailed,
		domain.ProbeInstallCanceled,
		domain.ProbeInstallInterrupted,
	} {
		t.Run(state, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 1)
			request := opInstallSubmission(opInstallID, 7)
			stored := env.journal.seed(opInstallIdentityFor(request, opInstallClientFingerprint), opInstallOwnerB, state)

			// Repeated retries never consume the single slot.
			for attempt := 0; attempt < 3; attempt++ {
				operation, err := env.svc.Submit(t.Context(), request)
				if err != nil || operation == nil {
					t.Fatalf("retry %d: operation=%+v error=%v", attempt, operation, err)
				}
				if operation.State != state || operation.OwnerID != opInstallOwnerB || operation.Identity != stored.Identity ||
					!operation.CreatedAt.Equal(stored.CreatedAt) || !operation.DeadlineAt.Equal(stored.DeadlineAt) {
					t.Fatalf("retry must return the stored receipt unchanged: %+v vs %+v", operation, stored)
				}
				// Closed immediately, before Submit returned.
				env.assertSessionClosedOnce(t, attempt)
				if env.active() != 0 {
					t.Fatalf("active=%d after a retry", env.active())
				}
			}
			// Committed work is answered from the journal: no Create and no
			// host preflight (its own resources would read as existing).
			if creates := env.journal.createCalls(); len(creates) != 0 {
				t.Fatalf("journal Create calls=%d, want 0", len(creates))
			}
			if observes := env.journal.observeCalls(); len(observes) != 3 {
				t.Fatalf("journal Observe calls=%d, want 3 (one per retry)", len(observes))
			}
			if env.events.count("preflight") != 0 {
				t.Fatalf("a retry preflighted the host: %v", env.events.snapshot())
			}
			env.assertNothingDispatched(t)
			if env.factory.sessionCount() != 3 {
				t.Fatalf("sessions=%d", env.factory.sessionCount())
			}
		})
	}
}

func TestProbeInstallOperationServiceRetryAfterRunNeverDispatchesTwice(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	request := opInstallSubmission(opInstallID, 7)
	first, err := env.svc.Submit(t.Context(), request)
	if err != nil || first == nil {
		t.Fatalf("first submit: operation=%+v error=%v", first, err)
	}
	opInstallAwait(t, env.runnerAt(t, 0).done, "the first run to return")
	opInstallAwait(t, env.factory.sessionAt(t, 0).closed, "the first session to close")
	opInstallPoll(t, "the slot to be released", func() bool { return env.active() == 0 })

	second, err := env.svc.Submit(t.Context(), request)
	if err != nil || second == nil {
		t.Fatalf("retry: operation=%+v error=%v", second, err)
	}
	if second.Identity != first.Identity || second.OwnerID != first.OwnerID || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("retry receipt %+v differs from %+v", second, first)
	}
	env.assertSessionClosedOnce(t, 1)
	if env.runnerCount() != 1 || env.events.count("new_runner") != 1 || env.totalRuns() != 1 {
		t.Fatalf("retry dispatched again: %v", env.events.snapshot())
	}
	if creates := env.journal.createCalls(); len(creates) != 1 {
		t.Fatalf("journal creates=%d, want 1", len(creates))
	}
	if env.events.count("preflight") != 1 {
		t.Fatalf("preflights %v, want only the first submit's", env.events.snapshot())
	}
	env.assertSessionClosedOnce(t, 0)
}

// ---------------------------------------------------------------------------
// 6. Journal failures dispatch nothing.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceJournalFailuresDispatchNothing(t *testing.T) {
	boom := errors.New("journal unavailable")
	wrongIdentity := func(identity domain.ProbeInstallIdentity) domain.ProbeInstallIdentity {
		identity.RequestedBy++
		return identity
	}
	for _, tc := range []struct {
		name  string
		setup func(env *opInstallEnv)
		check func(t *testing.T, env *opInstallEnv, err error)
	}{
		{
			name:  "create_error_passes_through",
			setup: func(env *opInstallEnv) { env.journal.createErr = boom },
			check: func(t *testing.T, env *opInstallEnv, err error) {
				if err != boom {
					t.Fatalf("error=%v, want exactly %v", err, boom)
				}
				if env.journal.rowCount() != 0 {
					t.Fatal("a failed insert left a row")
				}
			},
		},
		{
			name:  "ambiguous_commit_is_not_dispatched",
			setup: func(env *opInstallEnv) { env.journal.createErr, env.journal.createCommits = boom, true },
			check: func(t *testing.T, env *opInstallEnv, err error) {
				if err != boom {
					t.Fatalf("error=%v, want exactly %v", err, boom)
				}
				// The queued row honestly expires; it is never executed.
				if row := env.journal.row(opInstallID); row == nil || row.State != domain.ProbeInstallQueued {
					t.Fatalf("row %+v", row)
				}
			},
		},
		{
			name: "changed_identity_conflicts",
			setup: func(env *opInstallEnv) {
				changed := opInstallIdentityFor(opInstallSubmission(opInstallID, 8), opInstallClientFingerprint)
				env.journal.seed(changed, opInstallOwnerB, domain.ProbeInstallRunning)
			},
			check: func(t *testing.T, env *opInstallEnv, err error) {
				if err != ports.ErrConflict {
					t.Fatalf("error=%v, want exactly ports.ErrConflict", err)
				}
				if row := env.journal.row(opInstallID); row == nil || row.Identity.RequestedBy != 8 {
					t.Fatalf("the conflicting row was disturbed: %+v", row)
				}
			},
		},
		{
			name: "created_receipt_with_other_identity",
			setup: func(env *opInstallEnv) {
				env.journal.createOverride = func(identity domain.ProbeInstallIdentity, owner string) (*domain.ProbeInstallOperation, bool, error) {
					return opInstallNewRow(wrongIdentity(identity), owner), true, nil
				}
			},
			check: func(t *testing.T, _ *opInstallEnv, err error) {
				if !errors.Is(err, domain.ErrInternal) {
					t.Fatalf("error=%v, want ErrInternal", err)
				}
			},
		},
		{
			name: "retry_receipt_with_other_identity",
			setup: func(env *opInstallEnv) {
				env.journal.createOverride = func(identity domain.ProbeInstallIdentity, owner string) (*domain.ProbeInstallOperation, bool, error) {
					return opInstallNewRow(wrongIdentity(identity), owner), false, nil
				}
			},
			check: func(t *testing.T, _ *opInstallEnv, err error) {
				if !errors.Is(err, domain.ErrInternal) {
					t.Fatalf("error=%v, want ErrInternal", err)
				}
			},
		},
		{
			name: "nil_receipt_without_error",
			setup: func(env *opInstallEnv) {
				env.journal.createOverride = func(domain.ProbeInstallIdentity, string) (*domain.ProbeInstallOperation, bool, error) {
					return nil, true, nil
				}
			},
			check: func(t *testing.T, _ *opInstallEnv, err error) {
				if !errors.Is(err, domain.ErrInternal) {
					t.Fatalf("error=%v, want ErrInternal", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 1)
			tc.setup(env)
			operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
			if operation != nil || err == nil {
				t.Fatalf("operation=%+v error=%v, want nil + error", operation, err)
			}
			tc.check(t, env, err)
			env.assertSessionClosedOnce(t, 0)
			env.assertNothingDispatched(t)
			if env.active() != 0 {
				t.Fatalf("slot leaked: active=%d", env.active())
			}
			// The slot is free again: a clean, distinct request is admitted.
			env.journal.clearFaults()
			env.journal.createOverride = nil
			next, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7))
			if err != nil || next == nil {
				t.Fatalf("follow-up submit: operation=%+v error=%v", next, err)
			}
			opInstallAwait(t, env.runnerAt(t, 0).done, "the follow-up run to finish")
		})
	}
}

func TestProbeInstallOperationServiceRunnerConstructionFailureLeavesHonestQueuedRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		typedNil bool
	}{
		{"constructor_error", errors.New("runner unavailable"), false},
		{"typed_nil_runner", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 1)
			env.setNewRunnerFault(tc.err, tc.typedNil)
			operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
			if operation != nil || !errors.Is(err, domain.ErrInternal) {
				t.Fatalf("operation=%+v error=%v, want nil + ErrInternal", operation, err)
			}
			if tc.err != nil && errors.Is(err, tc.err) {
				t.Fatalf("runner construction error leaked: %v", err)
			}
			env.assertSessionClosedOnce(t, 0)
			if env.totalRuns() != 0 || env.events.count("run_start") != 0 {
				t.Fatal("a run was dispatched without a runner")
			}
			if env.active() != 0 {
				t.Fatalf("slot leaked: active=%d", env.active())
			}
			// The committed row is queued, never executing, and never terminal.
			row := env.journal.row(opInstallID)
			if row == nil || row.State != domain.ProbeInstallQueued {
				t.Fatalf("row %+v", row)
			}
			// An exact retry returns that stored receipt; it never redispatches.
			env.setNewRunnerFault(nil, false)
			retry, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
			if err != nil || retry == nil || retry.State != domain.ProbeInstallQueued {
				t.Fatalf("retry: operation=%+v error=%v", retry, err)
			}
			if env.totalRuns() != 0 {
				t.Fatal("retry redispatched a committed row")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 7. Saturation and capacity.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceCapacityBusyAndSlotRelease(t *testing.T) {
	for capacity := 1; capacity <= 4; capacity++ {
		t.Run(fmt.Sprintf("capacity_%d", capacity), func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), capacity)
			env.setRunners(func(n int) *opInstallRunner {
				runner := newOpInstallRunner(env.events)
				if n < capacity {
					runner.hold = make(chan struct{})
				}
				return runner
			})
			for i := 1; i <= capacity; i++ {
				operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallUUID(i), 7))
				if err != nil || operation == nil {
					t.Fatalf("submit %d within capacity: operation=%+v error=%v", i, operation, err)
				}
				opInstallAwait(t, env.runnerAt(t, i-1).started, fmt.Sprintf("run %d to start", i))
			}
			if env.active() != capacity {
				t.Fatalf("active=%d, want %d", env.active(), capacity)
			}

			busyID := opInstallUUID(capacity + 1)
			if operation, err := env.svc.Submit(t.Context(), opInstallSubmission(busyID, 7)); operation != nil || !errors.Is(err, domain.ErrProbeInstallBusy) {
				t.Fatalf("beyond capacity: operation=%+v error=%v, want ErrProbeInstallBusy", operation, err)
			}
			if got := len(env.journal.createCalls()); got != capacity {
				t.Fatalf("a Busy submit reached journal Create: creates=%d", got)
			}
			if row := env.journal.row(busyID); row != nil {
				t.Fatalf("a Busy submit committed a row: %+v", row)
			}
			if env.runnerCount() != capacity || env.totalRuns() != capacity {
				t.Fatalf("runners=%d runs=%d", env.runnerCount(), env.totalRuns())
			}
			// The Busy attempt's own session was disposed; the held ones were not.
			env.assertSessionClosedOnce(t, capacity)
			for i := 0; i < capacity; i++ {
				if closes := env.factory.sessionAt(t, i).closeCount(); closes != 0 {
					t.Fatalf("held session %d closed %d times while its run is live", i, closes)
				}
			}

			// Releasing one run frees exactly one slot.
			env.runnerAt(t, 0).release()
			opInstallAwait(t, env.factory.sessionAt(t, 0).closed, "the first session to close")
			var admitted *domain.ProbeInstallOperation
			deadline := time.Now().Add(opInstallWait)
			for {
				operation, err := env.svc.Submit(t.Context(), opInstallSubmission(busyID, 7))
				if err == nil {
					admitted = operation
					break
				}
				if !errors.Is(err, domain.ErrProbeInstallBusy) || time.Now().After(deadline) {
					t.Fatalf("after release: operation=%+v error=%v", operation, err)
				}
				time.Sleep(2 * time.Millisecond)
			}
			if admitted == nil || admitted.Identity.OperationID != busyID {
				t.Fatalf("admitted %+v", admitted)
			}
			opInstallAwait(t, env.runnerAt(t, capacity).done, "the late run to finish")
			if row := env.journal.row(busyID); row == nil {
				t.Fatal("the admitted request has no row")
			}
		})
	}
}

func TestProbeInstallOperationServiceSaturatedExactRetryAnswersWithoutDispatch(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	held := newOpInstallRunner(env.events)
	held.hold = make(chan struct{})
	env.setRunners(func(int) *opInstallRunner { return held })

	request := opInstallSubmission(opInstallID, 7)
	first, err := env.svc.Submit(t.Context(), request)
	if err != nil || first == nil {
		t.Fatalf("first submit: operation=%+v error=%v", first, err)
	}
	opInstallAwait(t, held.started, "the run to start")
	// The owning run advances the durable row; a retry must see that state.
	env.journal.mutate(opInstallID, func(o *domain.ProbeInstallOperation) { o.State = domain.ProbeInstallRunning })

	retry, err := env.svc.Submit(t.Context(), request)
	if err != nil || retry == nil {
		t.Fatalf("saturated exact retry: operation=%+v error=%v", retry, err)
	}
	if retry.State != domain.ProbeInstallRunning || retry.OwnerID != first.OwnerID || retry.Identity != first.Identity ||
		!retry.CreatedAt.Equal(first.CreatedAt) || !retry.DeadlineAt.Equal(first.DeadlineAt) {
		t.Fatalf("saturated retry must answer with the stored receipt: %+v vs %+v", retry, first)
	}
	// No second Create, no dispatch; the retry's own session was disposed.
	if creates := env.journal.createCalls(); len(creates) != 1 {
		t.Fatalf("journal creates=%d, want 1", len(creates))
	}
	if env.runnerCount() != 1 || env.totalRuns() != 1 || env.events.count("new_runner") != 1 {
		t.Fatalf("saturated retry dispatched: %v", env.events.snapshot())
	}
	env.assertSessionClosedOnce(t, 1)
	if closes := env.factory.sessionAt(t, 0).closeCount(); closes != 0 {
		t.Fatalf("the live run's session was disturbed: closes=%d", closes)
	}
	if env.active() != 1 {
		t.Fatalf("active=%d, want 1 (only the held run)", env.active())
	}

	// A brand-new identifier is Busy.
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7)); !errors.Is(err, domain.ErrProbeInstallBusy) {
		t.Fatalf("new id while saturated: %v, want ErrProbeInstallBusy", err)
	}
	env.assertSessionClosedOnce(t, 2)

	// A different identity under the same id is the conflict, never Busy.
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ProbeInstallSubmission)
		client string
	}{
		{"other_actor", func(r *domain.ProbeInstallSubmission) { r.RequestedBy = 8 }, ""},
		{"other_host", func(r *domain.ProbeInstallSubmission) { r.Host.Address = "other.example:22" }, ""},
		{"other_host_key", func(r *domain.ProbeInstallSubmission) { r.HostKeyFingerprint = opInstallFingerprint(0x02) }, ""},
		{"other_binary", func(r *domain.ProbeInstallSubmission) { r.Binary.Size++ }, ""},
		{"other_client_key", func(*domain.ProbeInstallSubmission) {}, opInstallFingerprint(0x6b)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionsBefore := env.factory.sessionCount()
			changed := opInstallSubmission(opInstallID, 7)
			tc.mutate(&changed)
			if tc.client != "" {
				env.factory.setFingerprint(tc.client)
				defer env.factory.setFingerprint(opInstallClientFingerprint)
			}
			operation, err := env.svc.Submit(t.Context(), changed)
			if operation != nil || err != ports.ErrConflict {
				t.Fatalf("operation=%+v error=%v, want exactly ports.ErrConflict", operation, err)
			}
			if got := len(env.journal.createCalls()); got != 1 {
				t.Fatalf("conflict reached journal Create: creates=%d", got)
			}
			env.assertSessionClosedOnce(t, sessionsBefore)
			if env.totalRuns() != 1 {
				t.Fatalf("runs=%d", env.totalRuns())
			}
		})
	}
	if row := env.journal.row(opInstallID); row == nil || row.Identity != opInstallIdentityFor(request, opInstallClientFingerprint) {
		t.Fatalf("the committed row was disturbed: %+v", row)
	}
}

func TestProbeInstallOperationServiceSaturatedIdentityCheckErrorsPassThrough(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	held := newOpInstallRunner(env.events)
	held.hold = make(chan struct{})
	env.setRunners(func(int) *opInstallRunner { return held })
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, held.started, "the run to start")

	storage := errors.New("storage unavailable")
	env.journal.observeErr = storage
	if operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7)); operation != nil || err != storage {
		t.Fatalf("operation=%+v error=%v, want the journal error untouched", operation, err)
	}
	env.assertSessionClosedOnce(t, 1)
	if env.active() != 1 || env.totalRuns() != 1 {
		t.Fatalf("active=%d runs=%d", env.active(), env.totalRuns())
	}
}

func TestProbeInstallOperationServiceSessionCloseFailureStillReleasesTheSlot(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	env.factory.closeErr = errors.New("snapshot disposal failed")
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	session := env.factory.sessionAt(t, 0)
	opInstallAwait(t, session.closed, "the session close attempt")
	opInstallPoll(t, "the slot to be released after a failing session close", func() bool { return env.active() == 0 })
	if closes := session.closeCount(); closes != 1 {
		t.Fatalf("close attempted %d times, want 1 (failures are not retried)", closes)
	}
	// Admission still works and Close does not hang on the failed disposal.
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7)); err != nil {
		t.Fatalf("follow-up submit: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), opInstallWait)
	defer cancel()
	if err := env.svc.Close(closeCtx); err != nil {
		t.Fatalf("close: %v", err)
	}
	env.assertSessionClosedOnce(t, 1)
}

// ---------------------------------------------------------------------------
// 8. Close and root-context lifecycle.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceCloseStopsAdmissionAndJoinsRuns(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 2)
	runner := newOpInstallRunner(env.events)
	runner.hold = make(chan struct{})
	runner.afterCancel = make(chan struct{})
	env.setRunners(func(int) *opInstallRunner { return runner })

	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, runner.started, "the run to start")
	session := env.factory.sessionAt(t, 0)

	closeDone := make(chan error, 1)
	go func() { closeDone <- env.svc.Close(context.Background()) }()

	// Close cancels the root context the held Run is using.
	opInstallAwait(t, runner.canceled, "the run context to be canceled")
	if err := runner.runCalls()[0].ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("run context error=%v, want context.Canceled", err)
	}
	// ... but does not return while Run has not returned.
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned (%v) before the run joined", err)
	case <-time.After(opInstallQuiet):
	}
	if closes := session.closeCount(); closes != 0 {
		t.Fatalf("session closed %d times before its run returned", closes)
	}
	// Admission is already refused while Close is pending, before any I/O.
	factoryCalls := env.factory.callCount()
	if operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7)); operation != nil || !errors.Is(err, domain.ErrProbeInstallClosed) {
		t.Fatalf("submit during Close: operation=%+v error=%v, want ErrProbeInstallClosed", operation, err)
	}
	if env.factory.callCount() != factoryCalls {
		t.Fatal("a refused submit still reached the session factory")
	}

	// The run records its honest outcome and returns; only then does Close.
	runner.finish()
	if err := opInstallAwaitErr(t, closeDone, "Close to return"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if closes := session.closeCount(); closes != 1 {
		t.Fatalf("session closed %d times by the time Close returned, want 1", closes)
	}
	if env.active() != 0 {
		t.Fatalf("active=%d after Close", env.active())
	}
	runReturn, sessionClose := env.events.indexOf("run_return"), env.events.indexOf("session_close:1")
	if runReturn < 0 || sessionClose < 0 || runReturn > sessionClose {
		t.Fatalf("session must close after Run returns: %v", env.events.snapshot())
	}
	// The session is disposed on a fresh bounded context even though the root
	// context is already canceled.
	if remaining := session.closeDeadline.Sub(session.closeCalledAt); session.closeCtxErr != nil || !session.closeHasDeadline ||
		remaining <= 0 || remaining > probeInstallCloseTTL {
		t.Fatalf("session closed on a stale or unbounded context: err=%v remaining=%v", session.closeCtxErr, remaining)
	}

	// Reads survive shutdown; admission stays closed; Close is idempotent.
	if _, err := env.svc.Observe(t.Context(), opInstallID); err != nil {
		t.Fatalf("observe after close: %v", err)
	}
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDThree, 7)); !errors.Is(err, domain.ErrProbeInstallClosed) {
		t.Fatalf("submit after close: %v", err)
	}
	if err := env.svc.Close(t.Context()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestProbeInstallOperationServiceCloseHonorsItsOwnDeadline(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	runner := newOpInstallRunner(env.events)
	runner.hold = make(chan struct{})
	runner.ignoreCancel = true
	env.setRunners(func(int) *opInstallRunner { return runner })

	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, runner.started, "the run to start")
	session := env.factory.sessionAt(t, 0)

	closeCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := env.svc.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close with a stuck run: %v, want context.DeadlineExceeded", err)
	}
	// The run was told to stop but still has not returned: nothing is closed
	// early and the slot is still accounted for.
	if err := runner.runCalls()[0].ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("run context error=%v, want context.Canceled", err)
	}
	if !opInstallStaysOpen(runner.done) {
		t.Fatal("the run returned although it was never released")
	}
	if closes := session.closeCount(); closes != 0 {
		t.Fatalf("session closed %d times while its run is still live", closes)
	}
	if env.active() != 1 {
		t.Fatalf("active=%d, want 1", env.active())
	}
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7)); !errors.Is(err, domain.ErrProbeInstallClosed) {
		t.Fatalf("submit after a timed-out Close: %v", err)
	}

	// Release the run so nothing leaks, then Close joins normally.
	runner.release()
	joinCtx, joinCancel := context.WithTimeout(context.Background(), opInstallWait)
	defer joinCancel()
	if err := env.svc.Close(joinCtx); err != nil {
		t.Fatalf("close after release: %v", err)
	}
	if closes := session.closeCount(); closes != 1 || env.active() != 0 {
		t.Fatalf("closes=%d active=%d after the run joined", closes, env.active())
	}
}

func TestProbeInstallOperationServiceCloseWithNilContextWaitsForRuns(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	runner := newOpInstallRunner(env.events)
	runner.hold = make(chan struct{})
	runner.ignoreCancel = true
	env.setRunners(func(int) *opInstallRunner { return runner })

	// With nothing running a nil context returns immediately.
	idle := opInstallEnvFor(t, t.Context(), 1)
	var nilCtx context.Context
	if err := idle.svc.Close(nilCtx); err != nil {
		t.Fatalf("idle close with nil context: %v", err)
	}

	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, runner.started, "the run to start")
	closeDone := make(chan error, 1)
	go func() { closeDone <- env.svc.Close(nilCtx) }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close(nil) returned (%v) while a run was live", err)
	case <-time.After(opInstallQuiet):
	}
	runner.release()
	if err := opInstallAwaitErr(t, closeDone, "Close(nil) to return"); err != nil {
		t.Fatalf("close: %v", err)
	}
	env.assertSessionClosedOnce(t, 0)
}

func TestProbeInstallOperationServiceCloseDuringSessionCreationReportsClosed(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	entered := make(chan struct{})
	var once sync.Once
	env.factory.gate = func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	submitDone := make(chan error, 1)
	go func() {
		operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
		if operation != nil {
			err = fmt.Errorf("unexpected operation %+v (error %v)", operation, err)
		}
		submitDone <- err
	}()
	opInstallAwait(t, entered, "session creation to begin")

	closeDone := make(chan error, 1)
	go func() { closeDone <- env.svc.Close(context.Background()) }()

	err := opInstallAwaitErr(t, submitDone, "Submit to return")
	if !errors.Is(err, domain.ErrProbeInstallClosed) || errors.Is(err, context.Canceled) {
		t.Fatalf("submit interrupted by Close: %v, want ErrProbeInstallClosed", err)
	}
	if err := opInstallAwaitErr(t, closeDone, "Close to return"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if env.journal.rowCount() != 0 || len(env.journal.createCalls()) != 0 {
		t.Fatal("an interrupted session creation reached the journal")
	}
	env.assertNothingDispatched(t)
	if env.active() != 0 {
		t.Fatalf("active=%d", env.active())
	}
}

func TestProbeInstallOperationServiceCloseDuringJournalCreateReportsClosed(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	entered := make(chan struct{})
	var once sync.Once
	env.journal.createGate = func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	submitDone := make(chan error, 1)
	go func() {
		operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
		if operation != nil {
			err = fmt.Errorf("unexpected operation %+v (error %v)", operation, err)
		}
		submitDone <- err
	}()
	opInstallAwait(t, entered, "journal Create to begin")

	closeDone := make(chan error, 1)
	go func() { closeDone <- env.svc.Close(context.Background()) }()

	err := opInstallAwaitErr(t, submitDone, "Submit to return")
	if !errors.Is(err, domain.ErrProbeInstallClosed) || errors.Is(err, context.Canceled) {
		t.Fatalf("submit interrupted by Close: %v, want ErrProbeInstallClosed", err)
	}
	if err := opInstallAwaitErr(t, closeDone, "Close to return"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if env.journal.rowCount() != 0 {
		t.Fatal("an interrupted Create committed a row")
	}
	env.assertSessionClosedOnce(t, 0)
	env.assertNothingDispatched(t)
	if env.active() != 0 {
		t.Fatalf("active=%d", env.active())
	}
}

func TestProbeInstallOperationServiceCallerCancelDuringSessionCreationIsNotClosed(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	entered := make(chan struct{})
	var once sync.Once
	env.factory.gate = func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	requestCtx, cancel := context.WithCancel(t.Context())
	submitDone := make(chan error, 1)
	go func() {
		_, err := env.svc.Submit(requestCtx, opInstallSubmission(opInstallID, 7))
		submitDone <- err
	}()
	opInstallAwait(t, entered, "session creation to begin")
	cancel()
	err := opInstallAwaitErr(t, submitDone, "Submit to return")
	if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrProbeInstallClosed) {
		t.Fatalf("caller cancellation: %v, want context.Canceled and not ErrProbeInstallClosed", err)
	}
	if env.journal.rowCount() != 0 || env.active() != 0 {
		t.Fatalf("rows=%d active=%d", env.journal.rowCount(), env.active())
	}
	// The service itself is still admitting.
	env.factory.mu.Lock()
	env.factory.gate = nil
	env.factory.mu.Unlock()
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7)); err != nil {
		t.Fatalf("service must keep admitting after a caller cancellation: %v", err)
	}
}

func TestProbeInstallOperationServiceRootContextCancellationStopsAdmissionAndRuns(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	env := opInstallEnvFor(t, parent, 2)

	runner := newOpInstallRunner(env.events)
	runner.hold = make(chan struct{})
	env.setRunners(func(int) *opInstallRunner { return runner })

	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, runner.started, "the run to start")
	cancelParent()
	opInstallAwait(t, runner.canceled, "the run to observe the parent cancellation")
	opInstallAwait(t, runner.done, "the run to return")
	opInstallAwait(t, env.factory.sessionAt(t, 0).closed, "the session to close")
	opInstallPoll(t, "the slot to be released", func() bool { return env.active() == 0 })

	factoryCalls := env.factory.callCount()
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDTwo, 7)); !errors.Is(err, domain.ErrProbeInstallClosed) {
		t.Fatalf("submit after the root context ended: %v", err)
	}
	if env.factory.callCount() != factoryCalls {
		t.Fatal("a refused submit reached the session factory")
	}
	if _, err := env.svc.Observe(t.Context(), opInstallID); err != nil {
		t.Fatalf("reads must survive shutdown: %v", err)
	}
	if err := env.svc.Close(t.Context()); err != nil {
		t.Fatalf("close after the root context ended: %v", err)
	}
}

func TestProbeInstallOperationServiceCloseAdmissionRace(t *testing.T) {
	env := opInstallEnvFor(t, context.Background(), 4)
	// Every run lives until Close cancels the root context.
	env.setRunners(func(int) *opInstallRunner {
		runner := newOpInstallRunner(env.events)
		runner.hold = make(chan struct{})
		return runner
	})

	const submitters = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := map[string]bool{}
	for i := 1; i <= submitters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := opInstallUUID(i)
			operation, err := env.svc.Submit(t.Context(), opInstallSubmission(id, 7))
			if err != nil {
				if !errors.Is(err, domain.ErrProbeInstallClosed) && !errors.Is(err, domain.ErrProbeInstallBusy) {
					t.Errorf("unexpected submit error for %s: %v", id, err)
				}
				return
			}
			mu.Lock()
			admitted[operation.Identity.OperationID] = true
			mu.Unlock()
		}(i)
	}
	closeDone := make(chan error, 1)
	go func() {
		<-start
		closeDone <- env.svc.Close(context.Background())
	}()
	close(start)
	wg.Wait()
	if err := opInstallAwaitErr(t, closeDone, "Close to return"); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Every committed row got exactly one joined run, and every session is gone.
	mu.Lock()
	defer mu.Unlock()
	if rows := env.journal.rowCount(); rows != len(admitted) {
		t.Fatalf("rows=%d admitted=%d", rows, len(admitted))
	}
	if runs := env.totalRuns(); runs != len(admitted) || env.runnerCount() != len(admitted) {
		t.Fatalf("runs=%d runners=%d admitted=%d", runs, env.runnerCount(), len(admitted))
	}
	for i := 0; i < env.factory.sessionCount(); i++ {
		env.assertSessionClosedOnce(t, i)
	}
	if env.active() != 0 {
		t.Fatalf("active=%d after Close", env.active())
	}
	for i := 0; i < env.runnerCount(); i++ {
		select {
		case <-env.runnerAt(t, i).done:
		default:
			t.Fatalf("run %d had not returned by the time Close returned", i)
		}
	}
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallIDThree, 7)); !errors.Is(err, domain.ErrProbeInstallClosed) {
		t.Fatalf("submit after close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 9. Observe and Cancel.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServiceObserveAndCancelValidateTheID(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 2)
	var nilCtx context.Context
	for _, id := range []string{
		"", "not-a-uuid", strings.ToUpper(opInstallHexID), domain.LocalProbeID,
		"00000000-0000-0000-0000-000000000000", opInstallID + "0", " " + opInstallID,
	} {
		t.Run("id_"+id, func(t *testing.T) {
			if operation, err := env.svc.Observe(t.Context(), id); operation != nil || !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Observe: operation=%+v error=%v", operation, err)
			}
			if operation, err := env.svc.Cancel(t.Context(), id); operation != nil || !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Cancel: operation=%+v error=%v", operation, err)
			}
		})
	}
	if _, err := env.svc.Observe(nilCtx, opInstallID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Observe(nil ctx): %v", err)
	}
	if _, err := env.svc.Cancel(nilCtx, opInstallID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Cancel(nil ctx): %v", err)
	}
	env.assertNoIO(t)
}

func TestProbeInstallOperationServiceObserveAndCancelDelegateUntouched(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 2)
	ctx := context.WithValue(t.Context(), opInstallCtxKey{}, "marker")

	// Not found is the journal's answer, surfaced as is; Cancel reads first
	// and never asks the journal to cancel an operation it cannot see.
	if _, err := env.svc.Observe(ctx, opInstallIDThree); err != ports.ErrNotFound {
		t.Fatalf("Observe missing: %v", err)
	}
	if _, err := env.svc.Cancel(ctx, opInstallIDThree); err != ports.ErrNotFound {
		t.Fatalf("Cancel missing: %v", err)
	}

	identity := opInstallIdentityFor(opInstallSubmission(opInstallID, 7), opInstallClientFingerprint)
	ready := &domain.ProbeInstallOperation{Identity: identity, OwnerID: opInstallOwnerB, State: domain.ProbeInstallResourcesReady}
	requested := &domain.ProbeInstallOperation{Identity: identity, OwnerID: opInstallOwnerB, State: domain.ProbeInstallCancelRequested}

	// The journal's receipt is returned as the same object, never rewritten.
	env.journal.observeResult = ready
	if got, err := env.svc.Observe(ctx, opInstallID); err != nil || got != ready {
		t.Fatalf("Observe: %p vs %p error=%v", got, ready, err)
	}
	// resources_ready has no remaining installer work: returned unchanged,
	// never demoted to a cancel_requested that nothing would finish.
	env.journal.cancelResult = requested
	if got, err := env.svc.Cancel(ctx, opInstallID); err != nil || got != ready {
		t.Fatalf("Cancel resources_ready: %p vs %p error=%v", got, ready, err)
	}
	if n := len(env.journal.cancelCalls()); n != 0 {
		t.Fatalf("resources_ready reached journal Cancel %d times", n)
	}

	// Active work is canceled through the journal, whose receipt is returned.
	running := &domain.ProbeInstallOperation{Identity: identity, OwnerID: opInstallOwnerB, State: domain.ProbeInstallRunning}
	env.journal.observeResult = running
	if got, err := env.svc.Cancel(ctx, opInstallID); err != nil || got != requested {
		t.Fatalf("Cancel running: %p vs %p error=%v", got, requested, err)
	}

	// Errors are not translated; a failed read never attempts cancellation.
	storage := errors.New("storage unavailable")
	env.journal.observeErr = storage
	if got, err := env.svc.Observe(ctx, opInstallID); got != nil || err != storage {
		t.Fatalf("Observe error: %+v %v", got, err)
	}
	if got, err := env.svc.Cancel(ctx, opInstallID); got != nil || err != storage {
		t.Fatalf("Cancel read error: %+v %v", got, err)
	}
	env.journal.observeErr, env.journal.cancelErr = nil, storage
	if got, err := env.svc.Cancel(ctx, opInstallID); got != nil || err != storage {
		t.Fatalf("Cancel journal error: %+v %v", got, err)
	}

	cancels := env.journal.cancelCalls()
	if len(cancels) != 2 {
		t.Fatalf("journal Cancel calls=%d, want 2 (running, journal error)", len(cancels))
	}
	for _, call := range append(env.journal.observeCalls(), cancels...) {
		if call.ctx != ctx {
			t.Fatal("caller context not forwarded")
		}
	}
	if env.executorCalls() != 0 || env.factory.callCount() != 0 {
		t.Fatal("reads and cancellations must not build sessions or runners")
	}
}

func TestProbeInstallOperationServiceCancelRecordsIntentButNeverStopsLocalWork(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	held := newOpInstallRunner(env.events)
	held.hold = make(chan struct{})
	env.setRunners(func(int) *opInstallRunner { return held })
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, held.started, "the run to start")

	requested, err := env.svc.Cancel(t.Context(), opInstallID)
	if err != nil || requested == nil || requested.State != domain.ProbeInstallCancelRequested {
		t.Fatalf("cancel: operation=%+v error=%v", requested, err)
	}
	if row := env.journal.row(opInstallID); row == nil || row.State != domain.ProbeInstallCancelRequested {
		t.Fatalf("cancel intent was not persisted: %+v", row)
	}
	// Cancel is durable intent only: the runner's watcher owns stopping work.
	if !opInstallStaysOpen(held.canceled) {
		t.Fatal("Cancel canceled the run context directly")
	}
	if closes := env.factory.sessionAt(t, 0).closeCount(); closes != 0 {
		t.Fatalf("Cancel closed the live session (closes=%d)", closes)
	}
	if env.active() != 1 {
		t.Fatalf("active=%d, want 1: Cancel must not release the slot", env.active())
	}
	held.release()
	opInstallAwait(t, env.factory.sessionAt(t, 0).closed, "the session to close")
}

// ---------------------------------------------------------------------------
// 10. Helpers under test.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationSessionError(t *testing.T) {
	canary := errors.New("canary /home/ops/.ssh/id_ed25519 passphrase=hunter2")
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"nil_is_internal", nil, domain.ErrInternal},
		{"canceled", context.Canceled, context.Canceled},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded},
		{"artifact", domain.ErrProbeInstallArtifact, domain.ErrProbeInstallArtifact},
		{"ssh_input", domain.ErrProbeSSHInput, domain.ErrProbeSSHInput},
		{"validation", domain.ErrValidation, domain.ErrValidation},
		{"wrapped_artifact", fmt.Errorf("open %s: %w", canary, domain.ErrProbeInstallArtifact), domain.ErrProbeInstallArtifact},
		{"joined_ssh_input", errors.Join(canary, domain.ErrProbeSSHInput), domain.ErrProbeSSHInput},
		{"wrapped_deadline", fmt.Errorf("prepare: %w", context.DeadlineExceeded), context.DeadlineExceeded},
		{"arbitrary", canary, domain.ErrInternal},
		{"internal", domain.ErrInternal, domain.ErrInternal},
		{"not_found", ports.ErrNotFound, domain.ErrInternal},
		{"conflict", ports.ErrConflict, domain.ErrInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeInstallSessionError(tc.err)
			if got != tc.want {
				t.Fatalf("got %v, want exactly %v", got, tc.want)
			}
			if strings.Contains(got.Error(), "canary") || strings.Contains(got.Error(), "hunter2") {
				t.Fatalf("message leaked: %q", got)
			}
		})
	}
}

func TestProbeInstallOperationAttempts(t *testing.T) {
	for _, n := range []int{0, 1, 4, 64} {
		t.Run(fmt.Sprintf("n_%d", n), func(t *testing.T) {
			attempts, err := probeInstallAttempts(n)
			if err != nil || len(attempts) != n {
				t.Fatalf("attempts=%v error=%v", attempts, err)
			}
			seen := map[string]bool{}
			for _, attempt := range attempts {
				if len(attempt) != 36 || !opInstallV4.MatchString(attempt) || !domain.ValidProbeOperationID(attempt) {
					t.Fatalf("%q is not a canonical lowercase v4 UUID", attempt)
				}
				if seen[attempt] {
					t.Fatalf("duplicate attempt %q in %v", attempt, attempts)
				}
				seen[attempt] = true
			}
		})
	}
	t.Run("distinct_across_calls", func(t *testing.T) {
		seen := map[string]bool{}
		for call := 0; call < 50; call++ {
			attempts, err := probeInstallAttempts(4)
			if err != nil {
				t.Fatal(err)
			}
			for _, attempt := range attempts {
				if seen[attempt] {
					t.Fatalf("attempt %q repeated across calls", attempt)
				}
				seen[attempt] = true
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Production lifecycle: the real journal facade and the real runner behind the
// real service (rule 11). Only the durable store and the SSH session are fakes.
// ---------------------------------------------------------------------------

type opInstallRealEnv struct {
	svc     *ProbeInstallOperationService
	fake    *runnerFakeJournal
	journal *ProbeInstallJournalService
	factory *opInstallFactory
	events  *opInstallEvents
}

func opInstallRealEnvFor(t *testing.T, ctx context.Context) *opInstallRealEnv {
	t.Helper()
	events := &opInstallEvents{}
	fake := newRunnerFakeJournal()
	journal, err := NewProbeInstallJournalService(fake, fake)
	if err != nil {
		t.Fatalf("journal facade: %v", err)
	}
	factory := &opInstallFactory{events: events, fingerprint: opInstallClientFingerprint}
	svc, err := NewProbeInstallOperationService(ctx, journal, factory, opInstallOwnerA, 2)
	if err != nil || svc == nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), opInstallWait)
		defer cancel()
		if err := svc.Close(closeCtx); err != nil {
			t.Errorf("closing service: %v", err)
		}
	})
	return &opInstallRealEnv{svc: svc, fake: fake, journal: journal, factory: factory, events: events}
}

func (e *opInstallRealEnv) state() string {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	if e.fake.op == nil {
		return ""
	}
	return e.fake.op.state
}

func TestProbeInstallOperationServiceLifecycleRunsEveryResourceThroughTheRealRunner(t *testing.T) {
	env := opInstallRealEnvFor(t, t.Context())
	request := opInstallSubmission(opInstallID, 7)
	receipt, err := env.svc.Submit(t.Context(), request)
	if err != nil || receipt == nil || receipt.State != domain.ProbeInstallQueued || receipt.OwnerID != opInstallOwnerA {
		t.Fatalf("receipt=%+v error=%v", receipt, err)
	}
	opInstallPoll(t, "resources_ready", func() bool { return env.state() == domain.ProbeInstallResourcesReady })
	session := env.factory.sessionAt(t, 0)
	opInstallAwait(t, session.closed, "the session to close")
	opInstallPoll(t, "the slot to be released", func() bool {
		env.svc.mu.Lock()
		defer env.svc.mu.Unlock()
		return env.svc.active == 0
	})

	wantKinds := []string{"data_directory", "artifact_directory", "unit_file", "service"}
	if got := session.appliedKinds(); strings.Join(got, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("applied %v, want %v in layout order", got, wantKinds)
	}
	observed, err := env.svc.Observe(t.Context(), opInstallID)
	if err != nil || observed == nil || observed.State != domain.ProbeInstallResourcesReady {
		t.Fatalf("observed=%+v error=%v", observed, err)
	}
	attempts := map[string]bool{}
	for _, resource := range observed.Resources {
		if resource.State != domain.ProbeInstallResourceApplied || !opInstallV4.MatchString(resource.AttemptID) || attempts[resource.AttemptID] {
			t.Fatalf("resource %+v", resource)
		}
		attempts[resource.AttemptID] = true
	}
	if observed.Identity != opInstallIdentityFor(request, opInstallClientFingerprint) {
		t.Fatalf("stored identity %+v", observed.Identity)
	}

	// An exact retry of the completed work answers from storage and applies nothing.
	retry, err := env.svc.Submit(t.Context(), request)
	if err != nil || retry == nil || retry.State != domain.ProbeInstallResourcesReady {
		t.Fatalf("retry=%+v error=%v", retry, err)
	}
	if got := len(session.appliedKinds()); got != 4 {
		t.Fatalf("retry re-applied resources: %d applications", got)
	}
	if env.factory.sessionCount() != 2 || env.factory.sessionAt(t, 1).closeCount() != 1 || len(env.factory.sessionAt(t, 1).appliedKinds()) != 0 {
		t.Fatalf("retry session handling: sessions=%d", env.factory.sessionCount())
	}
	if session.closeCount() != 1 {
		t.Fatalf("session closed %d times", session.closeCount())
	}
}

func TestProbeInstallOperationServiceLifecycleCancelFencesDependentWork(t *testing.T) {
	env := opInstallRealEnvFor(t, t.Context())
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	env.factory.applyHook = func(_ context.Context, kind string) (domain.ProbeInstallResourceOutcome, error) {
		if kind == "artifact_directory" {
			enteredOnce.Do(func() { close(entered) })
			// Models a remote helper that always finishes its in-flight action.
			<-release
		}
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	}
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, entered, "the second resource to be in flight")

	requested, err := env.svc.Cancel(t.Context(), opInstallID)
	if err != nil || requested == nil || requested.State != domain.ProbeInstallCancelRequested {
		t.Fatalf("cancel: operation=%+v error=%v", requested, err)
	}
	close(release)
	opInstallPoll(t, "the canceled terminal state", func() bool { return env.state() == domain.ProbeInstallCanceled })
	session := env.factory.sessionAt(t, 0)
	opInstallAwait(t, session.closed, "the session to close")

	// Cancellation fenced the dependents: only the already in-flight work ran.
	if got := session.appliedKinds(); strings.Join(got, ",") != "data_directory,artifact_directory" {
		t.Fatalf("applied %v: cancellation must stop dependent resources", got)
	}
	observed, err := env.svc.Observe(t.Context(), opInstallID)
	if err != nil || observed == nil || observed.State != domain.ProbeInstallCanceled {
		t.Fatalf("observed=%+v error=%v", observed, err)
	}
}

func TestProbeInstallOperationServiceLifecycleCloseRecordsHonestInterruptionBeforeReturning(t *testing.T) {
	env := opInstallRealEnvFor(t, t.Context())
	entered := make(chan struct{})
	var enteredOnce sync.Once
	env.factory.applyHook = func(ctx context.Context, _ string) (domain.ProbeInstallResourceOutcome, error) {
		enteredOnce.Do(func() { close(entered) })
		<-ctx.Done()
		return domain.ProbeInstallResourceOutcome{}, ctx.Err()
	}
	if _, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opInstallAwait(t, entered, "the first resource to be in flight")

	closeCtx, cancel := context.WithTimeout(context.Background(), opInstallWait)
	defer cancel()
	if err := env.svc.Close(closeCtx); err != nil {
		t.Fatalf("close: %v", err)
	}
	// No polling: Close joined the run, so the outcome is already persisted and
	// the session is already disposed.
	if state := env.state(); state != domain.ProbeInstallInterrupted {
		t.Fatalf("state after Close=%q, want %q", state, domain.ProbeInstallInterrupted)
	}
	if closes := env.factory.sessionAt(t, 0).closeCount(); closes != 1 {
		t.Fatalf("session closed %d times by the time Close returned", closes)
	}
	if got := env.factory.sessionAt(t, 0).appliedKinds(); len(got) != 1 {
		t.Fatalf("applied %v: shutdown must not let dependents start", got)
	}
}

// ---------------------------------------------------------------------------
// Host preflight gate: every refusal is definite and creates nothing.
// ---------------------------------------------------------------------------

func TestProbeInstallOperationServicePreflightRefusalsCreateNothing(t *testing.T) {
	canary := errors.New("canary uname=Linux key=/root/.ssh/id_ed25519")
	report := func(edit func(*domain.ProbeInstallHostReport)) func(context.Context, domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error) {
		return func(context.Context, domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error) {
			r := opInstallQualifyingReport()
			edit(&r)
			return r, nil
		}
	}
	fail := func(err error) func(context.Context, domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error) {
		return func(context.Context, domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error) {
			return domain.ProbeInstallHostReport{}, err
		}
	}
	for _, tc := range []struct {
		name string
		hook func(context.Context, domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error)
		code string // "" means a bare sentinel is wanted instead
		want error
	}{
		{"unsupported_os", report(func(r *domain.ProbeInstallHostReport) { r.Kernel = "FreeBSD" }), domain.ProbeInstallPreflightOS, nil},
		{"wrong_architecture", report(func(r *domain.ProbeInstallHostReport) { r.Machine = "aarch64" }), domain.ProbeInstallPreflightArchitecture, nil},
		{"unknown_architecture", report(func(r *domain.ProbeInstallHostReport) { r.Machine = "riscv64" }), domain.ProbeInstallPreflightArchitecture, nil},
		{"not_root", report(func(r *domain.ProbeInstallHostReport) { r.Root, r.Authority = false, false }), domain.ProbeInstallPreflightRoot, nil},
		{"no_systemd", report(func(r *domain.ProbeInstallHostReport) { r.Systemd = false }), domain.ProbeInstallPreflightSystemd, nil},
		{"no_authority", report(func(r *domain.ProbeInstallHostReport) { r.Authority = false }), domain.ProbeInstallPreflightAuthority, nil},
		{"existing_install", report(func(r *domain.ProbeInstallHostReport) { r.ExistingInstalls = 3 }), domain.ProbeInstallPreflightExisting, nil},
		{"invalid_report", report(func(r *domain.ProbeInstallHostReport) { r.Kernel = "" }), domain.ProbeInstallPreflightHelper, nil},
		{"bad_host_key", fail(&domain.ProbeInstallPreflightError{Code: domain.ProbeInstallPreflightTrust}), domain.ProbeInstallPreflightTrust, nil},
		{"auth_refused", fail(fmt.Errorf("wrap: %w", &domain.ProbeInstallPreflightError{Code: domain.ProbeInstallPreflightAuth})), domain.ProbeInstallPreflightAuth, nil},
		{"unknown_code_is_internal", fail(&domain.ProbeInstallPreflightError{Code: "host_" + canary.Error()}), "", domain.ErrInternal},
		{"arbitrary_error_is_internal", fail(canary), "", domain.ErrInternal},
		{"deadline_passes_through", fail(fmt.Errorf("%w: %w", canary, context.DeadlineExceeded)), "", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := opInstallEnvFor(t, t.Context(), 1)
			env.factory.mu.Lock()
			env.factory.preflightHook = tc.hook
			env.factory.mu.Unlock()
			operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
			if operation != nil || err == nil {
				t.Fatalf("operation=%+v error=%v, want a refusal", operation, err)
			}
			if tc.code != "" {
				var refusal *domain.ProbeInstallPreflightError
				if !errors.As(err, &refusal) || refusal.Code != tc.code || !errors.Is(err, domain.ErrProbeInstallPreflight) {
					t.Fatalf("error %v, want preflight refusal %q", err, tc.code)
				}
			} else if err != tc.want {
				t.Fatalf("error %v, want the bare %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatalf("refusal leaked host detail: %q", err)
			}
			if got := env.journal.createCalls(); len(got) != 0 {
				t.Fatalf("journal Create called after a refused preflight: %+v", got)
			}
			if env.journal.rowCount() != 0 {
				t.Fatal("a row exists after a refused preflight")
			}
			env.assertNothingDispatched(t)
			env.assertSessionClosedOnce(t, 0)
			if applied := env.factory.sessionAt(t, 0).appliedKinds(); len(applied) != 0 {
				t.Fatalf("host resources applied after a refusal: %v", applied)
			}
			if env.active() != 0 {
				t.Fatalf("slot leaked: active=%d", env.active())
			}
		})
	}
}

func TestProbeInstallOperationServicePreflightRunsAfterLookupAndBeforeCreate(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	request := opInstallSubmission(opInstallID, 7)
	operation, err := env.svc.Submit(t.Context(), request)
	if err != nil || operation == nil {
		t.Fatalf("submit: operation=%+v error=%v", operation, err)
	}
	observe, preflight, create := env.events.indexOf("observe"), env.events.indexOf("preflight"), env.events.indexOf("create")
	if observe < 0 || preflight < 0 || create < 0 || !(env.events.indexOf("factory") < observe && observe < preflight && preflight < create) {
		t.Fatalf("want factory < observe < preflight < create, got %v", env.events.snapshot())
	}
	session := env.factory.sessionAt(t, 0)
	session.mu.Lock()
	hosts := append([]domain.ProbeSSHHost(nil), session.preflights...)
	session.mu.Unlock()
	if len(hosts) != 1 || hosts[0] != request.Host {
		t.Fatalf("preflight hosts %+v, want exactly the request's host", hosts)
	}
	opInstallAwait(t, env.runnerAt(t, 0).done, "the run to finish")
}

func TestProbeInstallOperationServiceLookupFailureSkipsPreflight(t *testing.T) {
	env := opInstallEnvFor(t, t.Context(), 1)
	boom := errors.New("journal unavailable")
	env.journal.mu.Lock()
	env.journal.observeErr = boom
	env.journal.mu.Unlock()
	operation, err := env.svc.Submit(t.Context(), opInstallSubmission(opInstallID, 7))
	if operation != nil || !errors.Is(err, boom) {
		t.Fatalf("operation=%+v error=%v, want the lookup failure", operation, err)
	}
	if env.events.count("preflight") != 0 || len(env.journal.createCalls()) != 0 {
		t.Fatalf("lookup failure still preflighted or created: %v", env.events.snapshot())
	}
	env.assertNothingDispatched(t)
	env.assertSessionClosedOnce(t, 0)
}
