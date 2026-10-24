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
// Fixtures. Everything here is self-contained and actRunner-prefixed so this
// file owns no shared helper and depends on no other test file.
// ---------------------------------------------------------------------------

const (
	actRunnerOpID       = "11111111-1111-4111-8111-111111111111"
	actRunnerOwner      = "33333333-3333-4333-8333-333333333333"
	actRunnerOwnerTwo   = "44444444-4444-4444-8444-444444444444"
	actRunnerAttemptTwo = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	actRunnerOtherOp    = "77777777-7777-4777-8777-777777777777"
)

// actRunnerLeakMarker stands in for private executor/remote diagnostics a raw
// error could carry. It is composed so no fixture resembles a credential.
var actRunnerLeakMarker = strings.Join([]string{"raw", "remote", "output", "hunter2", "marker"}, "-")

// actRunnerKinds is the canonical activation layout order.
var actRunnerKinds = []string{
	domain.ProbeActivationProbeInit,
	domain.ProbeActivationRegistration,
	domain.ProbeActivationServiceStart,
	domain.ProbeActivationEnrollment,
}

// actRunnerBaseTime is microsecond-precision UTC like the stored stamps.
func actRunnerBaseTime() time.Time {
	return time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
}

func actRunnerFingerprint(fill byte) string {
	value := make([]byte, 32)
	for i := range value {
		value[i] = fill
	}
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(value)
}

func actRunnerIdentity(operationID string) domain.ProbeActivationIdentity {
	return domain.ProbeActivationIdentity{
		Install: domain.ProbeInstallIdentity{
			OperationID:          operationID,
			Host:                 domain.ProbeSSHHost{Address: "edge.example:22", User: "root"},
			HostKeyFingerprint:   actRunnerFingerprint(0x01),
			ClientKeyFingerprint: actRunnerFingerprint(0x02),
			RequestedBy:          7,
			LayoutVersion:        domain.ProbeInstallLayoutVersion,
			Mode:                 domain.ProbeInstallSystemd,
			Binary:               domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 100},
		},
		ProbeKey:             "edge-1",
		Name:                 "Edge One",
		Location:             "Bangkok",
		ListenPort:           8443,
		ClientKeyFingerprint: actRunnerFingerprint(0x02),
		RequestedBy:          7,
	}
}

// actRunnerFacts are the public probe_init facts a healthy executor reports.
func actRunnerFacts() domain.ProbeActivationFacts {
	return domain.ProbeActivationFacts{
		ProbeID:                "55555555-5555-4555-8555-555555555555",
		StreamID:               "66666666-6666-4666-8666-666666666666",
		CertificateFingerprint: strings.Repeat("c", 64),
	}
}

// actRunnerOtherFacts is a second valid fact set, distinct from the first.
func actRunnerOtherFacts() domain.ProbeActivationFacts {
	return domain.ProbeActivationFacts{
		ProbeID:                "88888888-8888-4888-8888-888888888888",
		StreamID:               "99999999-9999-4999-8999-999999999999",
		CertificateFingerprint: strings.Repeat("d", 64),
	}
}

// actRunnerAttempts builds one canonical unique attempt UUID per step.
func actRunnerAttempts(count int) []string {
	out := make([]string, count)
	for i := range out {
		out[i] = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa%04x", i+1)
	}
	return out
}

func actRunnerKindIndex(kind string) int {
	for i, k := range actRunnerKinds {
		if k == kind {
			return i
		}
	}
	return -1
}

func actRunnerApplied(kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
	facts := domain.ProbeActivationFacts{}
	if kind == domain.ProbeActivationProbeInit {
		facts = actRunnerFacts()
	}
	return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, facts, nil
}

func actRunnerNotStarted() domain.ProbeInstallResourceOutcome {
	return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
}

// ---------------------------------------------------------------------------
// In-memory activation journal fake. It mirrors the production store contract
// (state machine, exact-retry rules, atomic init facts, expiry reconciliation
// on the fake DB clock) and validates every receipt it produces. No I/O.
//
// Recording the last step applied makes the activation active, also from
// cancel_requested, because the port contract says the last applied step
// reaches active and a cancel_requested receipt with every step applied is
// not a valid graph.
// ---------------------------------------------------------------------------

type actRunnerStep struct {
	spec     domain.ProbeInstallResourceSpec
	state    string
	attempt  string
	code     string
	started  time.Time
	finished time.Time
}

type actRunnerState struct {
	identity domain.ProbeActivationIdentity
	owner    string
	state    string
	code     string
	facts    domain.ProbeActivationFacts
	created  time.Time
	updated  time.Time
	deadline time.Time
	steps    []actRunnerStep
}

type actRunnerCall struct {
	method   string
	kind     string
	attempt  string
	outcome  string
	facts    domain.ProbeActivationFacts
	created  bool
	deadline time.Duration
	canceled bool
}

type actRunnerFake struct {
	mu     sync.Mutex
	op     *actRunnerState
	dbNow  time.Time
	taken  map[string]bool
	calls  []actRunnerCall
	counts struct{ create, observe, begin, cancel, finish, reserve, record, grants int }

	observeFailAt map[int]error // fail the Nth ObserveActivation call
	observeErr    error         // fail every ObserveActivation call while set
	failBefore    map[string]error
	failAfter     map[string]error
	onCommit      map[string]func(ctx context.Context)
	// mangle rewrites a returned receipt after the commit, modelling a stale,
	// corrupt or forged acknowledgement.
	mangle map[string]func(*domain.ProbeActivation) *domain.ProbeActivation
}

func newActRunnerFake() *actRunnerFake {
	return &actRunnerFake{
		dbNow:         actRunnerBaseTime(),
		taken:         map[string]bool{},
		observeFailAt: map[int]error{},
		failBefore:    map[string]error{},
		failAfter:     map[string]error{},
		onCommit:      map[string]func(ctx context.Context){},
		mangle:        map[string]func(*domain.ProbeActivation) *domain.ProbeActivation{},
	}
}

func (f *actRunnerFake) advance() time.Time {
	f.dbNow = f.dbNow.Add(time.Second)
	return f.dbNow
}

func (f *actRunnerFake) receipt() *domain.ProbeActivation {
	activation := &domain.ProbeActivation{
		Identity:   f.op.identity,
		OwnerID:    f.op.owner,
		State:      f.op.state,
		ErrorCode:  f.op.code,
		Facts:      f.op.facts,
		CreatedAt:  f.op.created,
		UpdatedAt:  f.op.updated,
		DeadlineAt: f.op.deadline,
	}
	activation.Steps = make([]domain.ProbeInstallResource, len(f.op.steps))
	for i, step := range f.op.steps {
		activation.Steps[i] = domain.ProbeInstallResource{
			Spec:       step.spec,
			State:      step.state,
			AttemptID:  step.attempt,
			ErrorCode:  step.code,
			StartedAt:  step.started,
			FinishedAt: step.finished,
		}
	}
	if !activation.Valid() {
		panic("activation runner fake produced an invalid receipt")
	}
	return activation
}

// out applies the optional forged-acknowledgement rewrite for one method.
func (f *actRunnerFake) out(method string, activation *domain.ProbeActivation) *domain.ProbeActivation {
	if rewrite := f.mangle[method]; rewrite != nil {
		return rewrite(activation)
	}
	return activation
}

// actRunnerClone is a deep copy tests use before rewriting a receipt.
func actRunnerClone(a *domain.ProbeActivation) *domain.ProbeActivation {
	c := *a
	c.Steps = append([]domain.ProbeInstallResource(nil), a.Steps...)
	return &c
}

// reconcile mirrors the store's expiry reconciliation on the fake DB clock.
func (f *actRunnerFake) reconcile() {
	if f.op == nil || actRunnerTerminal(f.op.state) || f.dbNow.Before(f.op.deadline) {
		return
	}
	for i := range f.op.steps {
		if f.op.steps[i].state != domain.ProbeInstallResourceInFlight {
			continue
		}
		finished := f.advance()
		f.op.steps[i].state = domain.ProbeInstallResourceUnknown
		f.op.steps[i].code = "deadline_expired"
		f.op.steps[i].finished = finished
	}
	f.op.state = domain.ProbeActivationInterrupted
	f.op.code = "deadline_expired"
	f.op.updated = f.advance()
}

func actRunnerTerminal(state string) bool {
	return state == domain.ProbeActivationActive || state == domain.ProbeActivationFailed ||
		state == domain.ProbeActivationCanceled || state == domain.ProbeActivationInterrupted
}

func actRunnerJoined(state string) bool {
	return state == domain.ProbeActivationRunning || state == domain.ProbeActivationCancelRequested
}

func (f *actRunnerFake) log(ctx context.Context, method, kind, attempt, outcome string, facts domain.ProbeActivationFacts) {
	entry := actRunnerCall{method: method, kind: kind, attempt: attempt, outcome: outcome, facts: facts, canceled: ctx.Err() != nil}
	if deadline, ok := ctx.Deadline(); ok {
		entry.deadline = time.Until(deadline)
	}
	f.calls = append(f.calls, entry)
}

func (f *actRunnerFake) index(kind string) (int, bool) {
	for i := range f.op.steps {
		if f.op.steps[i].spec.Kind == kind {
			return i, true
		}
	}
	return 0, false
}

func (f *actRunnerFake) CreateActivation(ctx context.Context, identity domain.ProbeActivationIdentity, ownerID string) (*domain.ProbeActivation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(ctx, "create", "", "", "", domain.ProbeActivationFacts{})
	f.counts.create++
	if f.op != nil {
		if f.op.identity != identity {
			return nil, false, ports.ErrConflict
		}
		return f.receipt(), false, nil
	}
	now := f.advance()
	f.op = &actRunnerState{
		identity: identity,
		owner:    ownerID,
		state:    domain.ProbeActivationQueued,
		created:  now,
		updated:  now,
		deadline: now.Add(domain.ProbeActivationLifetime),
	}
	for _, spec := range identity.Steps() {
		f.op.steps = append(f.op.steps, actRunnerStep{spec: spec, state: domain.ProbeInstallResourcePlanned})
	}
	return f.receipt(), true, nil
}

func (f *actRunnerFake) ObserveActivation(ctx context.Context, operationID string) (*domain.ProbeActivation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "observe", "", "", "", domain.ProbeActivationFacts{})
	f.counts.observe++
	if err := f.observeFailAt[f.counts.observe]; err != nil {
		return nil, err
	}
	if f.observeErr != nil {
		return nil, f.observeErr
	}
	if f.op == nil || f.op.identity.Install.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	out := f.receipt()
	hook = f.onCommit["observe"]
	return f.out("observe", out), nil
}

func (f *actRunnerFake) BeginActivation(ctx context.Context, operationID, ownerID string) (*domain.ProbeActivation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "begin", "", "", "", domain.ProbeActivationFacts{})
	f.counts.begin++
	if f.op == nil || f.op.identity.Install.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	if current.OwnerID != ownerID || current.State != domain.ProbeActivationQueued || !f.dbNow.Before(current.DeadlineAt) {
		return nil, ports.ErrConflict
	}
	now := f.advance()
	f.op.state = domain.ProbeActivationRunning
	f.op.code = ""
	f.op.updated = now
	f.counts.grants++
	out := f.receipt()
	hook = f.onCommit["begin"]
	if err := f.failAfter["begin"]; err != nil {
		return nil, err
	}
	return f.out("begin", out), nil
}

func (f *actRunnerFake) RequestActivationCancellation(ctx context.Context, operationID string) (*domain.ProbeActivation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "cancel", "", "", "", domain.ProbeActivationFacts{})
	f.counts.cancel++
	if f.op == nil || f.op.identity.Install.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	if current.Terminal() || current.State == domain.ProbeActivationCancelRequested {
		return current, nil
	}
	now := f.advance()
	state, code := domain.ProbeActivationCancelRequested, ""
	if current.State == domain.ProbeActivationQueued {
		state, code = domain.ProbeActivationCanceled, "canceled"
	}
	f.op.state = state
	f.op.code = code
	f.op.updated = now
	out := f.receipt()
	hook = f.onCommit["cancel"]
	return f.out("cancel", out), nil
}

func (f *actRunnerFake) FinishActivation(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeActivation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "finish", "", "", outcome.State+":"+outcome.ErrorCode, domain.ProbeActivationFacts{})
	f.counts.finish++
	if err := f.failBefore["finish"]; err != nil {
		return nil, err
	}
	if f.op == nil || f.op.identity.Install.OperationID != operationID {
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
	if current.OwnerID != ownerID || !actRunnerJoined(current.State) || !f.dbNow.Before(current.DeadlineAt) {
		return nil, ports.ErrConflict
	}
	if outcome.State == domain.ProbeInstallCanceled && current.State != domain.ProbeActivationCancelRequested {
		return nil, ports.ErrConflict
	}
	switch outcome.State {
	case domain.ProbeInstallFailed, domain.ProbeInstallCanceled:
		for _, step := range f.op.steps {
			if step.state == domain.ProbeInstallResourceInFlight || step.state == domain.ProbeInstallResourceUnknown {
				return nil, ports.ErrConflict
			}
		}
	case domain.ProbeInstallInterrupted:
		for i := range f.op.steps {
			if f.op.steps[i].state != domain.ProbeInstallResourceInFlight {
				continue
			}
			finished := f.advance()
			f.op.steps[i].state = domain.ProbeInstallResourceUnknown
			f.op.steps[i].code = outcome.ErrorCode
			f.op.steps[i].finished = finished
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
	return f.out("finish", out), nil
}

func (f *actRunnerFake) ReserveActivationStep(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeActivation, bool, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "reserve", kind, attemptID, "", domain.ProbeActivationFacts{})
	f.counts.reserve++
	if f.op == nil || f.op.identity.Install.OperationID != operationID {
		return nil, false, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	index, ok := f.index(kind)
	if !ok {
		return nil, false, ports.ErrNotFound
	}
	step := f.op.steps[index]
	if step.state != domain.ProbeInstallResourcePlanned {
		if current.OwnerID == ownerID && step.attempt == attemptID {
			hook = f.onCommit["reserve"]
			return f.out("reserve", current), false, nil
		}
		return nil, false, ports.ErrConflict
	}
	if current.OwnerID != ownerID || current.State != domain.ProbeActivationRunning || !f.dbNow.Before(current.DeadlineAt) {
		return nil, false, ports.ErrConflict
	}
	for i := 0; i < index; i++ {
		if f.op.steps[i].state != domain.ProbeInstallResourceApplied {
			return nil, false, ports.ErrConflict
		}
	}
	if f.taken[attemptID] {
		return nil, false, ports.ErrConflict
	}
	now := f.advance()
	f.op.steps[index] = actRunnerStep{
		spec:    step.spec,
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
	return f.out("reserve", out), true, nil
}

func (f *actRunnerFake) RecordActivationStep(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome, facts domain.ProbeActivationFacts) (*domain.ProbeActivation, error) {
	f.mu.Lock()
	var hook func(context.Context)
	defer func() {
		f.mu.Unlock()
		if hook != nil {
			hook(ctx)
		}
	}()
	f.log(ctx, "record", kind, attemptID, outcome.State+":"+outcome.ErrorCode, facts)
	f.counts.record++
	if f.op == nil || f.op.identity.Install.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	f.reconcile()
	current := f.receipt()
	index, ok := f.index(kind)
	if !ok {
		return nil, ports.ErrNotFound
	}
	step := f.op.steps[index]
	if current.OwnerID != ownerID {
		return nil, ports.ErrConflict
	}
	if step.state == domain.ProbeInstallResourcePlanned || step.attempt != attemptID {
		return nil, ports.ErrConflict
	}
	if step.state != domain.ProbeInstallResourceInFlight {
		if step.state == outcome.State && step.code == outcome.ErrorCode {
			return f.out("record", current), nil
		}
		return nil, ports.ErrConflict
	}
	if !actRunnerJoined(current.State) || !f.dbNow.Before(current.DeadlineAt) {
		return nil, ports.ErrConflict
	}
	// Store contract: valid facts exactly with probe_init applied, else zero.
	withFacts := kind == domain.ProbeActivationProbeInit && outcome.State == domain.ProbeInstallResourceApplied
	if withFacts != facts.Valid() || (!withFacts && facts != (domain.ProbeActivationFacts{})) {
		return nil, domain.ErrValidation
	}
	now := f.advance()
	f.op.steps[index].state = outcome.State
	f.op.steps[index].code = outcome.ErrorCode
	if outcome.State == domain.ProbeInstallResourceApplied {
		f.op.steps[index].code = ""
	}
	f.op.steps[index].finished = now
	if withFacts {
		f.op.facts = facts
	}
	opState, opCode := current.State, ""
	switch outcome.State {
	case domain.ProbeInstallResourceApplied:
		allApplied := true
		for _, s := range f.op.steps {
			if s.state != domain.ProbeInstallResourceApplied {
				allApplied = false
				break
			}
		}
		if allApplied {
			opState = domain.ProbeActivationActive
		}
	case domain.ProbeInstallResourceNotStarted:
		// The activation keeps running so the owner can finish it honestly.
	case domain.ProbeInstallResourceUnknown:
		opState = domain.ProbeActivationInterrupted
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
	return f.out("record", out), nil
}

// Test-side mutation helpers (they model concurrent operators/corruption).

func (f *actRunnerFake) setAttempt(kind, attempt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index, ok := f.index(kind); ok {
		f.op.steps[index].attempt = attempt
	}
}

func (f *actRunnerFake) setOwner(owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.op.owner = owner
}

func (f *actRunnerFake) setProbeKey(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.op.identity.ProbeKey = key
}

// stageStep commits a step receipt without a port call, modelling a
// concurrent/earlier writer on another replica.
func (f *actRunnerFake) stageStep(kind, state, attempt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index, ok := f.index(kind)
	if !ok {
		return
	}
	now := f.advance()
	f.op.steps[index].state = state
	f.op.steps[index].attempt = attempt
	f.op.steps[index].code = ""
	f.op.steps[index].started = now
	if state == domain.ProbeInstallResourceInFlight {
		f.op.steps[index].finished = time.Time{}
	} else {
		f.op.steps[index].finished = now
	}
	if kind == domain.ProbeActivationProbeInit && state == domain.ProbeInstallResourceApplied {
		f.op.facts = actRunnerFacts()
	}
	f.op.updated = now
	f.taken[attempt] = true
}

// stageCancelIntent records durable cancellation intent without a port call.
func (f *actRunnerFake) stageCancelIntent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.op == nil || actRunnerTerminal(f.op.state) {
		return
	}
	now := f.advance()
	f.op.state = domain.ProbeActivationCancelRequested
	f.op.code = ""
	f.op.updated = now
}

func (f *actRunnerFake) expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbNow = f.op.deadline.Add(time.Second)
}

func (f *actRunnerFake) forceDeadlineExpired() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbNow = f.op.deadline.Add(time.Second)
	f.reconcile()
}

func (f *actRunnerFake) setObserveErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observeErr = err
}

func (f *actRunnerFake) stateNow() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.op.state
}

func (f *actRunnerFake) observeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts.observe
}

func (f *actRunnerFake) beginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts.begin
}

func (f *actRunnerFake) snapshot() *domain.ProbeActivation {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconcile()
	return f.receipt()
}

func (f *actRunnerFake) countsSnapshot() (reserve, record, finish, cancel int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts.reserve, f.counts.record, f.counts.finish, f.counts.cancel
}

func (f *actRunnerFake) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *actRunnerFake) callLog() []actRunnerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]actRunnerCall(nil), f.calls...)
}

// ---------------------------------------------------------------------------
// Scripted executor. The default report is a definite joined applied outcome
// (with the public facts for probe_init).
// ---------------------------------------------------------------------------

type actRunnerExecutor struct {
	mu    sync.Mutex
	calls []string
	apply func(ctx context.Context, activation domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error)
}

func (e *actRunnerExecutor) ApplyStep(ctx context.Context, activation domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
	e.mu.Lock()
	e.calls = append(e.calls, kind)
	hook := e.apply
	e.mu.Unlock()
	if hook == nil {
		return actRunnerApplied(kind)
	}
	return hook(ctx, activation, kind)
}

func (e *actRunnerExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *actRunnerExecutor) callSnapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// ---------------------------------------------------------------------------
// Fixture wiring: the fake ports under the real runner.
// ---------------------------------------------------------------------------

type actRunnerFixture struct {
	runner   *ProbeActivationRunner
	fake     *actRunnerFake
	exec     *actRunnerExecutor
	identity domain.ProbeActivationIdentity
	owner    string
	attempts []string
}

func actRunnerSetupBase(t *testing.T, base time.Time) *actRunnerFixture {
	t.Helper()
	fake := newActRunnerFake()
	fake.dbNow = base.Add(-time.Second)
	exec := &actRunnerExecutor{}
	runner, err := NewProbeActivationRunner(fake, fake, exec)
	if err != nil || runner == nil {
		t.Fatalf("runner constructor failed: %v", err)
	}
	identity := actRunnerIdentity(actRunnerOpID)
	if !identity.Valid() {
		t.Fatalf("fixture identity is not valid")
	}
	activation, created, err := fake.CreateActivation(context.Background(), identity, actRunnerOwner)
	if err != nil || !created || activation == nil || activation.State != domain.ProbeActivationQueued {
		t.Fatalf("fixture admission failed: created=%v err=%v", created, err)
	}
	return &actRunnerFixture{
		runner:   runner,
		fake:     fake,
		exec:     exec,
		identity: identity,
		owner:    actRunnerOwner,
		attempts: actRunnerAttempts(len(identity.Steps())),
	}
}

func actRunnerSetup(t *testing.T) *actRunnerFixture {
	t.Helper()
	return actRunnerSetupBase(t, actRunnerBaseTime())
}

func (fx *actRunnerFixture) run() (*domain.ProbeActivation, error) {
	return fx.runner.Run(context.Background(), fx.identity, fx.owner, fx.attempts)
}

// actRunnerAssertUnknown checks the shared outcome of an untrusted executor
// report for the step of the given kind: unknown, interrupted, zero facts
// recorded, and nothing dispatched or finished afterwards.
func actRunnerAssertUnknown(t *testing.T, fx *actRunnerFixture, activation *domain.ProbeActivation, err error, kind string) {
	t.Helper()
	index := actRunnerKindIndex(kind)
	if !errors.Is(err, domain.ErrInternal) || activation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, internal)", activation, err)
	}
	if strings.Contains(fmt.Sprint(err), actRunnerLeakMarker) {
		t.Fatalf("executor diagnostics leaked: %v", err)
	}
	if activation.State != domain.ProbeActivationInterrupted || activation.ErrorCode != "outcome_unknown" {
		t.Fatalf("terminal = %s/%s, want interrupted/outcome_unknown", activation.State, activation.ErrorCode)
	}
	stored := fx.fake.snapshot()
	if !stored.Valid() {
		t.Fatalf("stored graph invalid: %+v", stored)
	}
	if stored.Steps[index].State != domain.ProbeInstallResourceUnknown || stored.Steps[index].ErrorCode != "outcome_unknown" {
		t.Fatalf("uncertainty not preserved: %+v", stored.Steps[index])
	}
	wantFacts := domain.ProbeActivationFacts{}
	if index > 0 {
		wantFacts = actRunnerFacts()
	}
	if stored.Facts != wantFacts {
		t.Fatalf("stored facts = %+v, want %+v", stored.Facts, wantFacts)
	}
	nonZero := 0
	for _, call := range fx.fake.callLog() {
		if call.method != "record" {
			continue
		}
		if call.facts != (domain.ProbeActivationFacts{}) {
			nonZero++
			if call.kind != domain.ProbeActivationProbeInit || call.outcome != "applied:" {
				t.Fatalf("facts recorded outside an applied probe_init: %+v", call)
			}
		}
		if call.kind == kind && (call.outcome != "unknown:outcome_unknown" || call.facts != (domain.ProbeActivationFacts{})) {
			t.Fatalf("untrusted report recorded as %q with facts %+v", call.outcome, call.facts)
		}
	}
	wantNonZero := 0
	if index > 0 {
		wantNonZero = 1
	}
	if nonZero != wantNonZero {
		t.Fatalf("records carrying facts = %d, want %d", nonZero, wantNonZero)
	}
	// No dependent dispatch, no finish over the atomic interruption.
	if fx.exec.callCount() != index+1 {
		t.Fatalf("executor calls = %d, want %d (dependents dispatched?)", fx.exec.callCount(), index+1)
	}
	if reserve, _, finish, _ := fx.fake.countsSnapshot(); reserve != index+1 || finish != 0 {
		t.Fatalf("reserve=%d finish=%d, want %d/0", reserve, finish, index+1)
	}
}

// ---------------------------------------------------------------------------
// Construction and input validation
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerConstructionAndFixedDefaults(t *testing.T) {
	fake := newActRunnerFake()
	exec := &actRunnerExecutor{}
	if _, err := NewProbeActivationRunner(nil, fake, exec); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil repo accepted: %v", err)
	}
	if _, err := NewProbeActivationRunner(fake, nil, exec); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil steps accepted: %v", err)
	}
	if _, err := NewProbeActivationRunner(fake, fake, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil executor accepted: %v", err)
	}
	fx := actRunnerSetup(t)
	if fx.runner.pollInterval != time.Second {
		t.Fatalf("production poll interval is not fixed at 1s: %v", fx.runner.pollInterval)
	}
	if fx.runner.runBound != domain.ProbeActivationLifetime {
		t.Fatalf("production run bound is not fixed at %v: %v", domain.ProbeActivationLifetime, fx.runner.runBound)
	}
	var nilRunner *ProbeActivationRunner
	if got, err := nilRunner.Run(context.Background(), fx.identity, fx.owner, fx.attempts); !errors.Is(err, domain.ErrInternal) || got != nil {
		t.Fatalf("nil runner Run = (%v, %v), want (nil, internal)", got, err)
	}
}

func TestProbeActivationRunnerRejectsInvalidInputsBeforeIO(t *testing.T) {
	fx := actRunnerSetup(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	badPort := fx.identity
	badPort.ListenPort = 80

	cases := []struct {
		name     string
		ctx      context.Context
		identity domain.ProbeActivationIdentity
		owner    string
		attempts []string
		want     error
	}{
		{"nil context", nil, fx.identity, fx.owner, fx.attempts, domain.ErrValidation},
		{"canceled context", canceled, fx.identity, fx.owner, fx.attempts, context.Canceled},
		{"zero identity", context.Background(), domain.ProbeActivationIdentity{}, fx.owner, fx.attempts, domain.ErrValidation},
		{"invalid identity", context.Background(), badPort, fx.owner, fx.attempts, domain.ErrValidation},
		{"invalid owner", context.Background(), fx.identity, "not-a-uuid", fx.attempts, domain.ErrValidation},
		{"nil attempts", context.Background(), fx.identity, fx.owner, nil, domain.ErrValidation},
		{"too few attempts", context.Background(), fx.identity, fx.owner, fx.attempts[:1], domain.ErrValidation},
		{"too many attempts", context.Background(), fx.identity, fx.owner,
			append(append([]string(nil), fx.attempts...), actRunnerAttemptTwo), domain.ErrValidation},
		{"duplicate attempts", context.Background(), fx.identity, fx.owner,
			[]string{fx.attempts[0], fx.attempts[0], fx.attempts[2], fx.attempts[3]}, domain.ErrValidation},
		{"non canonical attempt", context.Background(), fx.identity, fx.owner,
			[]string{"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", fx.attempts[1], fx.attempts[2], fx.attempts[3]}, domain.ErrValidation},
		{"malformed attempt", context.Background(), fx.identity, fx.owner,
			[]string{fx.attempts[0], "not-a-uuid", fx.attempts[2], fx.attempts[3]}, domain.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := fx.fake.totalCalls()
			activation, err := fx.runner.Run(tc.ctx, tc.identity, tc.owner, tc.attempts)
			if !errors.Is(err, tc.want) || activation != nil {
				t.Fatalf("Run = (%v, %v), want (nil, %v)", activation, err, tc.want)
			}
			if after := fx.fake.totalCalls(); after != before {
				t.Fatalf("invalid input performed %d journal calls", after-before)
			}
			if fx.exec.callCount() != 0 {
				t.Fatalf("invalid input dispatched the executor")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The observe gate and the single Begin
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerObserveGateBeforeBegin(t *testing.T) {
	t.Run("foreign identity never begins", func(t *testing.T) {
		fx := actRunnerSetup(t)
		other := fx.identity
		other.ProbeKey = "intruder"
		if !other.Valid() {
			t.Fatalf("foreign identity fixture must itself be valid")
		}
		activation, err := fx.runner.Run(context.Background(), other, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || activation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", activation, err)
		}
		for _, call := range fx.fake.callLog() {
			if call.method == "begin" || call.method == "reserve" || call.method == "record" || call.method == "finish" {
				t.Fatalf("foreign identity triggered %s", call.method)
			}
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("foreign identity dispatched the executor")
		}
	})

	t.Run("foreign owner never begins", func(t *testing.T) {
		fx := actRunnerSetup(t)
		activation, err := fx.runner.Run(context.Background(), fx.identity, actRunnerOwnerTwo, fx.attempts)
		if !errors.Is(err, ports.ErrConflict) || activation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", activation, err)
		}
		for _, call := range fx.fake.callLog() {
			if call.method != "create" && call.method != "observe" {
				t.Fatalf("foreign owner triggered %s", call.method)
			}
		}
	})

	t.Run("non queued state never begins twice", func(t *testing.T) {
		fx := actRunnerSetup(t)
		if _, err := fx.fake.BeginActivation(context.Background(), actRunnerOpID, fx.owner); err != nil {
			t.Fatalf("seed begin failed: %v", err)
		}
		beginsBefore := fx.fake.beginCount()
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", activation, err)
		}
		if fx.fake.beginCount() != beginsBefore {
			t.Fatalf("runner attempted a second begin")
		}
	})

	t.Run("terminal state never begins", func(t *testing.T) {
		fx := actRunnerSetup(t)
		if _, err := fx.fake.RequestActivationCancellation(context.Background(), actRunnerOpID); err != nil {
			t.Fatalf("seed cancel failed: %v", err)
		}
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", activation, err)
		}
		if fx.fake.beginCount() != 0 {
			t.Fatalf("runner began over a terminal state")
		}
	})

	t.Run("unknown activation is not found", func(t *testing.T) {
		fx := actRunnerSetup(t)
		other := actRunnerIdentity(actRunnerOtherOp)
		activation, err := fx.runner.Run(context.Background(), other, fx.owner, fx.attempts)
		if !errors.Is(err, ports.ErrNotFound) || activation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, not found)", activation, err)
		}
	})

	t.Run("corrupt observation never leaks diagnostics", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.fake.observeFailAt[1] = errors.New(actRunnerLeakMarker)
		activation, err := fx.run()
		if !errors.Is(err, domain.ErrInternal) || activation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, internal)", activation, err)
		}
		if strings.Contains(err.Error(), actRunnerLeakMarker) {
			t.Fatalf("storage diagnostics leaked: %v", err)
		}
	})
}

func TestProbeActivationRunnerBeginLossNeverDispatchesFinishesOrInterrupts(t *testing.T) {
	fx := actRunnerSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The begin commits, then the acknowledgement is lost: the post-commit
	// context check discards the receipt exactly as in production.
	fx.fake.onCommit["begin"] = func(context.Context) { cancel() }

	activation, err := fx.runner.Run(ctx, fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, context.Canceled) || activation != nil {
		t.Fatalf("Run = (%v, %v), want (nil, canceled)", activation, err)
	}
	reserve, record, finish, cancels := fx.fake.countsSnapshot()
	if reserve+record+finish != 0 || cancels != 0 {
		t.Fatalf("lost begin ack wrote: reserve=%d record=%d finish=%d cancel=%d", reserve, record, finish, cancels)
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("lost begin ack dispatched the executor")
	}
	if stored := fx.fake.snapshot(); stored.State != domain.ProbeActivationRunning {
		t.Fatalf("committed begin not preserved: %s", stored.State)
	}

	// Cold restart: the winning run is never dispatched over nor finalized.
	activation, err = fx.run()
	if !errors.Is(err, ports.ErrConflict) || activation != nil {
		t.Fatalf("cold retry = (%v, %v), want (nil, conflict)", activation, err)
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("cold retry dispatched the executor")
	}
	reserve, record, finish, _ = fx.fake.countsSnapshot()
	if reserve+record+finish != 0 {
		t.Fatalf("cold retry wrote history")
	}
}

func TestProbeActivationRunnerCompetingRunsSingleBeginGrant(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = 2 * time.Millisecond

	const competitors = 2
	type result struct {
		activation *domain.ProbeActivation
		err        error
	}
	results := make(chan result, competitors)
	var ready sync.WaitGroup
	ready.Add(competitors)
	for i := 0; i < competitors; i++ {
		go func() {
			ready.Done()
			activation, err := fx.run()
			results <- result{activation, err}
		}()
	}
	ready.Wait()
	var wins, losses int
	for i := 0; i < competitors; i++ {
		got := <-results
		switch {
		case got.err == nil && got.activation != nil && got.activation.State == domain.ProbeActivationActive:
			wins++
		case errors.Is(got.err, ports.ErrConflict) && got.activation == nil:
			losses++
		default:
			t.Fatalf("unexpected competitor result: (%v, %v)", got.activation, got.err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins=%d losses=%d, want exactly one of each", wins, losses)
	}
	if fx.fake.counts.grants != 1 {
		t.Fatalf("begin grants = %d, want exactly 1", fx.fake.counts.grants)
	}
	if got := fx.exec.callCount(); got != len(fx.identity.Steps()) {
		t.Fatalf("executor calls = %d, want %d (one full sequence)", got, len(fx.identity.Steps()))
	}
	if _, _, _, cancels := fx.fake.countsSnapshot(); cancels != 0 {
		t.Fatalf("runner invented cancellation intent %d times", cancels)
	}
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerReachesActiveWithFactsRecordedOnce(t *testing.T) {
	fx := actRunnerSetup(t)
	activation, err := fx.run()
	if err != nil || activation == nil {
		t.Fatalf("Run = (%v, %v), want active", activation, err)
	}
	if activation.State != domain.ProbeActivationActive || activation.ErrorCode != "" || !activation.Valid() || !activation.Terminal() {
		t.Fatalf("bad active receipt: state=%s code=%s valid=%v", activation.State, activation.ErrorCode, activation.Valid())
	}
	specs := fx.identity.Steps()
	if fx.exec.callCount() != len(specs) {
		t.Fatalf("executor calls = %d, want %d", fx.exec.callCount(), len(specs))
	}
	for i, spec := range specs {
		if got := activation.Steps[i].Spec; got != spec {
			t.Fatalf("step %d spec = %+v, want %+v", i, got, spec)
		}
		if activation.Steps[i].State != domain.ProbeInstallResourceApplied || activation.Steps[i].AttemptID != fx.attempts[i] {
			t.Fatalf("step %d not bound to its attempt: %+v", i, activation.Steps[i])
		}
	}
	if activation.Facts != actRunnerFacts() {
		t.Fatalf("facts = %+v, want %+v", activation.Facts, actRunnerFacts())
	}
	if stored := fx.fake.snapshot(); stored.State != domain.ProbeActivationActive || stored.Facts != actRunnerFacts() {
		t.Fatalf("stored activation = %s facts=%+v", stored.State, stored.Facts)
	}

	// Facts travel with exactly one record: the applied probe_init.
	log := fx.fake.callLog()
	recordAt, reserveAt := map[string]int{}, map[string]int{}
	factRecords := 0
	for i, call := range log {
		switch call.method {
		case "record":
			recordAt[call.kind] = i
			if call.facts != (domain.ProbeActivationFacts{}) {
				factRecords++
				if call.kind != domain.ProbeActivationProbeInit || call.outcome != "applied:" || call.facts != actRunnerFacts() {
					t.Fatalf("facts recorded with the wrong report: %+v", call)
				}
			}
		case "reserve":
			reserveAt[call.kind] = i
		}
	}
	if factRecords != 1 {
		t.Fatalf("records carrying facts = %d, want exactly 1", factRecords)
	}
	// Dependent work only after the predecessor's record commit.
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
	reserve, record, finish, cancels := fx.fake.countsSnapshot()
	if reserve != len(specs) || record != len(specs) || finish != 0 || cancels != 0 {
		t.Fatalf("reserve=%d record=%d finish=%d cancel=%d, want %d/%d/0/0", reserve, record, finish, cancels, len(specs), len(specs))
	}
}

func TestProbeActivationRunnerExecutorSeesTheReservedStep(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = time.Hour
	fx.exec.apply = func(_ context.Context, activation domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		index := actRunnerKindIndex(kind)
		if activation.State != domain.ProbeActivationRunning || activation.OwnerID != fx.owner || activation.Identity != fx.identity {
			t.Errorf("%s: executor got %s owner=%s", kind, activation.State, activation.OwnerID)
		}
		step := activation.Steps[index]
		if step.State != domain.ProbeInstallResourceInFlight || step.AttemptID != fx.attempts[index] {
			t.Errorf("%s: executor did not get its in-flight reservation: %+v", kind, step)
		}
		for i := 0; i < index; i++ {
			if activation.Steps[i].State != domain.ProbeInstallResourceApplied {
				t.Errorf("%s: predecessor %d not applied: %+v", kind, i, activation.Steps[i])
			}
		}
		if index > 0 && activation.Facts != actRunnerFacts() {
			t.Errorf("%s: executor did not see the recorded facts: %+v", kind, activation.Facts)
		}
		return actRunnerApplied(kind)
	}
	activation, err := fx.run()
	if err != nil || activation == nil || activation.State != domain.ProbeActivationActive {
		t.Fatalf("Run = (%v, %v), want active", activation, err)
	}
}

func TestProbeActivationRunnerActiveIsNotRevokedByTheWatcher(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = time.Millisecond

	// Active is terminal, so the watcher reads it as an end state and signals.
	// That signal must never turn a completed, recorded activation into an
	// error: hold the last record's acknowledgement until the watcher has
	// really observed the active state.
	observedActive := false
	fx.fake.onCommit["record"] = func(context.Context) {
		if fx.fake.stateNow() != domain.ProbeActivationActive {
			return
		}
		before := fx.fake.observeCount()
		deadline := time.Now().Add(2 * time.Second)
		for fx.fake.observeCount() <= before && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		observedActive = fx.fake.observeCount() > before
		time.Sleep(20 * time.Millisecond)
	}

	activation, err := fx.run()
	if !observedActive {
		t.Fatalf("watcher never observed the terminal active state; the test did not exercise the race")
	}
	if err != nil || activation == nil || activation.State != domain.ProbeActivationActive {
		t.Fatalf("Run = (%v, %v), want active with nil error", activation, err)
	}
	if _, _, finish, _ := fx.fake.countsSnapshot(); finish != 0 {
		t.Fatalf("finish attempted over terminal active")
	}
}

func TestProbeActivationRunnerLateCancelIntentAtLastStepStillActive(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = time.Hour
	// Cancellation intent lands while the last step runs. The step is joined
	// applied and the journal records it as the last applied step, which
	// reaches active: the work is done and nothing is left to cancel.
	fx.exec.apply = func(_ context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		if kind == domain.ProbeActivationEnrollment {
			fx.fake.stageCancelIntent()
		}
		return actRunnerApplied(kind)
	}
	activation, err := fx.run()
	if err != nil || activation == nil || activation.State != domain.ProbeActivationActive {
		t.Fatalf("Run = (%v, %v), want active", activation, err)
	}
	if _, _, finish, _ := fx.fake.countsSnapshot(); finish != 0 {
		t.Fatalf("finish attempted over terminal active")
	}
}

// ---------------------------------------------------------------------------
// Executor report trust, including the facts contract
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerFactsMisuseRecordsUnknownWithZeroFacts(t *testing.T) {
	badFacts := actRunnerFacts()
	badFacts.CertificateFingerprint = "not-a-pin"
	localFacts := actRunnerFacts()
	localFacts.ProbeID = domain.LocalProbeID
	applied := domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}
	unknown := domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}

	cases := []struct {
		name    string
		kind    string
		outcome domain.ProbeInstallResourceOutcome
		facts   domain.ProbeActivationFacts
		err     error
	}{
		{"facts on registration", domain.ProbeActivationRegistration, applied, actRunnerFacts(), nil},
		{"facts on service_start", domain.ProbeActivationServiceStart, applied, actRunnerFacts(), nil},
		{"facts on enrollment", domain.ProbeActivationEnrollment, applied, actRunnerFacts(), nil},
		{"facts on not_started registration", domain.ProbeActivationRegistration, actRunnerNotStarted(), actRunnerFacts(), nil},
		{"invalid facts with probe_init applied", domain.ProbeActivationProbeInit, applied, badFacts, nil},
		{"local probe id with probe_init applied", domain.ProbeActivationProbeInit, applied, localFacts, nil},
		{"zero facts with probe_init applied", domain.ProbeActivationProbeInit, applied, domain.ProbeActivationFacts{}, nil},
		{"partial facts with probe_init applied", domain.ProbeActivationProbeInit, applied, domain.ProbeActivationFacts{ProbeID: actRunnerFacts().ProbeID}, nil},
		{"valid facts with probe_init not_started", domain.ProbeActivationProbeInit, actRunnerNotStarted(), actRunnerFacts(), nil},
		{"valid facts with probe_init unknown", domain.ProbeActivationProbeInit, unknown, actRunnerFacts(), nil},
		{"valid facts with probe_init error", domain.ProbeActivationProbeInit, applied, actRunnerFacts(), errors.New(actRunnerLeakMarker)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := actRunnerSetup(t)
			fx.runner.pollInterval = time.Hour
			fx.exec.apply = func(_ context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
				if kind == tc.kind {
					return tc.outcome, tc.facts, tc.err
				}
				return actRunnerApplied(kind)
			}
			activation, err := fx.run()
			actRunnerAssertUnknown(t, fx, activation, err, tc.kind)
		})
	}
}

func TestProbeActivationRunnerUntrustedReportsInterruptAndStopDependents(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		apply func() (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error)
	}{
		{"executor error at probe_init", domain.ProbeActivationProbeInit, func() (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			return domain.ProbeInstallResourceOutcome{}, domain.ProbeActivationFacts{}, errors.New(actRunnerLeakMarker)
		}},
		{"invalid outcome at probe_init", domain.ProbeActivationProbeInit, func() (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied, ErrorCode: "oops"}, actRunnerFacts(), nil
		}},
		{"empty outcome at probe_init", domain.ProbeActivationProbeInit, func() (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			return domain.ProbeInstallResourceOutcome{}, domain.ProbeActivationFacts{}, nil
		}},
		{"executor error at registration", domain.ProbeActivationRegistration, func() (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			return domain.ProbeInstallResourceOutcome{}, domain.ProbeActivationFacts{}, errors.New(actRunnerLeakMarker)
		}},
		{"executor error with applied outcome at service_start", domain.ProbeActivationServiceStart, func() (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, domain.ProbeActivationFacts{}, errors.New(actRunnerLeakMarker)
		}},
		{"executor error at enrollment keeps earlier history", domain.ProbeActivationEnrollment, func() (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			return domain.ProbeInstallResourceOutcome{}, domain.ProbeActivationFacts{}, errors.New(actRunnerLeakMarker)
		}},
		{"unauthorized outcome code at enrollment", domain.ProbeActivationEnrollment, func() (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "deadline_expired"}, domain.ProbeActivationFacts{}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := actRunnerSetup(t)
			fx.runner.pollInterval = time.Hour
			fx.exec.apply = func(_ context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
				if kind == tc.kind {
					return tc.apply()
				}
				return actRunnerApplied(kind)
			}
			activation, err := fx.run()
			actRunnerAssertUnknown(t, fx, activation, err, tc.kind)
		})
	}
}

func TestProbeActivationRunnerTrustedNotStartedClosesFailedWithoutDependents(t *testing.T) {
	for index, kind := range actRunnerKinds {
		t.Run(kind, func(t *testing.T) {
			fx := actRunnerSetup(t)
			fx.runner.pollInterval = time.Hour
			fx.exec.apply = func(_ context.Context, _ domain.ProbeActivation, k string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
				if k == kind {
					return actRunnerNotStarted(), domain.ProbeActivationFacts{}, nil
				}
				return actRunnerApplied(k)
			}
			activation, err := fx.run()
			if err != nil || activation == nil {
				t.Fatalf("Run = (%v, %v), want closed failure", activation, err)
			}
			if activation.State != domain.ProbeActivationFailed || activation.ErrorCode != "not_started" {
				t.Fatalf("terminal = %s/%s, want failed/not_started", activation.State, activation.ErrorCode)
			}
			stored := fx.fake.snapshot()
			if stored.Steps[index].State != domain.ProbeInstallResourceNotStarted {
				t.Fatalf("report lost: %+v", stored.Steps[index])
			}
			// Earlier applied effects, and the init facts, stay recorded.
			for i := 0; i < index; i++ {
				if stored.Steps[i].State != domain.ProbeInstallResourceApplied {
					t.Fatalf("earlier step %d lost: %+v", i, stored.Steps[i])
				}
			}
			wantFacts := domain.ProbeActivationFacts{}
			if index > 0 {
				wantFacts = actRunnerFacts()
			}
			if stored.Facts != wantFacts {
				t.Fatalf("stored facts = %+v, want %+v", stored.Facts, wantFacts)
			}
			if reserve, _, finish, _ := fx.fake.countsSnapshot(); reserve != index+1 || finish != 1 {
				t.Fatalf("reserve=%d finish=%d, want %d/1 (dependent reserved?)", reserve, finish, index+1)
			}
			if fx.exec.callCount() != index+1 {
				t.Fatalf("dependent dispatched after not_started: %v", fx.exec.callSnapshot())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Grants, lost acknowledgements and retries
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerHistoricalGrantNeverInvokes(t *testing.T) {
	t.Run("exact reservation retry is history", func(t *testing.T) {
		fx := actRunnerSetup(t)
		// A prior attempt committed the in-flight intent whose exact retry the
		// journal returns as history: created=false is never a grant.
		fx.fake.onCommit["begin"] = func(context.Context) {
			fx.fake.stageStep(domain.ProbeActivationProbeInit, domain.ProbeInstallResourceInFlight, fx.attempts[0])
		}
		before := fx.fake.totalCalls()
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, conflict)", activation, err)
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
		fx := actRunnerSetup(t)
		fx.fake.onCommit["begin"] = func(context.Context) {
			fx.fake.stageStep(domain.ProbeActivationProbeInit, domain.ProbeInstallResourceApplied, actRunnerAttemptTwo)
		}
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation == nil {
			t.Fatalf("Run = (%v, %v), want conflict over history", activation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("historical attempt invoked the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("historical attempt wrote history")
		}
	})

	t.Run("created false on a later step stops the sequence", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		// Another writer commits the second step's intent right after the first
		// record: the second reservation is then answered as an exact retry
		// (created=false). The first step ran, the second must not.
		var once sync.Once
		fx.fake.onCommit["record"] = func(context.Context) {
			once.Do(func() {
				fx.fake.stageStep(domain.ProbeActivationRegistration, domain.ProbeInstallResourceInFlight, fx.attempts[1])
			})
		}
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, conflict)", activation, err)
		}
		if got := fx.exec.callSnapshot(); len(got) != 1 || got[0] != domain.ProbeActivationProbeInit {
			t.Fatalf("executor calls = %v, want only probe_init", got)
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record != 1 || finish != 0 {
			t.Fatalf("record=%d finish=%d, want 1/0", record, finish)
		}
	})
}

func TestProbeActivationRunnerLostReservationAckNoInvocation(t *testing.T) {
	fx := actRunnerSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The reservation commits and then its acknowledgement is lost.
	fx.fake.onCommit["reserve"] = func(context.Context) { cancel() }

	activation, err := fx.runner.Run(ctx, fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, context.Canceled) || activation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, canceled)", activation, err)
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("lost reservation ack invoked the executor")
	}
	if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
		t.Fatalf("lost reservation ack wrote: record=%d finish=%d", record, finish)
	}
	stored := fx.fake.snapshot()
	if stored.Steps[0].State != domain.ProbeInstallResourceInFlight || stored.Steps[0].AttemptID != fx.attempts[0] {
		t.Fatalf("held intent not preserved for cold reconciliation: %+v", stored.Steps[0])
	}

	// Retry and cold restart never invoke from history.
	if _, err := fx.run(); err == nil {
		t.Fatalf("cold retry unexpectedly succeeded")
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("retry/cold restart invoked the executor")
	}
}

func TestProbeActivationRunnerLostRecordAckStopsWithoutRedispatch(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = time.Hour
	// The joined report commits and then its acknowledgement is lost.
	fx.fake.failAfter["record"] = context.Canceled

	activation, err := fx.run()
	if !errors.Is(err, context.Canceled) || activation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, canceled)", activation, err)
	}
	if reserve, record, finish, _ := fx.fake.countsSnapshot(); reserve != 1 || record != 1 || finish != 0 {
		t.Fatalf("reserve=%d record=%d finish=%d, want 1/1/0", reserve, record, finish)
	}
	if fx.exec.callCount() != 1 {
		t.Fatalf("executor calls = %d, want 1 (no redispatch)", fx.exec.callCount())
	}
	stored := fx.fake.snapshot()
	if stored.Steps[0].State != domain.ProbeInstallResourceApplied || stored.Facts != actRunnerFacts() {
		t.Fatalf("committed report and facts not preserved: %+v facts=%+v", stored.Steps[0], stored.Facts)
	}
}

func TestProbeActivationRunnerFinishFailureLeavesHonestState(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.fake.failBefore["finish"] = context.DeadlineExceeded
	fx.exec.apply = func(context.Context, domain.ProbeActivation, string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		return actRunnerNotStarted(), domain.ProbeActivationFacts{}, nil
	}
	activation, err := fx.run()
	if !errors.Is(err, context.DeadlineExceeded) || activation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, deadline)", activation, err)
	}
	stored := fx.fake.snapshot()
	if stored.Terminal() {
		t.Fatalf("terminal state fabricated locally: %s/%s", stored.State, stored.ErrorCode)
	}
	if stored.Steps[0].State != domain.ProbeInstallResourceNotStarted {
		t.Fatalf("joined report lost: %+v", stored.Steps[0])
	}
}

// ---------------------------------------------------------------------------
// Cancellation, joining and interruption
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerHeldInvocationJoinsBeforeTerminal(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = 2 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})
	canceledObserved := make(chan struct{})
	var once sync.Once
	fx.exec.apply = func(ctx context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		once.Do(func() { close(started) })
		go func() {
			<-ctx.Done()
			close(canceledObserved)
		}()
		<-release
		return actRunnerApplied(kind)
	}

	done := make(chan struct{})
	var activation *domain.ProbeActivation
	var runErr error
	go func() {
		defer close(done)
		activation, runErr = fx.run()
	}()
	<-started

	// Durable operator cancellation while the invocation is held.
	if _, err := fx.fake.RequestActivationCancellation(context.Background(), actRunnerOpID); err != nil {
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
	if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
		t.Fatalf("premature terminal writes while held: record=%d finish=%d", record, finish)
	}
	select {
	case <-done:
		t.Fatalf("Run returned before the executor joined")
	default:
	}

	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after the executor joined")
	}
	if runErr != nil || activation == nil || activation.State != domain.ProbeActivationCanceled {
		t.Fatalf("Run = (%v, %v), want terminal canceled", activation, runErr)
	}
	stored := fx.fake.snapshot()
	if stored.State != domain.ProbeActivationCanceled || stored.ErrorCode != "canceled" {
		t.Fatalf("stored terminal = %s/%s, want canceled", stored.State, stored.ErrorCode)
	}
	if stored.Steps[0].State != domain.ProbeInstallResourceApplied || stored.Facts != actRunnerFacts() {
		t.Fatalf("joined applied report and facts not preserved: %+v facts=%+v", stored.Steps[0], stored.Facts)
	}
	if reserve, _, _, _ := fx.fake.countsSnapshot(); reserve != 1 || fx.exec.callCount() != 1 {
		t.Fatalf("dependents dispatched after cancellation: reserve=%d calls=%v", reserve, fx.exec.callSnapshot())
	}

	// No watcher or executor survives the return.
	settled := fx.fake.totalCalls()
	executed := fx.exec.callCount()
	time.Sleep(20 * fx.runner.pollInterval)
	if fx.fake.totalCalls() != settled || fx.exec.callCount() != executed {
		t.Fatalf("work continued after Run returned")
	}
}

func TestProbeActivationRunnerCancelIntentDuringInvocationFinishesCanceledAfterRecord(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = time.Hour // the record, not the watcher, observes it
	fx.exec.apply = func(_ context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		fx.fake.stageCancelIntent()
		return actRunnerApplied(kind)
	}
	activation, err := fx.run()
	if err != nil || activation == nil || activation.State != domain.ProbeActivationCanceled {
		t.Fatalf("Run = (%v, %v), want terminal canceled", activation, err)
	}
	if reserve, record, finish, cancels := fx.fake.countsSnapshot(); reserve != 1 || record != 1 || finish != 1 || cancels != 0 {
		t.Fatalf("reserve=%d record=%d finish=%d cancel=%d, want 1/1/1/0", reserve, record, finish, cancels)
	}
	stored := fx.fake.snapshot()
	if stored.Steps[0].State != domain.ProbeInstallResourceApplied || stored.Facts != actRunnerFacts() {
		t.Fatalf("joined applied report and facts not preserved: %+v facts=%+v", stored.Steps[0], stored.Facts)
	}
}

func TestProbeActivationRunnerCallerCancelPreservesJoinedApplied(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var once sync.Once
	fx.exec.apply = func(callCtx context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		once.Do(func() { close(started) })
		<-callCtx.Done()
		// A definitely joined applied report racing cancellation stays applied.
		return actRunnerApplied(kind)
	}

	done := make(chan struct{})
	var activation *domain.ProbeActivation
	var runErr error
	go func() {
		defer close(done)
		activation, runErr = fx.runner.Run(ctx, fx.identity, fx.owner, fx.attempts)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after caller cancellation")
	}
	if !errors.Is(runErr, context.Canceled) || activation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, canceled)", activation, runErr)
	}
	if activation.State != domain.ProbeActivationInterrupted || activation.ErrorCode != "owner_interrupted" {
		t.Fatalf("terminal = %s/%s, want interrupted/owner_interrupted", activation.State, activation.ErrorCode)
	}
	stored := fx.fake.snapshot()
	if stored.Steps[0].State != domain.ProbeInstallResourceApplied || stored.Facts != actRunnerFacts() {
		t.Fatalf("definite applied report lost to cancellation: %+v facts=%+v", stored.Steps[0], stored.Facts)
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
		if call.deadline <= 0 || call.deadline > probeActivationRunnerMetadataTimeout+100*time.Millisecond {
			t.Fatalf("%s context is not bounded at %v: %v", call.method, probeActivationRunnerMetadataTimeout, call.deadline)
		}
	}
}

func TestProbeActivationRunnerPreInvokeCancelRecordsNotStartedThenCanceled(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = time.Hour // watcher must not interfere
	// The operator cancels after the grant commits but before any invocation.
	fx.fake.onCommit["reserve"] = func(context.Context) {
		// Durable operator intent on another replica; not a port call from the
		// runner, so the invented-cancellation assertion stays meaningful.
		fx.fake.stageCancelIntent()
	}

	activation, err := fx.run()
	if err != nil || activation == nil || activation.State != domain.ProbeActivationCanceled {
		t.Fatalf("Run = (%v, %v), want terminal canceled", activation, err)
	}
	if fx.exec.callCount() != 0 {
		t.Fatalf("pre-invocation cancel dispatched the executor")
	}
	stored := fx.fake.snapshot()
	if stored.Steps[0].State != domain.ProbeInstallResourceNotStarted || stored.Steps[0].ErrorCode != "not_started" {
		t.Fatalf("never-invoked grant not recorded not_started: %+v", stored.Steps[0])
	}
	if stored.Facts != (domain.ProbeActivationFacts{}) {
		t.Fatalf("facts invented for a never-invoked init: %+v", stored.Facts)
	}
	for _, call := range fx.fake.callLog() {
		if call.method == "record" && call.facts != (domain.ProbeActivationFacts{}) {
			t.Fatalf("not_started recorded with facts: %+v", call)
		}
	}
	if _, _, _, cancels := fx.fake.countsSnapshot(); cancels != 0 {
		t.Fatalf("runner invented cancellation intent")
	}
}

// ---------------------------------------------------------------------------
// Observational defense, corruption and honest journal time
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerForeignStateNeverWrites(t *testing.T) {
	t.Run("changed attempt stops without writes", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		fx.fake.onCommit["reserve"] = func(context.Context) {
			fx.fake.setAttempt(domain.ProbeActivationProbeInit, actRunnerAttemptTwo)
		}
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, conflict)", activation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("changed attempt dispatched the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("changed attempt wrote history")
		}
	})

	t.Run("cold deadline expiry is never overwritten", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		fx.fake.onCommit["reserve"] = func(context.Context) {
			fx.fake.forceDeadlineExpired()
		}
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, conflict)", activation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("expired state dispatched the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("expired state wrote history")
		}
		stored := fx.fake.snapshot()
		if stored.State != domain.ProbeActivationInterrupted || stored.ErrorCode != "deadline_expired" ||
			stored.Steps[0].ErrorCode != "deadline_expired" {
			t.Fatalf("cold expiry not preserved: %s/%s %+v", stored.State, stored.ErrorCode, stored.Steps[0])
		}
	})

	t.Run("owner change stops without writes", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		fx.fake.onCommit["reserve"] = func(context.Context) {
			fx.fake.setOwner(actRunnerOwnerTwo)
		}
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", activation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("foreign owner dispatched the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("foreign owner wrote history")
		}
	})

	t.Run("identity change stops without writes", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		fx.fake.onCommit["reserve"] = func(context.Context) {
			fx.fake.setProbeKey("someone-else")
		}
		activation, err := fx.run()
		if !errors.Is(err, ports.ErrConflict) || activation != nil {
			t.Fatalf("Run = (%v, %v), want (nil, conflict)", activation, err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("foreign identity dispatched the executor")
		}
		if _, record, finish, _ := fx.fake.countsSnapshot(); record+finish != 0 {
			t.Fatalf("foreign identity wrote history")
		}
	})

	t.Run("watcher sees a foreign owner mid invocation and never writes", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = 2 * time.Millisecond
		started := make(chan struct{})
		var once sync.Once
		fx.exec.apply = func(ctx context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return actRunnerApplied(kind)
		}
		done := make(chan struct{})
		var activation *domain.ProbeActivation
		var runErr error
		go func() {
			defer close(done)
			activation, runErr = fx.run()
		}()
		<-started
		fx.fake.setOwner(actRunnerOwnerTwo)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("Run did not stop after the owner changed")
		}
		// The joined report cannot be recorded under a foreign owner and is
		// never repaired: no further write, no further dispatch.
		if !errors.Is(runErr, ports.ErrConflict) {
			t.Fatalf("Run = (%v, %v), want conflict", activation, runErr)
		}
		if _, _, finish, _ := fx.fake.countsSnapshot(); finish != 0 {
			t.Fatalf("finish attempted under a foreign owner")
		}
		if fx.exec.callCount() != 1 {
			t.Fatalf("dependents dispatched under a foreign owner")
		}
	})
}

func TestProbeActivationRunnerObservationLossStopsDispatch(t *testing.T) {
	t.Run("pre-invocation loss records truthful not_started", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		fx.fake.observeFailAt[2] = errors.New(actRunnerLeakMarker) // the re-observe
		activation, err := fx.run()
		if !errors.Is(err, domain.ErrInternal) || activation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, internal)", activation, err)
		}
		if strings.Contains(fmt.Sprint(err), actRunnerLeakMarker) {
			t.Fatalf("storage diagnostics leaked: %v", err)
		}
		if fx.exec.callCount() != 0 {
			t.Fatalf("observation loss dispatched the executor")
		}
		stored := fx.fake.snapshot()
		if stored.Steps[0].State != domain.ProbeInstallResourceNotStarted {
			t.Fatalf("never-invoked grant not truthful: %+v", stored.Steps[0])
		}
		if stored.State != domain.ProbeActivationInterrupted || stored.ErrorCode != "owner_interrupted" {
			t.Fatalf("terminal = %s/%s, want interrupted/owner_interrupted", stored.State, stored.ErrorCode)
		}
	})

	t.Run("watcher loss cancels the action and interrupts", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = 2 * time.Millisecond
		started := make(chan struct{})
		var once sync.Once
		fx.exec.apply = func(ctx context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return actRunnerApplied(kind)
		}
		done := make(chan struct{})
		var runErr error
		var activation *domain.ProbeActivation
		go func() {
			defer close(done)
			activation, runErr = fx.run()
		}()
		<-started
		fx.fake.setObserveErr(errors.New("storage unavailable"))
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("Run did not stop after observation loss")
		}
		if !errors.Is(runErr, domain.ErrInternal) || activation == nil {
			t.Fatalf("Run = (%v, %v), want (receipt, internal)", activation, runErr)
		}
		stored := fx.fake.snapshot()
		if stored.Steps[0].State != domain.ProbeInstallResourceApplied || stored.Facts != actRunnerFacts() {
			t.Fatalf("joined applied report and facts lost: %+v facts=%+v", stored.Steps[0], stored.Facts)
		}
		if stored.State != domain.ProbeActivationInterrupted || stored.ErrorCode != "owner_interrupted" {
			t.Fatalf("terminal = %s/%s, want interrupted/owner_interrupted", stored.State, stored.ErrorCode)
		}
		if fx.exec.callCount() != 1 {
			t.Fatalf("dependents dispatched after observation loss")
		}
	})
}

func TestProbeActivationRunnerColdExpiryRejectsLateApplied(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = time.Hour
	proceed := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	fx.exec.apply = func(_ context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		once.Do(func() { close(started) })
		<-proceed
		return actRunnerApplied(kind)
	}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = fx.run()
	}()
	<-started
	fx.fake.expire() // the DB clock passes the fixed deadline mid-invocation
	close(proceed)
	<-done

	if !errors.Is(runErr, ports.ErrConflict) {
		t.Fatalf("Run error = %v, want conflict", runErr)
	}
	stored := fx.fake.snapshot()
	if stored.State != domain.ProbeActivationInterrupted || stored.ErrorCode != "deadline_expired" {
		t.Fatalf("expiry uncertainty replaced: %s/%s", stored.State, stored.ErrorCode)
	}
	if stored.Steps[0].State != domain.ProbeInstallResourceUnknown || stored.Steps[0].ErrorCode != "deadline_expired" {
		t.Fatalf("late applied repaired unknown: %+v", stored.Steps[0])
	}
	if stored.Facts != (domain.ProbeActivationFacts{}) {
		t.Fatalf("late applied facts were stored: %+v", stored.Facts)
	}
	if _, _, finish, _ := fx.fake.countsSnapshot(); finish != 0 {
		t.Fatalf("finish attempted over expiry uncertainty")
	}
}

func TestProbeActivationRunnerRunBoundFiresAsOwnerInterruption(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = time.Hour
	fx.runner.runBound = 40 * time.Millisecond
	fx.exec.apply = func(ctx context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		<-ctx.Done()
		return actRunnerApplied(kind)
	}
	started := time.Now()
	activation, err := fx.run()
	if !errors.Is(err, context.DeadlineExceeded) || activation == nil {
		t.Fatalf("Run = (%v, %v), want (receipt, deadline)", activation, err)
	}
	if elapsed := time.Since(started); elapsed > domain.ProbeActivationLifetime {
		t.Fatalf("run bound not honored: %v", elapsed)
	}
	if activation.State != domain.ProbeActivationInterrupted || activation.ErrorCode != "owner_interrupted" {
		t.Fatalf("terminal = %s/%s, want interrupted/owner_interrupted", activation.State, activation.ErrorCode)
	}
	if reserve, _, _, _ := fx.fake.countsSnapshot(); reserve != 1 {
		t.Fatalf("dependent reserved after the run bound fired: %d", reserve)
	}
}

// ---------------------------------------------------------------------------
// Receipt validation: the runner trusts no acknowledgement it cannot verify.
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerRejectsUntrustedReceipts(t *testing.T) {
	retryForeignFacts := actRunnerOtherFacts()
	cases := []struct {
		name         string
		method       string
		notStarted   bool
		mangle       func(*domain.ProbeActivation) *domain.ProbeActivation
		wantNil      bool // Run returns no receipt at all
		wantReserve  int
		wantExecuted int
		wantFinish   int
	}{
		{name: "nil begin receipt", method: "begin", wantNil: true,
			mangle: func(*domain.ProbeActivation) *domain.ProbeActivation { return nil }},
		{name: "begin receipt for another owner", method: "begin", wantNil: true,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.OwnerID = actRunnerOwnerTwo
				return c
			}},
		{name: "begin receipt for another operation", method: "begin", wantNil: true,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.Identity.Install.OperationID = actRunnerOtherOp
				return c
			}},
		{name: "begin receipt with a truncated graph", method: "begin", wantNil: true,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.Steps = c.Steps[:3]
				return c
			}},
		{name: "invalid observation", method: "observe", wantNil: true,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.Steps = nil
				return c
			}},
		{name: "reserve grant not in flight", method: "reserve", wantReserve: 1,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.Steps[0] = domain.ProbeInstallResource{Spec: c.Steps[0].Spec, State: domain.ProbeInstallResourcePlanned}
				return c
			}},
		{name: "reserve grant for another attempt", method: "reserve", wantReserve: 1,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.Steps[0].AttemptID = actRunnerAttemptTwo
				return c
			}},
		{name: "record not reflecting the outcome", method: "record", wantReserve: 1, wantExecuted: 1,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.Steps[0].State = domain.ProbeInstallResourceInFlight
				c.Steps[0].FinishedAt = time.Time{}
				c.Facts = domain.ProbeActivationFacts{}
				return c
			}},
		{name: "record returning foreign facts", method: "record", wantReserve: 1, wantExecuted: 1,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.Facts = retryForeignFacts
				return c
			}},
		{name: "finish returning a nonterminal receipt", method: "finish", notStarted: true, wantReserve: 1, wantExecuted: 1, wantFinish: 1,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.State, c.ErrorCode = domain.ProbeActivationRunning, ""
				return c
			}},
		{name: "finish returning another outcome", method: "finish", notStarted: true, wantReserve: 1, wantExecuted: 1, wantFinish: 1,
			mangle: func(a *domain.ProbeActivation) *domain.ProbeActivation {
				c := actRunnerClone(a)
				c.State, c.ErrorCode = domain.ProbeActivationCanceled, "canceled"
				return c
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := actRunnerSetup(t)
			fx.runner.pollInterval = time.Hour
			if tc.notStarted {
				fx.exec.apply = func(context.Context, domain.ProbeActivation, string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
					return actRunnerNotStarted(), domain.ProbeActivationFacts{}, nil
				}
			}
			fx.fake.mangle[tc.method] = tc.mangle
			activation, err := fx.run()
			if !errors.Is(err, domain.ErrInternal) {
				t.Fatalf("Run = (%v, %v), want internal", activation, err)
			}
			if tc.wantNil && activation != nil {
				t.Fatalf("untrusted receipt surfaced: %+v", activation)
			}
			if reserve, _, finish, _ := fx.fake.countsSnapshot(); reserve != tc.wantReserve || finish != tc.wantFinish {
				t.Fatalf("reserve=%d finish=%d, want %d/%d", reserve, finish, tc.wantReserve, tc.wantFinish)
			}
			if fx.exec.callCount() != tc.wantExecuted {
				t.Fatalf("executor calls = %d, want %d", fx.exec.callCount(), tc.wantExecuted)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Copies, bounded contexts and honest clock behavior
// ---------------------------------------------------------------------------

func TestProbeActivationRunnerDefensiveCopies(t *testing.T) {
	t.Run("attempts are copied before I/O", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		original := append([]string(nil), fx.attempts...)
		fx.exec.apply = func(_ context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			// Corrupt the caller's slice mid-run.
			for i := range fx.attempts {
				fx.attempts[i] = actRunnerAttemptTwo
			}
			return actRunnerApplied(kind)
		}
		activation, err := fx.run()
		if err != nil || activation == nil || activation.State != domain.ProbeActivationActive {
			t.Fatalf("Run = (%v, %v), want active", activation, err)
		}
		for i, step := range activation.Steps {
			if step.AttemptID != original[i] {
				t.Fatalf("step %d bound to %q, want %q", i, step.AttemptID, original[i])
			}
		}
		for i, call := range fx.fake.callLog() {
			if call.method != "reserve" && call.method != "record" {
				continue
			}
			found := false
			for _, attempt := range original {
				found = found || call.attempt == attempt
			}
			if !found {
				t.Fatalf("call %d used a mutated attempt %q", i, call.attempt)
			}
		}
	})

	t.Run("executor receives a defensive copy", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		fx.exec.apply = func(_ context.Context, activation domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			activation.Steps[0].State = "junk"
			activation.Steps[0].AttemptID = "junk"
			activation.Identity.Install.Host.User = "junk"
			activation.OwnerID = "junk"
			activation.Facts = domain.ProbeActivationFacts{}
			return actRunnerApplied(kind)
		}
		activation, err := fx.run()
		if err != nil || activation == nil || activation.State != domain.ProbeActivationActive {
			t.Fatalf("Run = (%v, %v), want active", activation, err)
		}
		if !activation.Valid() || activation.Identity != fx.identity || activation.OwnerID != fx.owner || activation.Facts != actRunnerFacts() {
			t.Fatalf("executor mutation corrupted the coordinator state: %+v", activation)
		}
		stored := fx.fake.snapshot()
		if stored.Steps[0].AttemptID != fx.attempts[0] || stored.Steps[0].State != domain.ProbeInstallResourceApplied {
			t.Fatalf("executor mutation corrupted storage: %+v", stored.Steps[0])
		}
	})

	t.Run("returned receipts own their slices", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		activation, err := fx.run()
		if err != nil || activation == nil {
			t.Fatalf("Run = (%v, %v), want active", activation, err)
		}
		activation.Steps[0].State = "junk"
		activation.Identity.Install.Host.User = "junk"
		activation.Facts = domain.ProbeActivationFacts{}
		stored := fx.fake.snapshot()
		if stored.Steps[0].State != domain.ProbeInstallResourceApplied || stored.Identity.Install.Host.User != "root" || stored.Facts != actRunnerFacts() {
			t.Fatalf("returned receipt shares storage state: %+v", stored.Steps[0])
		}
	})

	t.Run("returned failure receipts own their slices", func(t *testing.T) {
		fx := actRunnerSetup(t)
		fx.runner.pollInterval = time.Hour
		fx.exec.apply = func(context.Context, domain.ProbeActivation, string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
			return actRunnerNotStarted(), domain.ProbeActivationFacts{}, nil
		}
		activation, err := fx.run()
		if err != nil || activation == nil || activation.State != domain.ProbeActivationFailed {
			t.Fatalf("Run = (%v, %v), want failed", activation, err)
		}
		activation.Steps[0].State = "junk"
		if stored := fx.fake.snapshot(); stored.Steps[0].State != domain.ProbeInstallResourceNotStarted {
			t.Fatalf("returned receipt shares storage state: %+v", stored.Steps[0])
		}
	})
}

func TestProbeActivationRunnerBoundedMetadataAndCompletionContexts(t *testing.T) {
	fx := actRunnerSetup(t)
	fx.runner.pollInterval = 2 * time.Millisecond
	var execDeadline time.Duration
	fx.exec.apply = func(ctx context.Context, _ domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, error) {
		if deadline, ok := ctx.Deadline(); ok {
			execDeadline = time.Until(deadline)
		}
		if ctx.Done() == nil {
			t.Errorf("executor context is not cancelable")
		}
		return actRunnerApplied(kind)
	}
	if _, err := fx.run(); err != nil {
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
		limit := probeActivationRunnerMetadataTimeout + 100*time.Millisecond
		if call.method == "observe" && call.deadline <= probeActivationRunnerWatchReadTimeout+100*time.Millisecond {
			limit = probeActivationRunnerWatchReadTimeout + 100*time.Millisecond
		}
		if call.deadline > limit {
			t.Fatalf("%s context budget %v exceeds %v", call.method, call.deadline, limit)
		}
	}
	if execDeadline <= 0 || execDeadline > domain.ProbeActivationLifetime {
		t.Fatalf("executor context not bounded by the run bound: %v", execDeadline)
	}
}

func TestProbeActivationRunnerTrustsJournalClockNotHostClock(t *testing.T) {
	// The stored deadline is far in the host's past: a host-clock comparison
	// would refuse to dispatch, but DB authority is the journal's state.
	fx := actRunnerSetupBase(t, time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC))
	fx.runner.pollInterval = time.Hour
	if !fx.fake.snapshot().DeadlineAt.Before(time.Now()) {
		t.Fatalf("fixture deadline is not in the host past")
	}
	activation, err := fx.run()
	if err != nil || activation == nil || activation.State != domain.ProbeActivationActive {
		t.Fatalf("Run = (%v, %v), want active under a host-past deadline", activation, err)
	}
	if fx.exec.callCount() != len(fx.identity.Steps()) {
		t.Fatalf("executor calls = %d, want full sequence", fx.exec.callCount())
	}
}
