package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ---------------------------------------------------------------------------
// Fixtures. Everything here is self-contained and runner-prefixed so this file
// owns no shared helper and edits nothing outside it.
// ---------------------------------------------------------------------------

const (
	runnerOpID       = "11111111-1111-4111-8111-111111111111"
	runnerOwner      = "33333333-3333-4333-8333-333333333333"
	runnerOwnerTwo   = "44444444-4444-4444-8444-444444444444"
	runnerAttemptTwo = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// runnerLeakMarker stands in for private executor/remote diagnostics a raw
// error could carry. It is composed so no fixture resembles a credential.
var runnerLeakMarker = strings.Join([]string{"raw", "remote", "output", "hunter2", "marker"}, "-")

// runnerBaseTime is microsecond-precision UTC like the stored stamps.
func runnerBaseTime() time.Time {
	return time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
}

func runnerFingerprint(fill byte) string {
	value := make([]byte, 32)
	for i := range value {
		value[i] = fill
	}
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(value)
}

func runnerIdentity(mode, operationID string) domain.ProbeInstallIdentity {
	identity := domain.ProbeInstallIdentity{
		OperationID:          operationID,
		Host:                 domain.ProbeSSHHost{Address: "edge.example:22", User: "root"},
		HostKeyFingerprint:   runnerFingerprint(0x01),
		ClientKeyFingerprint: runnerFingerprint(0x02),
		RequestedBy:          7,
		LayoutVersion:        domain.ProbeInstallLayoutVersion,
		Mode:                 mode,
	}
	if mode == domain.ProbeInstallSystemd {
		identity.Binary = domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 100}
	} else {
		identity.Image = domain.ProbeImageSelection{
			Pin:            domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100},
			ManifestSHA256: strings.Repeat("b", 64),
			ConfigSHA256:   strings.Repeat("c", 64),
			LayerCount:     1,
		}
	}
	return identity
}

// runnerAttempts builds one canonical unique attempt UUID per resource.
func runnerAttempts(count int) []string {
	out := make([]string, count)
	for i := range out {
		out[i] = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa%04x", i+1)
	}
	return out
}

// ---------------------------------------------------------------------------
// In-memory journal fake. It mirrors the production store semantics (state
// machine, exact-retry rules, expiry reconciliation on the fake DB clock) so
// the real facade validation runs unchanged above it. It performs no I/O.
// ---------------------------------------------------------------------------

type runnerFakeRes struct {
	spec     domain.ProbeInstallResourceSpec
	state    string
	attempt  string
	code     string
	started  time.Time
	finished time.Time
}

type runnerFakeState struct {
	identity domain.ProbeInstallIdentity
	owner    string
	state    string
	code     string
	created  time.Time
	updated  time.Time
	deadline time.Time
	res      []runnerFakeRes
}

type runnerFakeCall struct {
	method   string
	kind     string
	attempt  string
	outcome  string
	deadline time.Duration
	canceled bool
}

type runnerFakeJournal struct {
	mu     sync.Mutex
	op     *runnerFakeState
	dbNow  time.Time
	taken  map[string]bool
	calls  []runnerFakeCall
	counts struct{ create, observe, begin, cancel, finish, reserve, record, grants int }

	observeFailAt map[int]error // fail the Nth ObserveInstall call
	observeErr    error         // fail every ObserveInstall call while set
	failBefore    map[string]error
	failAfter     map[string]error
	onCommit      map[string]func(ctx context.Context)
}

func newRunnerFakeJournal() *runnerFakeJournal {
	return &runnerFakeJournal{
		dbNow:         runnerBaseTime(),
		taken:         map[string]bool{},
		observeFailAt: map[int]error{},
		failBefore:    map[string]error{},
		failAfter:     map[string]error{},
		onCommit:      map[string]func(ctx context.Context){},
	}
}

func (f *runnerFakeJournal) advance() time.Time {
	f.dbNow = f.dbNow.Add(time.Second)
	return f.dbNow
}

func (f *runnerFakeJournal) receipt() *domain.ProbeInstallOperation {
	operation := &domain.ProbeInstallOperation{
		Identity:   f.op.identity,
		OwnerID:    f.op.owner,
		State:      f.op.state,
		ErrorCode:  f.op.code,
		CreatedAt:  f.op.created,
		UpdatedAt:  f.op.updated,
		DeadlineAt: f.op.deadline,
	}
	operation.Resources = make([]domain.ProbeInstallResource, len(f.op.res))
	for i, resource := range f.op.res {
		operation.Resources[i] = domain.ProbeInstallResource{
			Spec:       resource.spec,
			State:      resource.state,
			AttemptID:  resource.attempt,
			ErrorCode:  resource.code,
			StartedAt:  resource.started,
			FinishedAt: resource.finished,
		}
	}
	if !operation.Valid() {
		panic("runner fake produced an invalid receipt")
	}
	return operation
}

// reconcile mirrors the store's expiry reconciliation on the fake DB clock.
func (f *runnerFakeJournal) reconcile() {
	if f.op == nil || isRunnerTerminal(f.op.state) || f.dbNow.Before(f.op.deadline) {
		return
	}
	for i := range f.op.res {
		if f.op.res[i].state != domain.ProbeInstallResourceInFlight {
			continue
		}
		finished := f.advance()
		f.op.res[i].state = domain.ProbeInstallResourceUnknown
		f.op.res[i].code = "deadline_expired"
		f.op.res[i].finished = finished
	}
	f.op.state = domain.ProbeInstallInterrupted
	f.op.code = "deadline_expired"
	f.op.updated = f.advance()
}

func isRunnerTerminal(state string) bool {
	return state == domain.ProbeInstallFailed || state == domain.ProbeInstallCanceled || state == domain.ProbeInstallInterrupted
}

func isRunnerJoined(state string) bool {
	return state == domain.ProbeInstallRunning || state == domain.ProbeInstallResourcesReady || state == domain.ProbeInstallCancelRequested
}

func (f *runnerFakeJournal) log(ctx context.Context, method, kind, attempt, outcome string) {
	entry := runnerFakeCall{method: method, kind: kind, attempt: attempt, outcome: outcome, canceled: ctx.Err() != nil}
	if deadline, ok := ctx.Deadline(); ok {
		entry.deadline = time.Until(deadline)
	}
	f.calls = append(f.calls, entry)
}

func (f *runnerFakeJournal) index(kind string) (int, bool) {
	for i := range f.op.res {
		if f.op.res[i].spec.Kind == kind {
			return i, true
		}
	}
	return 0, false
}

func (f *runnerFakeJournal) CreateInstall(ctx context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "create", "", "", "")
	f.counts.create++
	if f.op != nil {
		if f.op.identity != request {
			return nil, false, ports.ErrConflict
		}
		return f.receipt(), false, nil
	}
	now := f.advance()
	f.op = &runnerFakeState{
		identity: request,
		owner:    ownerID,
		state:    domain.ProbeInstallQueued,
		created:  now,
		updated:  now,
		deadline: now.Add(domain.ProbeInstallLifetime),
	}
	for _, spec := range request.Resources() {
		f.op.res = append(f.op.res, runnerFakeRes{spec: spec, state: domain.ProbeInstallResourcePlanned})
	}
	out := f.receipt()
	hook = f.onCommit["create"]
	return out, true, nil
}

func (f *runnerFakeJournal) ObserveInstall(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "observe", "", "", "")
	f.counts.observe++
	if err := f.observeFailAt[f.counts.observe]; err != nil {
		return nil, err
	}
	if f.observeErr != nil {
		return nil, f.observeErr
	}
	if f.op == nil || f.op.identity.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	out := f.receipt()
	hook = f.onCommit["observe"]
	return out, nil
}

func (f *runnerFakeJournal) BeginInstall(ctx context.Context, operationID, ownerID string) (*domain.ProbeInstallOperation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "begin", "", "", "")
	f.counts.begin++
	if f.op == nil || f.op.identity.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	if current.OwnerID != ownerID || current.State != domain.ProbeInstallQueued || !f.dbNow.Before(current.DeadlineAt) {
		return nil, ports.ErrConflict
	}
	now := f.advance()
	f.op.state = domain.ProbeInstallRunning
	f.op.code = ""
	f.op.updated = now
	f.counts.grants++
	out := f.receipt()
	hook = f.onCommit["begin"]
	if err := f.failAfter["begin"]; err != nil {
		return nil, err
	}
	return out, nil
}

func (f *runnerFakeJournal) RequestInstallCancellation(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "cancel", "", "", "")
	f.counts.cancel++
	if f.op == nil || f.op.identity.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	if current.Terminal() || current.State == domain.ProbeInstallCancelRequested {
		return current, nil
	}
	now := f.advance()
	state, code := domain.ProbeInstallCancelRequested, ""
	if current.State == domain.ProbeInstallQueued {
		state, code = domain.ProbeInstallCanceled, "canceled"
	}
	f.op.state = state
	f.op.code = code
	f.op.updated = now
	out := f.receipt()
	hook = f.onCommit["cancel"]
	return out, nil
}

func (f *runnerFakeJournal) FinishInstall(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "finish", "", "", outcome.State+":"+outcome.ErrorCode)
	f.counts.finish++
	if err := f.failBefore["finish"]; err != nil {
		return nil, err
	}
	if f.op == nil || f.op.identity.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	if current.Terminal() {
		if current.OwnerID == ownerID && current.State == outcome.State && current.ErrorCode == outcome.ErrorCode {
			return current, nil
		}
		return nil, ports.ErrConflict
	}
	if current.OwnerID != ownerID || !isRunnerJoined(current.State) || !f.dbNow.Before(current.DeadlineAt) {
		return nil, ports.ErrConflict
	}
	if outcome.State == domain.ProbeInstallCanceled && current.State != domain.ProbeInstallCancelRequested {
		return nil, ports.ErrConflict
	}
	switch outcome.State {
	case domain.ProbeInstallFailed, domain.ProbeInstallCanceled:
		for _, resource := range f.op.res {
			if resource.state == domain.ProbeInstallResourceInFlight || resource.state == domain.ProbeInstallResourceUnknown {
				return nil, ports.ErrConflict
			}
		}
	case domain.ProbeInstallInterrupted:
		for i := range f.op.res {
			if f.op.res[i].state != domain.ProbeInstallResourceInFlight {
				continue
			}
			finished := f.advance()
			f.op.res[i].state = domain.ProbeInstallResourceUnknown
			f.op.res[i].code = outcome.ErrorCode
			f.op.res[i].finished = finished
		}
	}
	now := f.advance()
	f.op.state = outcome.State
	f.op.code = outcome.ErrorCode
	f.op.updated = now
	out := f.receipt()
	hook = f.onCommit["finish"]
	if err := f.failAfter["finish"]; err != nil {
		return nil, err
	}
	return out, nil
}

func (f *runnerFakeJournal) ReserveInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "reserve", kind, attemptID, "")
	f.counts.reserve++
	if f.op == nil || f.op.identity.OperationID != operationID {
		return nil, false, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	index, ok := f.index(kind)
	if !ok {
		return nil, false, ports.ErrNotFound
	}
	resource := f.op.res[index]
	if resource.state != domain.ProbeInstallResourcePlanned {
		if current.OwnerID == ownerID && resource.attempt == attemptID {
			hook = f.onCommit["reserve"]
			return current, false, nil
		}
		return nil, false, ports.ErrConflict
	}
	if current.OwnerID != ownerID || current.State != domain.ProbeInstallRunning || !f.dbNow.Before(current.DeadlineAt) {
		return nil, false, ports.ErrConflict
	}
	for i := 0; i < index; i++ {
		if f.op.res[i].state != domain.ProbeInstallResourceApplied {
			return nil, false, ports.ErrConflict
		}
	}
	if f.taken[attemptID] {
		return nil, false, ports.ErrConflict
	}
	now := f.advance()
	f.op.res[index] = runnerFakeRes{
		spec:    resource.spec,
		state:   domain.ProbeInstallResourceInFlight,
		attempt: attemptID,
		started: now,
	}
	f.op.updated = now
	f.taken[attemptID] = true
	out := f.receipt()
	hook = f.onCommit["reserve"]
	if err := f.failAfter["reserve"]; err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func (f *runnerFakeJournal) RecordInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "record", kind, attemptID, outcome.State+":"+outcome.ErrorCode)
	f.counts.record++
	if f.op == nil || f.op.identity.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	index, ok := f.index(kind)
	if !ok {
		return nil, ports.ErrNotFound
	}
	resource := f.op.res[index]
	if current.OwnerID != ownerID {
		return nil, ports.ErrConflict
	}
	if resource.state == domain.ProbeInstallResourcePlanned || resource.attempt != attemptID {
		return nil, ports.ErrConflict
	}
	if resource.state != domain.ProbeInstallResourceInFlight {
		if resource.state == outcome.State && resource.code == outcome.ErrorCode {
			return current, nil
		}
		return nil, ports.ErrConflict
	}
	if current.State != domain.ProbeInstallRunning && current.State != domain.ProbeInstallCancelRequested {
		return nil, ports.ErrConflict
	}
	if !f.dbNow.Before(current.DeadlineAt) {
		return nil, ports.ErrConflict
	}
	now := f.advance()
	f.op.res[index].state = outcome.State
	f.op.res[index].code = outcome.ErrorCode
	if outcome.State == domain.ProbeInstallResourceApplied {
		f.op.res[index].code = ""
	}
	f.op.res[index].finished = now
	opState, opCode := current.State, ""
	switch outcome.State {
	case domain.ProbeInstallResourceApplied:
		allApplied := true
		for _, resource := range f.op.res {
			if resource.state != domain.ProbeInstallResourceApplied {
				allApplied = false
				break
			}
		}
		if allApplied && current.State == domain.ProbeInstallRunning {
			opState = domain.ProbeInstallResourcesReady
		}
	case domain.ProbeInstallResourceNotStarted:
		// The operation keeps running so the owner can finish it honestly.
	case domain.ProbeInstallResourceUnknown:
		opState = domain.ProbeInstallInterrupted
		opCode = outcome.ErrorCode
	}
	f.op.state = opState
	f.op.code = opCode
	f.op.updated = now
	out := f.receipt()
	hook = f.onCommit["record"]
	if err := f.failAfter["record"]; err != nil {
		return nil, err
	}
	return out, nil
}

// Test-side mutation helpers (they model concurrent operators/corruption).

func (f *runnerFakeJournal) setAttempt(kind, attempt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index, ok := f.index(kind); ok {
		f.op.res[index].attempt = attempt
	}
}

func (f *runnerFakeJournal) setOwner(owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.op.owner = owner
}

// stageResource commits a resource receipt without a port call, modeling a
// concurrent/earlier writer on another replica.
func (f *runnerFakeJournal) stageResource(kind, state, attempt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index, ok := f.index(kind)
	if !ok {
		return
	}
	now := f.advance()
	f.op.res[index].state = state
	f.op.res[index].attempt = attempt
	f.op.res[index].code = ""
	f.op.res[index].started = now
	if state == domain.ProbeInstallResourceInFlight {
		f.op.res[index].finished = time.Time{}
	} else {
		f.op.res[index].finished = now
	}
	f.op.updated = now
	f.taken[attempt] = true
}

// stageCancelIntent records durable cancellation intent without a port call.
func (f *runnerFakeJournal) stageCancelIntent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.op == nil || isRunnerTerminal(f.op.state) {
		return
	}
	now := f.advance()
	f.op.state = domain.ProbeInstallCancelRequested
	f.op.code = ""
	f.op.updated = now
}

func (f *runnerFakeJournal) expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbNow = f.op.deadline.Add(time.Second)
}

func (f *runnerFakeJournal) forceDeadlineExpired() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbNow = f.op.deadline.Add(time.Second)
	f.reconcile()
}

func (f *runnerFakeJournal) snapshot() *domain.ProbeInstallOperation {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconcile()
	return f.receipt()
}

func (f *runnerFakeJournal) countsSnapshot() (reserve, record, finish, cancel int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts.reserve, f.counts.record, f.counts.finish, f.counts.cancel
}

func (f *runnerFakeJournal) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *runnerFakeJournal) callLog() []runnerFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runnerFakeCall(nil), f.calls...)
}

// ---------------------------------------------------------------------------
// Scripted executor. The default report is a definite joined applied outcome.
// ---------------------------------------------------------------------------

type runnerFakeExecutor struct {
	mu    sync.Mutex
	calls []string
	apply func(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error)
}

func (e *runnerFakeExecutor) ApplyResource(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
	e.mu.Lock()
	e.calls = append(e.calls, kind)
	hook := e.apply
	e.mu.Unlock()
	if hook == nil {
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	}
	return hook(ctx, operation, kind)
}

func (e *runnerFakeExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *runnerFakeExecutor) callSnapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// ---------------------------------------------------------------------------
// Fixture wiring: fake ports under the real facade, real runner above it.
// ---------------------------------------------------------------------------

type runnerFixture struct {
	runner   *ProbeInstallRunner
	journal  *ProbeInstallJournalService
	fake     *runnerFakeJournal
	exec     *runnerFakeExecutor
	identity domain.ProbeInstallIdentity
	owner    string
	attempts []string
}

func runnerSetupBase(t *testing.T, mode string, base time.Time) *runnerFixture {
	t.Helper()
	fake := newRunnerFakeJournal()
	fake.dbNow = base.Add(-time.Second)
	exec := &runnerFakeExecutor{}
	journal, err := NewProbeInstallJournalService(fake, fake)
	if err != nil || journal == nil {
		t.Fatalf("journal constructor failed: %v", err)
	}
	runner, err := NewProbeInstallRunner(journal, exec)
	if err != nil || runner == nil {
		t.Fatalf("runner constructor failed: %v", err)
	}
	identity := runnerIdentity(mode, runnerOpID)
	operation, created, err := journal.Create(context.Background(), identity, runnerOwner)
	if err != nil || !created || operation == nil || operation.State != domain.ProbeInstallQueued {
		t.Fatalf("fixture admission failed: created=%v err=%v", created, err)
	}
	return &runnerFixture{
		runner:   runner,
		journal:  journal,
		fake:     fake,
		exec:     exec,
		identity: identity,
		owner:    runnerOwner,
		attempts: runnerAttempts(len(identity.Resources())),
	}
}

func runnerSetup(t *testing.T, mode string) *runnerFixture {
	t.Helper()
	return runnerSetupBase(t, mode, runnerBaseTime())
}

// ---------------------------------------------------------------------------
// Construction and input validation
// ---------------------------------------------------------------------------

func TestInstallRunnerConstructionAndFixedDefaults(t *testing.T) {
	exec := &runnerFakeExecutor{}
	if _, err := NewProbeInstallRunner(nil, exec); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil journal accepted: %v", err)
	}
	if _, err := NewProbeInstallRunner(&ProbeInstallJournalService{}, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil executor accepted: %v", err)
	}
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	if fx.runner.pollInterval != time.Second {
		t.Fatalf("production poll interval is not fixed at 1s: %v", fx.runner.pollInterval)
	}
	if fx.runner.runBound != domain.ProbeInstallLifetime {
		t.Fatalf("production run bound is not fixed at %v: %v", domain.ProbeInstallLifetime, fx.runner.runBound)
	}
}

func TestInstallRunnerRejectsInvalidInputsBeforeIO(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name     string
		ctx      context.Context
		identity domain.ProbeInstallIdentity
		owner    string
		attempts []string
		want     error
	}{
		{"nil context", nil, fx.identity, fx.owner, fx.attempts, domain.ErrValidation},
		{"canceled context", canceled, fx.identity, fx.owner, fx.attempts, context.Canceled},
		{"invalid identity", context.Background(), domain.ProbeInstallIdentity{}, fx.owner, fx.attempts, domain.ErrValidation},
		{"invalid owner", context.Background(), fx.identity, "not-a-uuid", fx.attempts, domain.ErrValidation},
		{"wrong attempt count", context.Background(), fx.identity, fx.owner, fx.attempts[:1], domain.ErrValidation},
		{"duplicate attempts", context.Background(), fx.identity, fx.owner,
			[]string{fx.attempts[0], fx.attempts[0], fx.attempts[2], fx.attempts[3]}, domain.ErrValidation},
		{"non canonical attempt", context.Background(), fx.identity, fx.owner,
			[]string{"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", fx.attempts[1], fx.attempts[2], fx.attempts[3]}, domain.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := fx.fake.totalCalls()
			operation, err := fx.runner.Run(tc.ctx, tc.identity, tc.owner, tc.attempts)
			if !errors.Is(err, tc.want) || operation != nil {
				t.Fatalf("Run = (%v, %v), want (nil, %v)", operation, err, tc.want)
			}
			if after := fx.fake.totalCalls(); after != before {
				t.Fatalf("invalid input performed %d journal calls", after-before)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The observe gate and the single Begin
// ---------------------------------------------------------------------------

func TestInstallRunnerObserveGateBeforeBegin(t *testing.T) {
	t.Run("foreign identity never begins", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		other := fx.identity
		other.Host.User = "intruder"
		operation, err := fx.runner.Run(context.Background(), other, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", operation, err)
		}
		for _, call := range fx.fake.callLog() {
			if call.method == "begin" || call.method == "reserve" || call.method == "record" || call.method == "finish" {
				t.Fatalf("foreign identity triggered %s", call.method)
			}
		}
	})

	t.Run("foreign owner never begins", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		operation, err := fx.runner.Run(context.Background(), fx.identity, runnerOwnerTwo, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", operation, err)
		}
		for _, call := range fx.fake.callLog() {
			if call.method != "create" && call.method != "observe" {
				t.Fatalf("foreign owner triggered %s", call.method)
			}
		}
	})

	t.Run("non queued state never begins twice", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		if _, err := fx.journal.Begin(context.Background(), fx.identity.OperationID, fx.owner); err != nil {
			t.Fatalf("seed begin failed: %v", err)
		}
		beginsBefore := fx.fake.counts.begin
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", operation, err)
		}
		if fx.fake.counts.begin != beginsBefore {
			t.Fatalf("runner attempted a second begin")
		}
	})

	t.Run("terminal state never begins", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		if _, err := fx.journal.Cancel(context.Background(), fx.identity.OperationID); err != nil {
			t.Fatalf("seed cancel failed: %v", err)
		}
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", operation, err)
		}
		if fx.fake.counts.begin != 0 {
			t.Fatalf("runner began over a terminal state")
		}
	})

	t.Run("corrupt observation never leaks diagnostics", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.fake.observeFailAt[1] = errors.New(runnerLeakMarker)
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, domain.ErrInternal) || operation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, internal)", operation, err)
		}
		if strings.Contains(err.Error(), runnerLeakMarker) {
			t.Fatalf("storage diagnostics leaked: %v", err)
		}
	})
}

func TestInstallRunnerBeginLossNeverDispatchesFinishesOrInterrupts(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The begin commits, then the acknowledgement is lost: the facade's
	// post-commit context check discards the receipt exactly as in production.
	fx.fake.onCommit["begin"] = func(context.Context) { cancel() }

	operation, err := fx.runner.Run(ctx, fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, context.Canceled) || operation != nil {
		t.Fatalf("Run = (%v, %v), want (nil, canceled)", operation, err)
	}
	reserve, record, finish, cancels := fx.fake.countsSnapshot()
	if reserve+record+finish != 0 || cancels != 0 {
		t.Fatalf("lost begin ack wrote: reserve=%d record=%d finish=%d cancel=%d", reserve, record, finish, cancels)
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("lost begin ack dispatched the executor")
	}
	stored := fx.fake.snapshot()
	if stored.State != domain.ProbeInstallRunning {
		t.Fatalf("committed begin not preserved: %s", stored.State)
	}

	// Cold restart: the winning run is never dispatched over nor finalized.
	operation, err = fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, ports.ErrConflict) || operation != nil {
		t.Fatalf("cold retry = (%v, %v), want (nil, conflict)", operation, err)
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("cold retry dispatched the executor")
	}
	reserve, record, finish, _ = fx.fake.countsSnapshot()
	if reserve+record+finish != 0 {
		t.Fatalf("cold retry wrote history")
	}
}

func TestInstallRunnerCompetingRunsSingleBeginGrant(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = 2 * time.Millisecond

	const competitors = 2
	type result struct {
		operation *domain.ProbeInstallOperation
		err       error
	}
	results := make(chan result, competitors)
	var ready sync.WaitGroup
	ready.Add(competitors)
	for i := 0; i < competitors; i++ {
		go func() {
			ready.Done()
			operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
			results <- result{operation, err}
		}()
	}
	ready.Wait()
	var wins, losses int
	for i := 0; i < competitors; i++ {
		got := <-results
		switch {
		case got.err == nil && got.operation != nil && got.operation.State == domain.ProbeInstallResourcesReady:
			wins++
		case errors.Is(got.err, ports.ErrConflict) && got.operation == nil:
			losses++
		default:
			t.Fatalf("unexpected competitor result: (%v, %v)", got.operation, got.err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins=%d losses=%d, want exactly one of each", wins, losses)
	}
	if fx.fake.counts.grants != 1 {
		t.Fatalf("begin grants = %d, want exactly 1", fx.fake.counts.grants)
	}
	if got := fx.exec.callCount(); got != len(fx.identity.Resources()) {
		t.Fatalf("executor calls = %d, want %d (one full sequence)", got, len(fx.identity.Resources()))
	}
	if _, _, _, cancels := fx.fake.countsSnapshot(); cancels != 0 {
		t.Fatalf("runner invented cancellation intent %d times", cancels)
	}
}

// ---------------------------------------------------------------------------
// The happy paths
// ---------------------------------------------------------------------------

func TestInstallRunnerResourcesReadySystemd(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if err != nil || operation == nil {
		t.Fatalf("Run = (%v, %v), want readiness", operation, err)
	}
	if operation.State != domain.ProbeInstallResourcesReady || operation.ErrorCode != "" || !operation.Valid() {
		t.Fatalf("bad readiness receipt: state=%s code=%s valid=%v", operation.State, operation.ErrorCode, operation.Valid())
	}
	specs := fx.identity.Resources()
	if fx.exec.callCount() != len(specs) {
		t.Fatalf("executor calls = %d, want %d", fx.exec.callCount(), len(specs))
	}
	for i, spec := range specs {
		if got := operation.Resources[i].Spec; got != spec {
			t.Fatalf("resource %d spec = %+v, want %+v", i, got, spec)
		}
		if operation.Resources[i].State != domain.ProbeInstallResourceApplied ||
			operation.Resources[i].AttemptID != fx.attempts[i] {
			t.Fatalf("resource %d not bound to its attempt: %+v", i, operation.Resources[i])
		}
	}
	// Dependent work only after the predecessor's record commit.
	log := fx.fake.callLog()
	recordAt, reserveAt := map[string]int{}, map[string]int{}
	for i, call := range log {
		switch call.method {
		case "record":
			recordAt[call.kind] = i
		case "reserve":
			reserveAt[call.kind] = i
		}
	}
	for i := 1; i < len(specs); i++ {
		previous, current := specs[i-1].Kind, specs[i].Kind
		if recordAt[previous] == 0 || reserveAt[current] == 0 || recordAt[previous] > reserveAt[current] {
			t.Fatalf("reserve(%s) at %d not after record(%s) at %d", current, reserveAt[current], previous, recordAt[previous])
		}
	}
	// Kinds reach the executor in canonical layout order.
	execOrder := fx.exec.callSnapshot()
	for i, spec := range specs {
		if execOrder[i] != spec.Kind {
			t.Fatalf("executor order = %v, want layout order", execOrder)
		}
	}
	if _, _, _, cancels := fx.fake.countsSnapshot(); cancels != 0 {
		t.Fatalf("runner invented cancellation intent")
	}
}

func TestInstallRunnerResourcesReadyDocker(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallDocker)
	operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if err != nil || operation == nil {
		t.Fatalf("Run = (%v, %v), want readiness", operation, err)
	}
	if operation.State != domain.ProbeInstallResourcesReady || !operation.Valid() {
		t.Fatalf("bad readiness receipt: state=%s valid=%v", operation.State, operation.Valid())
	}
	specs := fx.identity.Resources()
	if specs[0].Kind != "image_cache" || specs[len(specs)-1].Kind != "container" {
		t.Fatalf("unexpected docker layout: %+v", specs)
	}
	for i := range specs {
		if operation.Resources[i].State != domain.ProbeInstallResourceApplied || operation.Resources[i].AttemptID != fx.attempts[i] {
			t.Fatalf("resource %d not applied with its attempt: %+v", i, operation.Resources[i])
		}
	}
	if fx.exec.callCount() != len(specs) {
		t.Fatalf("executor calls = %d, want %d", fx.exec.callCount(), len(specs))
	}
}

// ---------------------------------------------------------------------------
// Grants, lost acknowledgements and retries
// ---------------------------------------------------------------------------

func TestInstallRunnerHistoricalGrantNeverInvokes(t *testing.T) {
	t.Run("exact reservation retry is history", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		spec := fx.identity.Resources()[0]
		// A prior attempt committed the in-flight intent whose exact retry the
		// journal returns as history: created=false is never a grant.
		fx.fake.onCommit["begin"] = func(context.Context) {
			fx.fake.stageResource(spec.Kind, domain.ProbeInstallResourceInFlight, fx.attempts[0])
		}
		before := fx.fake.totalCalls()
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, conflict)", operation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("historical grant invoked the executor")
		}
		reserve, record, finish, _ := fx.fake.countsSnapshot()
		if record+finish != 0 {
			t.Fatalf("historical grant wrote: record=%d finish=%d", record, finish)
		}
		if reserve == 0 || fx.fake.totalCalls() <= before {
			t.Fatalf("historical grant was never even read")
		}
	})

	t.Run("completed attempt never replays", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		spec := fx.identity.Resources()[0]
		fx.fake.onCommit["begin"] = func(context.Context) {
			fx.fake.stageResource(spec.Kind, domain.ProbeInstallResourceApplied, runnerAttemptTwo)
		}
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation == nil {
			t.Fatalf("Run = (%v, %v), want conflict over history", operation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("historical attempt invoked the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("historical attempt wrote history")
		}
	})
}

func TestInstallRunnerLostReservationAckNoInvocation(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The reservation commits and then its acknowledgement is lost.
	fx.fake.onCommit["reserve"] = func(context.Context) { cancel() }

	operation, err := fx.runner.Run(ctx, fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, context.Canceled) || operation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, canceled)", operation, err)
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("lost reservation ack invoked the executor")
	}
	if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
		t.Fatalf("lost reservation ack wrote: record=%d finish=%d", record, finish)
	}
	stored := fx.fake.snapshot()
	if stored.Resources[0].State != domain.ProbeInstallResourceInFlight ||
		stored.Resources[0].AttemptID != fx.attempts[0] {
		t.Fatalf("held intent not preserved for cold reconciliation: %+v", stored.Resources[0])
	}

	// Retry and cold restart never invoke from history.
	if _, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts); err == nil {
		t.Fatalf("cold retry unexpectedly succeeded")
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("retry/cold restart invoked the executor")
	}
}

func TestInstallRunnerFinishFailureLeavesHonestState(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.fake.failBefore["finish"] = context.DeadlineExceeded
	fx.exec.apply = func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}, nil
	}
	operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, context.DeadlineExceeded) || operation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, deadline)", operation, err)
	}
	stored := fx.fake.snapshot()
	if stored.Terminal() {
		t.Fatalf("terminal state fabricated locally: %s/%s", stored.State, stored.ErrorCode)
	}
	if stored.Resources[0].State != domain.ProbeInstallResourceNotStarted {
		t.Fatalf("joined report lost: %+v", stored.Resources[0])
	}
}

// ---------------------------------------------------------------------------
// Cancellation, joining and interruption
// ---------------------------------------------------------------------------

func TestInstallRunnerLostRecordAckStopsWithoutRedispatch(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = time.Hour
	// The joined report commits and then its acknowledgement is lost.
	fx.fake.failAfter["record"] = context.Canceled

	operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, context.Canceled) || operation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, canceled)", operation, err)
	}
	if reserve, record, finish, _ := fx.fake.countsSnapshot(); reserve != 1 || record != 1 || finish != 0 {
		t.Fatalf("reserve=%d record=%d finish=%d, want 1/1/0", reserve, record, finish)
	}
	if fx.exec.callCount() != 1 {
		t.Fatalf("executor calls = %d, want 1 (no redispatch)", fx.exec.callCount())
	}
	if stored := fx.fake.snapshot(); stored.Resources[0].State != domain.ProbeInstallResourceApplied {
		t.Fatalf("committed report not preserved: %+v", stored.Resources[0])
	}
}

func TestInstallRunnerHeldInvocationJoinsBeforeTerminal(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = 2 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})
	canceledObserved := make(chan struct{})
	var once sync.Once
	fx.exec.apply = func(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
		once.Do(func() { close(started) })
		go func() {
			<-ctx.Done()
			close(canceledObserved)
		}()
		<-release
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	}

	done := make(chan struct{})
	var operation *domain.ProbeInstallOperation
	var runErr error
	go func() {
		defer close(done)
		operation, runErr = fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	}()
	<-started

	// Durable operator cancellation while the invocation is held.
	if _, err := fx.journal.Cancel(context.Background(), fx.identity.OperationID); err != nil {
		t.Fatalf("operator cancel failed: %v", err)
	}
	select {
	case <-canceledObserved:
	case <-time.After(3 * time.Second):
		t.Fatalf("watcher never revoked the in-flight action")
	}

	// The watcher must not let the coordinator write a terminal outcome or
	// return while the executor has not joined.
	time.Sleep(20 * fx.runner.pollInterval)
	reserve, record, finish, _ := fx.fake.countsSnapshot()
	if record+finish != 0 {
		t.Fatalf("premature terminal writes while held: record=%d finish=%d", record, finish)
	}
	select {
	case <-done:
		t.Fatalf("Run returned before the executor joined")
	default:
	}
	_ = reserve

	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after the executor joined")
	}
	if runErr != nil || operation == nil || operation.State != domain.ProbeInstallCanceled {
		t.Fatalf("Run = (%v, %v), want terminal canceled", operation, runErr)
	}
	stored := fx.fake.snapshot()
	if stored.State != domain.ProbeInstallCanceled || stored.ErrorCode != "canceled" {
		t.Fatalf("stored terminal = %s/%s, want canceled", stored.State, stored.ErrorCode)
	}
	if stored.Resources[0].State != domain.ProbeInstallResourceApplied {
		t.Fatalf("joined applied report not preserved: %+v", stored.Resources[0])
	}

	// No watcher or executor survives the return.
	settled := fx.fake.totalCalls()
	executed := fx.exec.callCount()
	time.Sleep(20 * fx.runner.pollInterval)
	if fx.fake.totalCalls() != settled || fx.exec.callCount() != executed {
		t.Fatalf("work continued after Run returned")
	}
}

func TestInstallRunnerCallerCancelPreservesJoinedApplied(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var once sync.Once
	fx.exec.apply = func(callCtx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
		once.Do(func() { close(started) })
		<-callCtx.Done()
		// A definitely joined applied report racing cancellation stays applied.
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	}

	done := make(chan struct{})
	var operation *domain.ProbeInstallOperation
	var runErr error
	go func() {
		defer close(done)
		operation, runErr = fx.runner.Run(ctx, fx.identity, fx.owner, fx.attempts)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after caller cancellation")
	}
	if !errors.Is(runErr, context.Canceled) || operation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, canceled)", operation, runErr)
	}
	if operation.State != domain.ProbeInstallInterrupted || operation.ErrorCode != "owner_interrupted" {
		t.Fatalf("terminal = %s/%s, want interrupted/owner_interrupted", operation.State, operation.ErrorCode)
	}
	stored := fx.fake.snapshot()
	if stored.Resources[0].State != domain.ProbeInstallResourceApplied {
		t.Fatalf("definite applied report lost to cancellation: %+v", stored.Resources[0])
	}
	// Recording and finishing used independent bounded contexts even though
	// the caller was already canceled.
	for _, call := range fx.fake.callLog() {
		if call.method != "record" && call.method != "finish" {
			continue
		}
		if call.canceled {
			t.Fatalf("%s ran on an already-canceled caller context", call.method)
		}
		if call.deadline <= 0 || call.deadline > installRunnerMetadataTimeout+100*time.Millisecond {
			t.Fatalf("%s context is not bounded at %v: %v", call.method, installRunnerMetadataTimeout, call.deadline)
		}
	}
}

func TestInstallRunnerPreInvokeCancelRecordsNotStartedThenCanceled(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = time.Hour // watcher must not interfere
	// The operator cancels after the grant commits but before any invocation.
	fx.fake.onCommit["reserve"] = func(context.Context) {
		// Durable operator intent on another replica; not a port call from the
		// runner, so the invented-cancellation assertion stays meaningful.
		fx.fake.stageCancelIntent()
	}

	operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if err != nil || operation == nil || operation.State != domain.ProbeInstallCanceled {
		t.Fatalf("Run = (%v, %v), want terminal canceled", operation, err)
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("pre-invocation cancel dispatched the executor")
	}
	stored := fx.fake.snapshot()
	if stored.Resources[0].State != domain.ProbeInstallResourceNotStarted {
		t.Fatalf("never-invoked grant not recorded not_started: %+v", stored.Resources[0])
	}
	if stored.Resources[0].ErrorCode != "not_started" {
		t.Fatalf("not_started code = %q", stored.Resources[0].ErrorCode)
	}
	if _, _, _, cancels := fx.fake.countsSnapshot(); cancels != 0 {
		t.Fatalf("runner invented cancellation intent")
	}
}

// ---------------------------------------------------------------------------
// Executor report trust
// ---------------------------------------------------------------------------

func TestInstallRunnerUntrustedReportsInterruptAndStopDependents(t *testing.T) {
	cases := []struct {
		name  string
		apply func() (domain.ProbeInstallResourceOutcome, error)
	}{
		{"executor error", func() (domain.ProbeInstallResourceOutcome, error) {
			return domain.ProbeInstallResourceOutcome{}, errors.New(runnerLeakMarker)
		}},
		{"invalid outcome", func() (domain.ProbeInstallResourceOutcome, error) {
			return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied, ErrorCode: "oops"}, nil
		}},
		{"empty outcome", func() (domain.ProbeInstallResourceOutcome, error) {
			return domain.ProbeInstallResourceOutcome{}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := runnerSetup(t, domain.ProbeInstallSystemd)
			fx.runner.pollInterval = time.Hour
			fx.exec.apply = func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
				return tc.apply()
			}
			operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
			if !errors.Is(err, domain.ErrInternal) || operation == nil {
				t.Fatalf("Run = (%v, %v), want (receipt, internal)", operation, err)
			}
			if strings.Contains(fmt.Sprint(err), runnerLeakMarker) {
				t.Fatalf("executor diagnostics leaked: %v", err)
			}
			if operation.State != domain.ProbeInstallInterrupted || operation.ErrorCode != "outcome_unknown" {
				t.Fatalf("terminal = %s/%s, want interrupted/outcome_unknown", operation.State, operation.ErrorCode)
			}
			stored := fx.fake.snapshot()
			if stored.Resources[0].State != domain.ProbeInstallResourceUnknown ||
				stored.Resources[0].ErrorCode != "outcome_unknown" {
				t.Fatalf("uncertainty not preserved: %+v", stored.Resources[0])
			}
			if !stored.Valid() {
				t.Fatalf("stored graph invalid: %+v", stored)
			}
			// No dependent dispatch, no finish over the atomic interruption.
			if fx.exec.callCount() != 1 {
				t.Fatalf("dependents dispatched after an unknown report")
			}
			if reserve, _, finish, _ := fx.fake.countsSnapshot(); reserve != 1 || finish != 0 {
				t.Fatalf("reserve=%d finish=%d, want 1/0", reserve, finish)
			}
		})
	}
}

func TestInstallRunnerTrustedNotStartedClosesFailedWithoutDependents(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = time.Hour
	fx.exec.apply = func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}, nil
	}
	operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if err != nil || operation == nil {
		t.Fatalf("Run = (%v, %v), want closed failure", operation, err)
	}
	if operation.State != domain.ProbeInstallFailed || operation.ErrorCode != "not_started" {
		t.Fatalf("terminal = %s/%s, want failed/not_started", operation.State, operation.ErrorCode)
	}
	stored := fx.fake.snapshot()
	if stored.Resources[0].State != domain.ProbeInstallResourceNotStarted {
		t.Fatalf("report lost: %+v", stored.Resources[0])
	}
	if reserve, _, _, _ := fx.fake.countsSnapshot(); reserve != 1 {
		t.Fatalf("dependent reserved after not_started: %d reserves", reserve)
	}
	if fx.exec.callCount() != 1 {
		t.Fatalf("dependent dispatched after not_started")
	}
}

// ---------------------------------------------------------------------------
// Observational defense, corruption and honest journal time
// ---------------------------------------------------------------------------

func TestInstallRunnerForeignStateNeverWrites(t *testing.T) {
	t.Run("changed attempt stops without writes", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.runner.pollInterval = time.Hour
		spec := fx.identity.Resources()[0]
		fx.fake.onCommit["reserve"] = func(context.Context) {
			fx.fake.setAttempt(spec.Kind, runnerAttemptTwo)
		}
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, conflict)", operation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("changed attempt dispatched the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("changed attempt wrote history")
		}
	})

	t.Run("cold deadline expiry is never overwritten", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.runner.pollInterval = time.Hour
		fx.fake.onCommit["reserve"] = func(context.Context) {
			fx.fake.forceDeadlineExpired()
		}
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, conflict)", operation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("expired state dispatched the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("expired state wrote history")
		}
		stored := fx.fake.snapshot()
		if stored.State != domain.ProbeInstallInterrupted || stored.ErrorCode != "deadline_expired" ||
			stored.Resources[0].ErrorCode != "deadline_expired" {
			t.Fatalf("cold expiry not preserved: %s/%s %+v", stored.State, stored.ErrorCode, stored.Resources[0])
		}
	})

	t.Run("owner change stops without writes", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.runner.pollInterval = time.Hour
		fx.fake.onCommit["reserve"] = func(context.Context) {
			fx.fake.setOwner(runnerOwnerTwo)
		}
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || operation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", operation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("foreign owner dispatched the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("foreign owner wrote history")
		}
	})
}

func TestInstallRunnerObservationLossStopsDispatch(t *testing.T) {
	t.Run("pre-invocation loss records truthful not_started", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.runner.pollInterval = time.Hour
		fx.fake.observeFailAt[2] = errors.New(runnerLeakMarker) // the re-observe
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if !errors.Is(err, domain.ErrInternal) || operation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, internal)", operation, err)
		}
		if strings.Contains(fmt.Sprint(err), runnerLeakMarker) {
			t.Fatalf("storage diagnostics leaked: %v", err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("observation loss dispatched the executor")
		}
		stored := fx.fake.snapshot()
		if stored.Resources[0].State != domain.ProbeInstallResourceNotStarted {
			t.Fatalf("never-invoked grant not truthful: %+v", stored.Resources[0])
		}
		if stored.State != domain.ProbeInstallInterrupted || stored.ErrorCode != "owner_interrupted" {
			t.Fatalf("terminal = %s/%s, want interrupted/owner_interrupted", stored.State, stored.ErrorCode)
		}
	})

	t.Run("watcher loss cancels the action and interrupts", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.runner.pollInterval = 2 * time.Millisecond
		started := make(chan struct{})
		var once sync.Once
		fx.exec.apply = func(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
		}
		done := make(chan struct{})
		var runErr error
		var operation *domain.ProbeInstallOperation
		go func() {
			defer close(done)
			operation, runErr = fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		}()
		<-started
		fx.fake.mu.Lock()
		fx.fake.observeErr = errors.New("storage unavailable")
		fx.fake.mu.Unlock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("Run did not stop after observation loss")
		}
		if !errors.Is(runErr, domain.ErrInternal) || operation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, internal)", operation, runErr)
		}
		stored := fx.fake.snapshot()
		if stored.Resources[0].State != domain.ProbeInstallResourceApplied {
			t.Fatalf("joined applied report lost: %+v", stored.Resources[0])
		}
		if stored.State != domain.ProbeInstallInterrupted || stored.ErrorCode != "owner_interrupted" {
			t.Fatalf("terminal = %s/%s, want interrupted/owner_interrupted", stored.State, stored.ErrorCode)
		}
	})
}

func TestInstallRunnerColdExpiryRejectsLateApplied(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = time.Hour
	proceed := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	fx.exec.apply = func(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
		once.Do(func() { close(started) })
		<-proceed
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	}()
	<-started
	fx.fake.expire() // the DB clock passes the fixed deadline mid-invocation
	close(proceed)
	<-done

	if !errors.Is(runErr, ports.ErrConflict) {
		t.Fatalf("Run error = %v, want conflict", runErr)
	}
	stored := fx.fake.snapshot()
	if stored.State != domain.ProbeInstallInterrupted || stored.ErrorCode != "deadline_expired" {
		t.Fatalf("expiry uncertainty replaced: %s/%s", stored.State, stored.ErrorCode)
	}
	if stored.Resources[0].State != domain.ProbeInstallResourceUnknown ||
		stored.Resources[0].ErrorCode != "deadline_expired" {
		t.Fatalf("late applied repaired unknown: %+v", stored.Resources[0])
	}
	if _, _, finish, _ := fx.fake.countsSnapshot(); finish != 0 {
		t.Fatalf("finish attempted over expiry uncertainty")
	}
}

func TestInstallRunnerRunBoundFiresAsOwnerInterruption(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = time.Hour
	fx.runner.runBound = 40 * time.Millisecond
	fx.exec.apply = func(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
		<-ctx.Done()
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	}
	started := time.Now()
	operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, context.DeadlineExceeded) || operation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, deadline)", operation, err)
	}
	if elapsed := time.Since(started); elapsed > domain.ProbeInstallLifetime {
		t.Fatalf("run bound not honored: %v", elapsed)
	}
	if operation.State != domain.ProbeInstallInterrupted || operation.ErrorCode != "owner_interrupted" {
		t.Fatalf("terminal = %s/%s, want interrupted/owner_interrupted", operation.State, operation.ErrorCode)
	}
	if reserve, _, _, _ := fx.fake.countsSnapshot(); reserve != 1 {
		t.Fatalf("dependent reserved after the run bound fired: %d", reserve)
	}
}

// ---------------------------------------------------------------------------
// Copies, bounded contexts and honest clock behavior
// ---------------------------------------------------------------------------

func TestInstallRunnerDefensiveCopies(t *testing.T) {
	t.Run("attempts are copied before I/O", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.runner.pollInterval = time.Hour
		original := append([]string(nil), fx.attempts...)
		fx.exec.apply = func(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
			// Corrupt the caller's slice mid-run.
			for i := range fx.attempts {
				fx.attempts[i] = runnerAttemptTwo
			}
			return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
		}
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if err != nil || operation == nil || operation.State != domain.ProbeInstallResourcesReady {
			t.Fatalf("Run = (%v, %v), want readiness", operation, err)
		}
		for i, call := range fx.fake.callLog() {
			if call.method != "reserve" {
				continue
			}
			if call.attempt != original[0] && call.attempt != original[1] && call.attempt != original[2] && call.attempt != original[3] {
				t.Fatalf("call %d used a mutated attempt %q", i, call.attempt)
			}
		}
		for i, resource := range operation.Resources {
			if resource.AttemptID != original[i] {
				t.Fatalf("resource %d bound to %q, want %q", i, resource.AttemptID, original[i])
			}
		}
	})

	t.Run("executor receives a defensive copy", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.runner.pollInterval = time.Hour
		fx.exec.apply = func(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
			operation.Resources[0].State = "junk"
			operation.Resources[0].AttemptID = "junk"
			operation.Identity.Host.User = "junk"
			return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
		}
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if err != nil || operation == nil || operation.State != domain.ProbeInstallResourcesReady {
			t.Fatalf("Run = (%v, %v), want readiness", operation, err)
		}
		if !operation.Valid() || operation.Identity != fx.identity || operation.OwnerID != fx.owner {
			t.Fatalf("executor mutation corrupted the coordinator state: %+v", operation)
		}
		stored := fx.fake.snapshot()
		if stored.Resources[0].AttemptID != fx.attempts[0] || stored.Resources[0].State != domain.ProbeInstallResourceApplied {
			t.Fatalf("executor mutation corrupted storage: %+v", stored.Resources[0])
		}
	})

	t.Run("returned receipts own their slices", func(t *testing.T) {
		fx := runnerSetup(t, domain.ProbeInstallSystemd)
		fx.runner.pollInterval = time.Hour
		operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
		if err != nil || operation == nil {
			t.Fatalf("Run = (%v, %v), want readiness", operation, err)
		}
		operation.Resources[0].State = "junk"
		operation.Identity.Host.User = "junk"
		stored := fx.fake.snapshot()
		if stored.Resources[0].State != domain.ProbeInstallResourceApplied || stored.Identity.Host.User != "root" {
			t.Fatalf("returned receipt shares storage state: %+v", stored.Resources[0])
		}
	})
}

func TestInstallRunnerBoundedMetadataAndCompletionContexts(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	fx.runner.pollInterval = 2 * time.Millisecond
	var execDeadline time.Duration
	fx.exec.apply = func(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
		if deadline, ok := ctx.Deadline(); ok {
			execDeadline = time.Until(deadline)
		}
		if ctx.Done() == nil {
			t.Errorf("executor context is not cancelable")
		}
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	}
	if _, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	for _, call := range fx.fake.callLog() {
		if call.method == "create" {
			// Fixture admission, not a runner call.
			continue
		}
		if call.deadline <= 0 {
			t.Fatalf("%s ran on an unbounded context", call.method)
		}
		limit := installRunnerMetadataTimeout + 100*time.Millisecond
		if call.method == "observe" && call.deadline <= installRunnerWatchReadTimeout+100*time.Millisecond {
			limit = installRunnerWatchReadTimeout + 100*time.Millisecond
		}
		if call.deadline > limit {
			t.Fatalf("%s context budget %v exceeds %v", call.method, call.deadline, limit)
		}
	}
	if execDeadline <= 0 || execDeadline > domain.ProbeInstallLifetime {
		t.Fatalf("executor context not bounded by the run bound: %v", execDeadline)
	}
}

func TestInstallRunnerTrustsJournalClockNotHostClock(t *testing.T) {
	// The stored deadline is far in the host's past: a host-clock comparison
	// would refuse to dispatch, but DB authority is the journal's state.
	fx := runnerSetupBase(t, domain.ProbeInstallSystemd, time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC))
	fx.runner.pollInterval = time.Hour
	if !fx.fake.snapshot().DeadlineAt.Before(time.Now()) {
		t.Fatalf("fixture deadline is not in the host past")
	}
	operation, err := fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
	if err != nil || operation == nil || operation.State != domain.ProbeInstallResourcesReady {
		t.Fatalf("Run = (%v, %v), want readiness under a host-past deadline", operation, err)
	}
	if fx.exec.callCount() != len(fx.identity.Resources()) {
		t.Fatalf("executor calls = %d, want full sequence", fx.exec.callCount())
	}
}
