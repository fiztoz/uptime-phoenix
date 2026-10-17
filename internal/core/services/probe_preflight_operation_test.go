package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	opPreflightOwnerA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	opPreflightOwnerB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

const (
	opPreflightID      = "11111111-1111-4111-8111-111111111111"
	opPreflightIDTwo   = "22222222-2222-4222-8222-222222222222"
	opPreflightIDThree = "33333333-3333-4333-8333-333333333333"
)

func opPreflightFingerprint(fill byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func opPreflightSubmission(opID string, requestedBy int64) domain.ProbePreflightSubmission {
	return domain.ProbePreflightSubmission{
		OperationID: opID,
		Host: domain.ProbeSSHHost{
			Address: "edge.example:22",
			User:    "root",
		},
		HostKeyFingerprint: opPreflightFingerprint(0x01),
		RequestedBy:        requestedBy,
		PrivateKey:         []byte("transient-ssh-key-material"),
	}
}

type opPreflightInspector struct {
	mu          sync.Mutex
	startedOnce bool
	started     chan struct{}
	release     chan struct{}
	facts       domain.ProbeHostFacts
	err         error
	calls       int
	returned    bool
	canceled    bool
}

func opPreflightNewInspector(blocked bool) *opPreflightInspector {
	inspector := &opPreflightInspector{
		facts: domain.ProbeHostFacts{Kernel: "Linux", Machine: "x86_64", Privilege: "root", Systemd: true},
	}
	if blocked {
		inspector.started = make(chan struct{})
		inspector.release = make(chan struct{})
	}
	return inspector
}

func (i *opPreflightInspector) InspectHost(ctx context.Context, _ domain.ProbeSSHHost) (domain.ProbeHostFacts, error) {
	i.mu.Lock()
	i.calls++
	if i.started != nil && !i.startedOnce {
		i.startedOnce = true
		close(i.started)
	}
	facts, err, release := i.facts, i.err, i.release
	i.mu.Unlock()
	if release == nil {
		i.markReturned()
		return facts, err
	}
	select {
	case <-release:
		i.markReturned()
		return facts, err
	case <-ctx.Done():
		i.mu.Lock()
		i.canceled = true
		i.returned = true
		i.mu.Unlock()
		return domain.ProbeHostFacts{}, ctx.Err()
	}
}

func (i *opPreflightInspector) markReturned() {
	i.mu.Lock()
	i.returned = true
	i.mu.Unlock()
}

func (i *opPreflightInspector) counts() (calls int, returned bool, canceled bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls, i.returned, i.canceled
}

type opPreflightFactory struct {
	mu          sync.Mutex
	calls       int
	err         error
	fingerprint string
	inspectors  []ports.ProbeHostInspector
}

func (f *opPreflightFactory) NewInspector(ctx context.Context, privateKey []byte, _ string) (ports.ProbeHostInspector, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, "", f.err
	}
	if len(privateKey) == 0 {
		return nil, "", domain.ErrProbeSSHInput
	}
	if len(f.inspectors) == 0 {
		return nil, "", domain.ErrInternal
	}
	index := f.calls - 1
	if index >= len(f.inspectors) {
		index = len(f.inspectors) - 1
	}
	return f.inspectors[index], f.fingerprint, nil
}

func (f *opPreflightFactory) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *opPreflightFactory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type opPreflightCtxSample struct {
	err         error
	deadline    time.Time
	hasDeadline bool
}

// opPreflightStore is an effectful in-memory ProbePreflightRepository that
// enforces the frozen fences: owner, lifecycle state, deadline and outcome
// validity, so service tests exercise real repository effects instead of
// call-count assertions alone.
type opPreflightStore struct {
	mu   sync.Mutex
	rows map[string]*domain.ProbePreflightOperation

	creates   int
	committed int
	begins    int
	finishes  int
	observes  int
	cancels   int

	joinViolations int

	createErr     error
	createCommits bool
	beginErr      error
	finishErr     error
	observeErr    error
	cancelErr     error

	onBegin   func()
	onFinish  func(opID, outcomeState string)
	joinGuard func(opID string) bool

	finishSamples []opPreflightCtxSample
}

func newOpPreflightStore() *opPreflightStore {
	return &opPreflightStore{rows: map[string]*domain.ProbePreflightOperation{}}
}

func opPreflightCopyOperation(o *domain.ProbePreflightOperation) *domain.ProbePreflightOperation {
	if o == nil {
		return nil
	}
	copied := *o
	if o.Result != nil {
		result := *o.Result
		copied.Result = &result
	}
	return &copied
}

func opPreflightNewRow(request domain.ProbePreflightIdentity, ownerID string) *domain.ProbePreflightOperation {
	now := time.Now().UTC()
	return &domain.ProbePreflightOperation{
		Identity:   request,
		OwnerID:    ownerID,
		State:      domain.ProbePreflightQueued,
		CreatedAt:  now,
		UpdatedAt:  now,
		DeadlineAt: now.Add(domain.ProbePreflightLifetime),
	}
}

func (s *opPreflightStore) CreatePreflight(ctx context.Context, request domain.ProbePreflightIdentity, ownerID string) (*domain.ProbePreflightOperation, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if s.createErr != nil {
		if s.createCommits {
			if _, exists := s.rows[request.OperationID]; !exists {
				s.rows[request.OperationID] = opPreflightNewRow(request, ownerID)
				s.committed++
			}
		}
		return nil, false, s.createErr
	}
	if existing, ok := s.rows[request.OperationID]; ok {
		// The owner is deliberately not part of duplicate binding: an exact
		// identity retry returns the original owner unchanged.
		if existing.Identity != request {
			return nil, false, ports.ErrConflict
		}
		return opPreflightCopyOperation(existing), false, nil
	}
	row := opPreflightNewRow(request, ownerID)
	if !row.Valid() {
		return nil, false, domain.ErrValidation
	}
	s.rows[request.OperationID] = row
	s.committed++
	return opPreflightCopyOperation(row), true, nil
}

func (s *opPreflightStore) ObservePreflight(ctx context.Context, operationID string) (*domain.ProbePreflightOperation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observes++
	if s.observeErr != nil {
		return nil, s.observeErr
	}
	row, ok := s.rows[operationID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	if !row.Terminal() && !time.Now().UTC().Before(row.DeadlineAt) {
		row.State = domain.ProbePreflightInterrupted
		row.ErrorCode = "interrupted"
		row.UpdatedAt = time.Now().UTC()
	}
	return opPreflightCopyOperation(row), nil
}

func (s *opPreflightStore) BeginPreflight(ctx context.Context, operationID, ownerID string) (*domain.ProbePreflightOperation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.onBegin != nil {
		s.onBegin()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begins++
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	row, ok := s.rows[operationID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	if row.State != domain.ProbePreflightQueued || row.OwnerID != ownerID ||
		!time.Now().UTC().Before(row.DeadlineAt) {
		return nil, ports.ErrConflict
	}
	row.State = domain.ProbePreflightRunning
	row.UpdatedAt = time.Now().UTC()
	return opPreflightCopyOperation(row), nil
}

func (s *opPreflightStore) FinishPreflight(ctx context.Context, operationID, ownerID string, outcome domain.ProbePreflightOutcome) (*domain.ProbePreflightOperation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.onFinish != nil {
		s.onFinish(operationID, outcome.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishes++
	sample := opPreflightCtxSample{}
	sample.err = ctx.Err()
	sample.deadline, sample.hasDeadline = ctx.Deadline()
	s.finishSamples = append(s.finishSamples, sample)
	if s.finishErr != nil {
		return nil, s.finishErr
	}
	row, ok := s.rows[operationID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	if row.OwnerID != ownerID {
		return nil, ports.ErrConflict
	}
	if row.Terminal() {
		if row.State == outcome.State && row.ErrorCode == outcome.ErrorCode {
			return opPreflightCopyOperation(row), nil
		}
		return nil, ports.ErrConflict
	}
	if !time.Now().UTC().Before(row.DeadlineAt) {
		return nil, ports.ErrConflict
	}
	if !outcome.Valid() {
		return nil, domain.ErrValidation
	}
	switch outcome.State {
	case domain.ProbePreflightPassed, domain.ProbePreflightFailed:
		if row.State != domain.ProbePreflightRunning {
			return nil, ports.ErrConflict
		}
	case domain.ProbePreflightCanceled:
		if row.State != domain.ProbePreflightCancelRequested {
			return nil, ports.ErrConflict
		}
	case domain.ProbePreflightInterrupted:
		// A joined owner may interrupt any nonterminal state on shutdown.
	default:
		return nil, domain.ErrValidation
	}
	if s.joinGuard != nil && !s.joinGuard(operationID) {
		s.joinViolations++
	}
	row.State = outcome.State
	row.ErrorCode = outcome.ErrorCode
	row.Result = outcome.Result
	updated := time.Now().UTC()
	if updated.Before(row.UpdatedAt) {
		updated = row.UpdatedAt
	}
	row.UpdatedAt = updated
	return opPreflightCopyOperation(row), nil
}

func (s *opPreflightStore) RequestPreflightCancellation(ctx context.Context, operationID string) (*domain.ProbePreflightOperation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels++
	if s.cancelErr != nil {
		return nil, s.cancelErr
	}
	row, ok := s.rows[operationID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	if row.Terminal() {
		return opPreflightCopyOperation(row), nil
	}
	row.State = domain.ProbePreflightCancelRequested
	row.UpdatedAt = time.Now().UTC()
	return opPreflightCopyOperation(row), nil
}

func (s *opPreflightStore) snapshot(operationID string) *domain.ProbePreflightOperation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return opPreflightCopyOperation(s.rows[operationID])
}

func (s *opPreflightStore) expire(operationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[operationID]; ok {
		row.CreatedAt = time.Now().UTC().Add(-2 * domain.ProbePreflightLifetime)
		row.DeadlineAt = row.CreatedAt.Add(domain.ProbePreflightLifetime)
	}
}

func (s *opPreflightStore) setFinishErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishErr = err
}

func (s *opPreflightStore) createCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

func (s *opPreflightStore) committedRows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
}

func (s *opPreflightStore) beginCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.begins
}

func (s *opPreflightStore) finishCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishes
}

func (s *opPreflightStore) observeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observes
}

func (s *opPreflightStore) cancelCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancels
}

func (s *opPreflightStore) violations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joinViolations
}

func (s *opPreflightStore) samples() []opPreflightCtxSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	samples := make([]opPreflightCtxSample, len(s.finishSamples))
	copy(samples, s.finishSamples)
	return samples
}

type opPreflightEnv struct {
	svc        *ProbePreflightOperationService
	store      *opPreflightStore
	factory    *opPreflightFactory
	inspectors []*opPreflightInspector
}

func opPreflightEnvFor(t *testing.T, ctx context.Context, capacity int, inspectors ...*opPreflightInspector) *opPreflightEnv {
	t.Helper()
	if len(inspectors) == 0 {
		inspectors = append(inspectors, opPreflightNewInspector(false))
	}
	store := newOpPreflightStore()
	factoryInspectors := make([]ports.ProbeHostInspector, 0, len(inspectors))
	for _, inspector := range inspectors {
		factoryInspectors = append(factoryInspectors, inspector)
	}
	factory := &opPreflightFactory{
		fingerprint: opPreflightFingerprint(0x5a),
		inspectors:  factoryInspectors,
	}
	service, err := NewProbePreflightOperationService(ctx, store, factory, opPreflightOwnerA, capacity)
	if err != nil {
		t.Fatalf("constructing service: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := service.Close(closeCtx); err != nil {
			t.Errorf("closing service: %v", err)
		}
	})
	return &opPreflightEnv{
		svc:        service,
		store:      store,
		factory:    factory,
		inspectors: inspectors,
	}
}

func (e *opPreflightEnv) waitTerminal(t *testing.T, operationID string) *domain.ProbePreflightOperation {
	t.Helper()
	var operation *domain.ProbePreflightOperation
	opPreflightWait(t, "terminal operation "+operationID, func() bool {
		operation = e.store.snapshot(operationID)
		return operation != nil && operation.Terminal()
	})
	return operation
}

func opPreflightWait(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func opPreflightAssertWriteCtx(t *testing.T, sample opPreflightCtxSample) {
	t.Helper()
	if sample.err != nil {
		t.Fatalf("final write context was already canceled: %v", sample.err)
	}
	if !sample.hasDeadline {
		t.Fatal("final write context had no deadline")
	}
	remaining := time.Until(sample.deadline)
	if remaining <= 0 || remaining > probePreflightFinalWriteTTL {
		t.Fatalf("final write deadline outside the bounded window: %v", remaining)
	}
}

func assertOpPreflightNoWork(t *testing.T, env *opPreflightEnv, operationID string, expectNoRow bool) {
	t.Helper()
	if begins, observes := env.store.beginCalls(), env.store.observeCalls(); begins != 0 || observes != 0 {
		t.Fatalf("begins=%d observes=%d, want no execution", begins, observes)
	}
	for _, inspector := range env.inspectors {
		if calls, _, _ := inspector.counts(); calls != 0 {
			t.Fatalf("inspector contacted %d times before commit", calls)
		}
	}
	if expectNoRow {
		if operation := env.store.snapshot(operationID); operation != nil {
			t.Fatalf("row must not exist: %+v", operation)
		}
	}
}

func TestProbePreflightOperationServiceConstructor(t *testing.T) {
	store := newOpPreflightStore()
	factory := &opPreflightFactory{
		fingerprint: opPreflightFingerprint(0x5a),
		inspectors:  []ports.ProbeHostInspector{opPreflightNewInspector(false)},
	}
	service, err := NewProbePreflightOperationService(t.Context(), store, factory, opPreflightOwnerA, probePreflightMaxCapacity)
	if err != nil || service == nil {
		t.Fatalf("service=%v error=%v", service, err)
	}
	if service.pollEvery <= 0 || service.pollEvery > time.Second {
		t.Fatalf("poll interval %v violates the at-most-once-per-second contract", service.pollEvery)
	}
	if service, err = NewProbePreflightOperationService(context.Background(), store, factory, opPreflightOwnerA, probePreflightMinCapacity); err != nil || service == nil {
		t.Fatalf("minimum capacity must be admitted: %v", err)
	}
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		repo     ports.ProbePreflightRepository
		factory  ports.ProbeHostInspectorFactory
		ownerID  string
		capacity int
	}{
		{"nil_context", nil, store, factory, opPreflightOwnerA, 4},
		{"nil_repository", t.Context(), nil, factory, opPreflightOwnerA, 4},
		{"nil_factory", t.Context(), store, nil, opPreflightOwnerA, 4},
		{"reserved_owner", t.Context(), store, factory, domain.LocalProbeID, 4},
		{"zero_capacity", t.Context(), store, factory, opPreflightOwnerA, 0},
		{"excessive_capacity", t.Context(), store, factory, opPreflightOwnerA, probePreflightMaxCapacity + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if built, err := NewProbePreflightOperationService(tc.ctx, tc.repo, tc.factory, tc.ownerID, tc.capacity); built != nil || !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("service=%v error=%v", built, err)
			}
		})
	}
}

func TestProbePreflightOperationServiceBoundedAdmission(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 1, opPreflightNewInspector(true))
	first, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7))
	if err != nil || first == nil || first.State != domain.ProbePreflightQueued {
		t.Fatalf("operation=%+v error=%v", first, err)
	}
	opPreflightWait(t, "first inspection to start", func() bool {
		calls, _, _ := env.inspectors[0].counts()
		return calls == 1
	})
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightIDTwo, 7)); !errors.Is(err, domain.ErrProbePreflightBusy) {
		t.Fatalf("capacity exhaustion must report busy: %v", err)
	}
	if calls := env.factory.callCount(); calls != 2 {
		t.Fatalf("busy identity check parsed %d keys, want 2", calls)
	}
	duplicate, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7))
	if err != nil || duplicate == nil || duplicate.OwnerID != first.OwnerID || !duplicate.DeadlineAt.Equal(first.DeadlineAt) {
		t.Fatalf("saturated exact retry must preserve original receipt: %+v %v", duplicate, err)
	}
	changed := opPreflightSubmission(opPreflightID, 8)
	if _, err := env.svc.Submit(t.Context(), changed); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("saturated changed identity: %v", err)
	}
	if calls, _, _ := env.inspectors[0].counts(); calls != 1 {
		t.Fatalf("saturated retry dispatched %d inspections", calls)
	}
	close(env.inspectors[0].release)
	env.waitTerminal(t, opPreflightID)
	deadline := time.Now().Add(10 * time.Second)
	var second *domain.ProbePreflightOperation
	for {
		second, err = env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightIDTwo, 7))
		if err == nil {
			break
		}
		if !errors.Is(err, domain.ErrProbePreflightBusy) || time.Now().After(deadline) {
			t.Fatalf("operation=%+v error=%v", second, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if second == nil {
		t.Fatal("second submission returned no operation")
	}
	env.waitTerminal(t, opPreflightIDTwo)
	if calls, _, _ := env.inspectors[0].counts(); calls != 2 {
		t.Fatalf("inspections=%d, want 2", calls)
	}
	if begins, finishes := env.store.beginCalls(), env.store.finishCalls(); begins != 2 || finishes != 2 {
		t.Fatalf("begins=%d finishes=%d, want 2/2", begins, finishes)
	}
	if got := env.store.snapshot(opPreflightID); got == nil || got.State != domain.ProbePreflightPassed {
		t.Fatalf("first operation %+v", got)
	}
	if got := env.store.snapshot(opPreflightIDTwo); got == nil || got.State != domain.ProbePreflightPassed {
		t.Fatalf("second operation %+v", got)
	}
}

func TestProbePreflightOperationServiceClosedAdmission(t *testing.T) {
	t.Run("closed_before_submit", func(t *testing.T) {
		env := opPreflightEnvFor(t, t.Context(), 4)
		if err := env.svc.Close(t.Context()); err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); !errors.Is(err, domain.ErrProbePreflightClosed) {
			t.Fatalf("admission after close: %v", err)
		}
		if calls := env.factory.callCount(); calls != 0 {
			t.Fatalf("closed admission parsed %d keys, want 0", calls)
		}
		if _, err := env.svc.Observe(t.Context(), opPreflightID); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("reads must survive shutdown: %v", err)
		}
		if err := env.svc.Close(t.Context()); err != nil {
			t.Fatalf("second close: %v", err)
		}
	})
	t.Run("canceled_root_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		env := opPreflightEnvFor(t, ctx, 4)
		cancel()
		if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); !errors.Is(err, domain.ErrProbePreflightClosed) {
			t.Fatalf("admission after root cancellation: %v", err)
		}
		if calls := env.factory.callCount(); calls != 0 {
			t.Fatalf("canceled admission parsed %d keys, want 0", calls)
		}
	})
}

func TestProbePreflightOperationServiceExactDuplicateNeverDispatchesTwice(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 4, opPreflightNewInspector(true))
	primary, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7))
	if err != nil || primary == nil || primary.OwnerID != opPreflightOwnerA {
		t.Fatalf("operation=%+v error=%v", primary, err)
	}
	opPreflightWait(t, "primary inspection to start", func() bool {
		calls, _, _ := env.inspectors[0].counts()
		return calls == 1
	})
	secondary, err := NewProbePreflightOperationService(t.Context(), env.store, env.factory, opPreflightOwnerB, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := secondary.Close(closeCtx); err != nil {
			t.Errorf("closing secondary service: %v", err)
		}
	})
	duplicate, err := secondary.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7))
	if err != nil || duplicate == nil {
		t.Fatalf("exact retry operation=%+v error=%v", duplicate, err)
	}
	if duplicate.OwnerID != opPreflightOwnerA || duplicate.State != domain.ProbePreflightRunning {
		t.Fatalf("exact retry rebinds or renews the receipt: %+v", duplicate)
	}
	if _, err := secondary.Submit(t.Context(), opPreflightSubmission(opPreflightID, 8)); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("changed actor must conflict: %v", err)
	}
	if calls, _, _ := env.inspectors[0].counts(); calls != 1 {
		t.Fatalf("duplicate submissions dispatched %d inspections, want 1", calls)
	}
	if begins, finishes := env.store.beginCalls(), env.store.finishCalls(); begins != 1 || finishes != 0 {
		t.Fatalf("begins=%d finishes=%d, want 1/0", begins, finishes)
	}
	close(env.inspectors[0].release)
	env.waitTerminal(t, opPreflightID)
	if got := env.store.snapshot(opPreflightID); got == nil || got.State != domain.ProbePreflightPassed {
		t.Fatalf("completed operation %+v", got)
	}
	finalRetry, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7))
	if err != nil || finalRetry == nil || finalRetry.State != domain.ProbePreflightPassed {
		t.Fatalf("terminal retry operation=%+v error=%v", finalRetry, err)
	}
	if calls, _, _ := env.inspectors[0].counts(); calls != 1 {
		t.Fatalf("terminal retry dispatched again: inspections=%d", calls)
	}
	if begins, finishes := env.store.beginCalls(), env.store.finishCalls(); begins != 1 || finishes != 1 {
		t.Fatalf("begins=%d finishes=%d, want 1/1", begins, finishes)
	}
}

func TestProbePreflightOperationServiceNoDispatchBeforeCommit(t *testing.T) {
	t.Run("empty_private_key", func(t *testing.T) {
		env := opPreflightEnvFor(t, t.Context(), 4)
		request := opPreflightSubmission(opPreflightID, 7)
		request.PrivateKey = nil
		if _, err := env.svc.Submit(t.Context(), request); !errors.Is(err, domain.ErrProbeSSHInput) {
			t.Fatalf("error=%v", err)
		}
		assertOpPreflightNoWork(t, env, opPreflightID, true)
	})
	t.Run("invalid_operation_id", func(t *testing.T) {
		env := opPreflightEnvFor(t, t.Context(), 4)
		if _, err := env.svc.Submit(t.Context(), opPreflightSubmission("not-a-uuid", 7)); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("error=%v", err)
		}
		assertOpPreflightNoWork(t, env, "not-a-uuid", true)
	})
	t.Run("canceled_request", func(t *testing.T) {
		env := opPreflightEnvFor(t, t.Context(), 4)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := env.svc.Submit(ctx, opPreflightSubmission(opPreflightID, 7)); !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
		assertOpPreflightNoWork(t, env, opPreflightID, true)
	})
	t.Run("factory_error", func(t *testing.T) {
		env := opPreflightEnvFor(t, t.Context(), 4)
		parsing := errors.New("key parse failed")
		env.factory.setErr(parsing)
		if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); !errors.Is(err, parsing) {
			t.Fatalf("error=%v", err)
		}
		if creates := env.store.createCalls(); creates != 0 {
			t.Fatalf("factory failures created %d rows, want 0", creates)
		}
		assertOpPreflightNoWork(t, env, opPreflightID, true)
	})
	t.Run("failed_insert", func(t *testing.T) {
		env := opPreflightEnvFor(t, t.Context(), 4)
		insert := errors.New("insert failed")
		env.store.createErr = insert
		if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); !errors.Is(err, insert) {
			t.Fatalf("error=%v", err)
		}
		if creates, committed := env.store.createCalls(), env.store.committedRows(); creates != 1 || committed != 0 {
			t.Fatalf("creates=%d committed=%d, want 1/0", creates, committed)
		}
		assertOpPreflightNoWork(t, env, opPreflightID, true)
	})
	t.Run("ambiguous_commit", func(t *testing.T) {
		env := opPreflightEnvFor(t, t.Context(), 4)
		ack := errors.New("commit acknowledgement lost")
		env.store.createErr = ack
		env.store.createCommits = true
		if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); !errors.Is(err, ack) {
			t.Fatalf("error=%v", err)
		}
		if committed := env.store.committedRows(); committed != 1 {
			t.Fatalf("committed=%d, want 1", committed)
		}
		assertOpPreflightNoWork(t, env, opPreflightID, false)
		env.store.expire(opPreflightID)
		reconciled, err := env.svc.Observe(t.Context(), opPreflightID)
		if err != nil || reconciled == nil || reconciled.State != domain.ProbePreflightInterrupted ||
			reconciled.ErrorCode != "interrupted" {
			t.Fatalf("operation=%+v error=%v", reconciled, err)
		}
	})
}

func TestProbePreflightOperationServiceBeginFailureLeavesHonestRow(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 4)
	env.store.beginErr = errors.New("begin write failed")
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opPreflightWait(t, "failed begin reconciliation", func() bool {
		return env.store.beginCalls() >= 1 && env.store.observeCalls() >= 1
	})
	if operation := env.store.snapshot(opPreflightID); operation == nil || operation.Terminal() ||
		operation.State != domain.ProbePreflightQueued {
		t.Fatalf("unexecuted row must stay honestly nonterminal: %+v", operation)
	}
	if calls, _, _ := env.inspectors[0].counts(); calls != 0 {
		t.Fatalf("failed begin must not dispatch: inspections=%d", calls)
	}
	if finishes := env.store.finishCalls(); finishes != 0 {
		t.Fatalf("failed begin wrote %d outcomes, want 0", finishes)
	}
	env.store.expire(opPreflightID)
	reconciled, err := env.svc.Observe(t.Context(), opPreflightID)
	if err != nil || reconciled == nil || reconciled.State != domain.ProbePreflightInterrupted {
		t.Fatalf("operation=%+v error=%v", reconciled, err)
	}
}

func TestProbePreflightOperationServiceCancelBeforeBeginNeverDispatches(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 4, opPreflightNewInspector(true))
	env.store.joinGuard = func(string) bool {
		calls, returned, _ := env.inspectors[0].counts()
		return calls == 0 || returned
	}
	env.store.onBegin = func() {
		if _, err := env.store.RequestPreflightCancellation(context.Background(), opPreflightID); err != nil {
			t.Errorf("cross-replica cancellation: %v", err)
		}
	}
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	operation := env.waitTerminal(t, opPreflightID)
	if operation.State != domain.ProbePreflightCanceled || operation.ErrorCode != "canceled" {
		t.Fatalf("operation %+v", operation)
	}
	if calls, _, _ := env.inspectors[0].counts(); calls != 0 {
		t.Fatalf("cancellation before begin dispatched %d inspections", calls)
	}
	if begins, finishes := env.store.beginCalls(), env.store.finishCalls(); begins != 1 || finishes != 1 {
		t.Fatalf("begins=%d finishes=%d, want 1/1", begins, finishes)
	}
	if violations := env.store.violations(); violations != 0 {
		t.Fatalf("completion persisted before inspector join: %d violations", violations)
	}
}

func TestProbePreflightOperationServiceCrossReplicaCancelJoinsInspector(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 4, opPreflightNewInspector(true))
	env.store.joinGuard = func(string) bool {
		calls, returned, _ := env.inspectors[0].counts()
		return calls == 0 || returned
	}
	startedAt := time.Now()
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opPreflightWait(t, "inspection to start", func() bool {
		calls, _, _ := env.inspectors[0].counts()
		return calls == 1
	})
	requested, err := env.svc.Cancel(t.Context(), opPreflightID)
	if err != nil || requested == nil || requested.State != domain.ProbePreflightCancelRequested {
		t.Fatalf("cancel request operation=%+v error=%v", requested, err)
	}
	operation := env.waitTerminal(t, opPreflightID)
	if operation.State != domain.ProbePreflightCanceled || operation.ErrorCode != "canceled" {
		t.Fatalf("operation %+v", operation)
	}
	if elapsed := time.Since(startedAt); elapsed < 900*time.Millisecond {
		t.Fatalf("cancellation completed in %v; durable polling must observe intent within one second", elapsed)
	}
	if _, joined, canceled := env.inspectors[0].counts(); !joined || !canceled {
		t.Fatalf("inspector joined=%v canceled=%v, want both", joined, canceled)
	}
	if violations := env.store.violations(); violations != 0 {
		t.Fatalf("completion persisted before inspector join: %d violations", violations)
	}
	if observes := env.store.observeCalls(); observes > 3 {
		t.Fatalf("polled durable state %d times; at most once per second allowed", observes)
	}
	if cancels := env.store.cancelCalls(); cancels != 1 {
		t.Fatalf("cancel requests=%d, want 1", cancels)
	}
	samples := env.store.samples()
	if len(samples) == 0 {
		t.Fatal("final write never attempted")
	}
	opPreflightAssertWriteCtx(t, samples[len(samples)-1])
}

func TestProbePreflightOperationServiceCancelRacingPassedFinishCompletesCanceled(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 4)
	env.store.onFinish = func(operationID, outcomeState string) {
		if outcomeState != domain.ProbePreflightPassed {
			return
		}
		if _, err := env.store.RequestPreflightCancellation(context.Background(), operationID); err != nil {
			t.Errorf("racing cancellation: %v", err)
		}
	}
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	operation := env.waitTerminal(t, opPreflightID)
	if operation.State != domain.ProbePreflightCanceled {
		t.Fatalf("durable cancellation intent must fence a racing pass: %+v", operation)
	}
}

func TestProbePreflightOperationServiceRequestDisconnectDoesNotCancelWork(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 4, opPreflightNewInspector(true))
	requestCtx, disconnect := context.WithCancel(t.Context())
	if _, err := env.svc.Submit(requestCtx, opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opPreflightWait(t, "inspection to start", func() bool {
		calls, _, _ := env.inspectors[0].counts()
		return calls == 1
	})
	disconnect()
	if _, joined, canceled := env.inspectors[0].counts(); canceled || joined {
		t.Fatalf("request disconnect canceled the inspector: joined=%v canceled=%v", joined, canceled)
	}
	close(env.inspectors[0].release)
	operation := env.waitTerminal(t, opPreflightID)
	if operation.State != domain.ProbePreflightPassed {
		t.Fatalf("operation %+v", operation)
	}
	samples := env.store.samples()
	if len(samples) == 0 {
		t.Fatal("final write never attempted")
	}
	opPreflightAssertWriteCtx(t, samples[len(samples)-1])
}

func TestProbePreflightOperationServiceRootShutdownJoinsBeforeInterrupted(t *testing.T) {
	rootCtx, shutdown := context.WithCancel(context.Background())
	env := opPreflightEnvFor(t, rootCtx, 4, opPreflightNewInspector(true))
	env.store.joinGuard = func(string) bool {
		calls, returned, _ := env.inspectors[0].counts()
		return calls == 0 || returned
	}
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opPreflightWait(t, "inspection to start", func() bool {
		calls, _, _ := env.inspectors[0].counts()
		return calls == 1
	})
	shutdown()
	operation := env.waitTerminal(t, opPreflightID)
	if operation.State != domain.ProbePreflightInterrupted || operation.ErrorCode != "interrupted" {
		t.Fatalf("operation %+v", operation)
	}
	if _, joined, canceled := env.inspectors[0].counts(); !joined || !canceled {
		t.Fatalf("inspector joined=%v canceled=%v, want both", joined, canceled)
	}
	if violations := env.store.violations(); violations != 0 {
		t.Fatalf("completion persisted before inspector join: %d violations", violations)
	}
	samples := env.store.samples()
	if len(samples) == 0 {
		t.Fatal("final write never attempted")
	}
	opPreflightAssertWriteCtx(t, samples[len(samples)-1])
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightIDTwo, 7)); !errors.Is(err, domain.ErrProbePreflightClosed) {
		t.Fatalf("admission after root shutdown: %v", err)
	}
	if err := env.svc.Close(t.Context()); err != nil {
		t.Fatalf("close after shutdown: %v", err)
	}
}

func TestProbePreflightOperationServiceFailedFinishKeepsHonestRow(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 4)
	env.store.finishErr = errors.New("database unavailable")
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	opPreflightWait(t, "failed outcome attempt", func() bool {
		return env.store.finishCalls() >= 1
	})
	if operation := env.store.snapshot(opPreflightID); operation == nil || operation.Terminal() {
		t.Fatalf("failed finish must leave a nonterminal row: %+v", operation)
	}
	env.store.setFinishErr(nil)
	env.store.expire(opPreflightID)
	reconciled, err := env.svc.Observe(t.Context(), opPreflightID)
	if err != nil || reconciled == nil || reconciled.State != domain.ProbePreflightInterrupted {
		t.Fatalf("operation=%+v error=%v", reconciled, err)
	}
}

func TestProbePreflightOperationServiceFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode string
	}{
		{"trust", domain.ErrProbeSSHTrust, "ssh_trust"},
		{"wrapped_connect", fmt.Errorf("transport dial: %w", domain.ErrProbeSSHConnect), "ssh_connect"},
		{"platform", domain.ErrProbeHostPlatform, "unsupported_platform"},
		{"runtime", domain.ErrProbeHostRuntime, "missing_runtime"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"unmapped", errors.New("raw ssh library failure 0xdeadbeef"), "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inspector := opPreflightNewInspector(false)
			inspector.err = tc.err
			env := opPreflightEnvFor(t, t.Context(), 4, inspector)
			if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
				t.Fatalf("submit: %v", err)
			}
			operation := env.waitTerminal(t, opPreflightID)
			if operation.State != domain.ProbePreflightFailed || operation.ErrorCode != tc.wantCode ||
				operation.Result != nil {
				t.Fatalf("operation %+v", operation)
			}
		})
	}
}

func TestProbePreflightOperationServiceObserveAndCancelAPI(t *testing.T) {
	env := opPreflightEnvFor(t, t.Context(), 4)
	if _, err := env.svc.Observe(t.Context(), "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid identifier: %v", err)
	}
	if _, err := env.svc.Cancel(t.Context(), "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid identifier: %v", err)
	}
	if _, err := env.svc.Observe(t.Context(), opPreflightIDThree); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing operation: %v", err)
	}
	if _, err := env.svc.Cancel(t.Context(), opPreflightIDThree); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing operation: %v", err)
	}
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	env.waitTerminal(t, opPreflightID)
	unchanged, err := env.svc.Cancel(t.Context(), opPreflightID)
	if err != nil || unchanged == nil || unchanged.State != domain.ProbePreflightPassed {
		t.Fatalf("terminal receipts must never relabel: %+v error=%v", unchanged, err)
	}
	if cancels := env.store.cancelCalls(); cancels != 2 {
		t.Fatalf("cancel requests=%d, want 2 including the missing-ID attempt", cancels)
	}
	observed, err := env.svc.Observe(t.Context(), opPreflightID)
	if err != nil || observed == nil || observed.State != domain.ProbePreflightPassed {
		t.Fatalf("observed=%+v error=%v", observed, err)
	}
	observing := errors.New("read failure")
	env.store.observeErr = observing
	if _, err := env.svc.Observe(t.Context(), opPreflightID); !errors.Is(err, observing) {
		t.Fatalf("storage errors must propagate: %v", err)
	}
	requesting := errors.New("write failure")
	env.store.cancelErr = requesting
	if _, err := env.svc.Cancel(t.Context(), opPreflightID); !errors.Is(err, requesting) {
		t.Fatalf("storage errors must propagate: %v", err)
	}
}

func TestProbePreflightOperationServiceCloseAdmissionRace(t *testing.T) {
	rootCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	env := opPreflightEnvFor(t, rootCtx, probePreflightMaxCapacity, opPreflightNewInspector(true))
	if _, err := env.svc.Submit(t.Context(), opPreflightSubmission(opPreflightID, 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	var mu sync.Mutex
	succeeded := map[string]bool{opPreflightID: true}
	var wg sync.WaitGroup
	closeErr := make(chan error, 1)
	go func() {
		closeErr <- env.svc.Close(context.Background())
	}()
	for _, id := range []string{opPreflightID, opPreflightIDTwo, opPreflightIDThree} {
		for attempt := 0; attempt < 2; attempt++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				operation, err := env.svc.Submit(t.Context(), opPreflightSubmission(id, 7))
				if err != nil {
					if !errors.Is(err, domain.ErrProbePreflightClosed) {
						t.Errorf("unexpected submission error: %v", err)
					}
					return
				}
				mu.Lock()
				succeeded[operation.Identity.OperationID] = true
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	if err := <-closeErr; err != nil {
		t.Fatalf("close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(succeeded) == 0 {
		t.Fatal("no submission was admitted before shutdown")
	}
	for id := range succeeded {
		operation := env.store.snapshot(id)
		if operation != nil && operation.Terminal() {
			if operation.State != domain.ProbePreflightInterrupted {
				t.Fatalf("operation %+v", operation)
			}
			continue
		}
		env.store.expire(id)
		reconciled, err := env.svc.Observe(context.Background(), id)
		if err != nil || reconciled == nil || reconciled.State != domain.ProbePreflightInterrupted {
			t.Fatalf("operation=%+v error=%v", reconciled, err)
		}
	}
}
