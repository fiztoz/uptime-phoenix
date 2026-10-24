package services

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	cleanupTestOwner   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	cleanupTestID      = "c1111111-1111-4111-8111-111111111111"
	cleanupTestIDTwo   = "c2222222-2222-4222-8222-222222222222"
	cleanupTestKeyText = "-----BEGIN OPENSSH PRIVATE KEY-----\ncleanup-test\n-----END OPENSSH PRIVATE KEY-----\n"
)

func cleanupTestInstall(state string) *domain.ProbeInstallOperation {
	request := opInstallSubmission(opInstallID, 7)
	return &domain.ProbeInstallOperation{Identity: opInstallIdentityFor(request, opInstallFingerprint(0x33)), State: state}
}

func cleanupTestSubmission(cleanupID string) ProbeCleanupSubmission {
	return ProbeCleanupSubmission{CleanupID: cleanupID, OperationID: opInstallID, PrivateKey: []byte(cleanupTestKeyText), RequestedBy: 9}
}

// cleanupInstalls is the install read the service performs.
type cleanupInstalls struct {
	mu        sync.Mutex
	operation *domain.ProbeInstallOperation
	err       error
	calls     int
}

func (c *cleanupInstalls) Observe(_ context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	if c.operation == nil || c.operation.Identity.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	copied := *c.operation
	return &copied, nil
}

// cleanupStore is an effectful in-memory store with the port's transition
// rules, so tests assert persisted receipts.
type cleanupStore struct {
	mu         sync.Mutex
	rows       map[string]*domain.ProbeCleanup
	createErr  error
	beginErr   error
	recordErr  error
	creates    int
	begins     int
	records    []domain.ProbeInstallResourceOutcome
	abandons   int
	recordCtxs []error
}

func newCleanupStore() *cleanupStore { return &cleanupStore{rows: map[string]*domain.ProbeCleanup{}} }

func (s *cleanupStore) row(id string) *domain.ProbeCleanup {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[id]; ok {
		copied := *r
		return &copied
	}
	return nil
}

func (s *cleanupStore) CreateCleanup(_ context.Context, identity domain.ProbeCleanupIdentity, ownerID string) (*domain.ProbeCleanup, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if s.createErr != nil {
		return nil, false, s.createErr
	}
	if r, ok := s.rows[identity.CleanupID]; ok {
		if r.Identity != identity {
			return nil, false, ports.ErrConflict
		}
		copied := *r
		return &copied, false, nil
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := &domain.ProbeCleanup{Identity: identity, OwnerID: ownerID, State: domain.ProbeCleanupQueued, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(domain.ProbeCleanupLifetime)}
	s.rows[identity.CleanupID] = r
	copied := *r
	return &copied, true, nil
}

func (s *cleanupStore) ObserveCleanup(_ context.Context, id string) (*domain.ProbeCleanup, error) {
	if r := s.row(id); r != nil {
		return r, nil
	}
	return nil, ports.ErrNotFound
}

func (s *cleanupStore) LatestCleanup(_ context.Context, operationID string) (*domain.ProbeCleanup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *domain.ProbeCleanup
	for _, r := range s.rows {
		if r.Identity.Install.OperationID == operationID && (latest == nil || r.CreatedAt.After(latest.CreatedAt)) {
			latest = r
		}
	}
	if latest == nil {
		return nil, ports.ErrNotFound
	}
	copied := *latest
	return &copied, nil
}

func (s *cleanupStore) BeginCleanup(ctx context.Context, id, ownerID, attemptID string) (*domain.ProbeCleanup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begins++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	r, ok := s.rows[id]
	if !ok || r.OwnerID != ownerID || r.State != domain.ProbeCleanupQueued {
		return nil, ports.ErrConflict
	}
	r.State, r.AttemptID = domain.ProbeCleanupRunning, attemptID
	copied := *r
	return &copied, nil
}

func (s *cleanupStore) RecordCleanup(ctx context.Context, id, ownerID, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeCleanup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, outcome)
	s.recordCtxs = append(s.recordCtxs, ctx.Err())
	if s.recordErr != nil {
		return nil, s.recordErr
	}
	r, ok := s.rows[id]
	if !ok || r.OwnerID != ownerID || r.AttemptID != attemptID || r.State != domain.ProbeCleanupRunning {
		return nil, ports.ErrConflict
	}
	switch outcome.State {
	case domain.ProbeInstallResourceApplied:
		r.State = domain.ProbeCleanupCleaned
	case domain.ProbeInstallResourceNotStarted:
		r.State, r.ErrorCode = domain.ProbeCleanupFailed, "not_started"
	default:
		r.State, r.ErrorCode = domain.ProbeCleanupInterrupted, "outcome_unknown"
	}
	copied := *r
	return &copied, nil
}

func (s *cleanupStore) AbandonCleanup(_ context.Context, id, ownerID string) (*domain.ProbeCleanup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abandons++
	r, ok := s.rows[id]
	if !ok || r.OwnerID != ownerID || r.State != domain.ProbeCleanupQueued {
		return nil, ports.ErrConflict
	}
	r.State, r.ErrorCode = domain.ProbeCleanupInterrupted, "owner_interrupted"
	copied := *r
	return &copied, nil
}

// cleanupSessions is the fake factory and session.
type cleanupSessions struct {
	mu       sync.Mutex
	err      error
	hostFPs  []string
	keys     []string
	intents  []domain.ProbeHostActionIntent
	outcome  domain.ProbeInstallResourceOutcome
	hook     func(ctx context.Context)
	finished chan struct{}
}

func newCleanupSessions() *cleanupSessions {
	return &cleanupSessions{outcome: domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, finished: make(chan struct{}, 8)}
}

func (f *cleanupSessions) NewCleanupSession(_ context.Context, privateKey []byte, hostFingerprint string) (ports.ProbeCleanupHostSession, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, string(privateKey))
	f.hostFPs = append(f.hostFPs, hostFingerprint)
	if f.err != nil {
		return nil, "", f.err
	}
	return f, opInstallFingerprint(0x44), nil
}

func (f *cleanupSessions) Cleanup(ctx context.Context, intent domain.ProbeHostActionIntent) domain.ProbeInstallResourceOutcome {
	f.mu.Lock()
	f.intents = append(f.intents, intent)
	hook, outcome := f.hook, f.outcome
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	f.finished <- struct{}{}
	return outcome
}

func (f *cleanupSessions) intentList() []domain.ProbeHostActionIntent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ProbeHostActionIntent(nil), f.intents...)
}

type cleanupEnv struct {
	svc      *ProbeCleanupOperationService
	store    *cleanupStore
	installs *cleanupInstalls
	sessions *cleanupSessions
}

func cleanupEnvFor(t *testing.T, ctx context.Context, capacity int, installState string) *cleanupEnv {
	t.Helper()
	env := &cleanupEnv{store: newCleanupStore(), installs: &cleanupInstalls{operation: cleanupTestInstall(installState)}, sessions: newCleanupSessions()}
	svc, err := newProbeCleanupOperationService(ctx, env.store, env.installs, env.sessions, cleanupTestOwner, capacity)
	if err != nil {
		t.Fatal(err)
	}
	env.svc = svc
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	return env
}

// await waits until the stored cleanup reaches a terminal state.
func (e *cleanupEnv) await(t *testing.T, id string) *domain.ProbeCleanup {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r := e.store.row(id); r != nil && r.Terminal() {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup %s never settled: %+v", id, e.store.row(id))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProbeCleanupOperationServiceConstructorRefusals(t *testing.T) {
	store, installs, sessions := newCleanupStore(), &cleanupInstalls{}, newCleanupSessions()
	var nilStore *cleanupStore
	for name, build := range map[string]func() (*ProbeCleanupOperationService, error){
		"nil ctx": func() (*ProbeCleanupOperationService, error) {
			return newProbeCleanupOperationService(nil, store, installs, sessions, cleanupTestOwner, 1)
		}, //nolint:staticcheck // nil ctx is the refused input
		"typed nil": func() (*ProbeCleanupOperationService, error) {
			return newProbeCleanupOperationService(t.Context(), nilStore, installs, sessions, cleanupTestOwner, 1)
		},
		"nil installs": func() (*ProbeCleanupOperationService, error) {
			return newProbeCleanupOperationService(t.Context(), store, nil, sessions, cleanupTestOwner, 1)
		},
		"nil sessions": func() (*ProbeCleanupOperationService, error) {
			return newProbeCleanupOperationService(t.Context(), store, installs, nil, cleanupTestOwner, 1)
		},
		"bad owner": func() (*ProbeCleanupOperationService, error) {
			return newProbeCleanupOperationService(t.Context(), store, installs, sessions, "owner", 1)
		},
		"zero capacity": func() (*ProbeCleanupOperationService, error) {
			return newProbeCleanupOperationService(t.Context(), store, installs, sessions, cleanupTestOwner, 0)
		},
		"capacity five": func() (*ProbeCleanupOperationService, error) {
			return newProbeCleanupOperationService(t.Context(), store, installs, sessions, cleanupTestOwner, 5)
		},
		"exported nil journal": func() (*ProbeCleanupOperationService, error) {
			return NewProbeCleanupOperationService(t.Context(), store, nil, sessions, cleanupTestOwner, 1)
		},
	} {
		if svc, err := build(); svc != nil || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: svc=%v err=%v", name, svc, err)
		}
	}
}

func TestProbeCleanupOperationServiceHappyPathCleansWithTheInstallsPinnedHost(t *testing.T) {
	env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallInterrupted)
	receipt, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID))
	if err != nil || receipt == nil || receipt.State != domain.ProbeCleanupQueued {
		t.Fatalf("submit: %+v %v", receipt, err)
	}
	final := env.await(t, cleanupTestID)
	if final.State != domain.ProbeCleanupCleaned || final.ErrorCode != "" {
		t.Fatalf("final %+v", final)
	}
	install := env.installs.operation.Identity
	if fps := env.sessions.hostFPs; len(fps) != 1 || fps[0] != install.HostKeyFingerprint {
		t.Fatalf("session host pins %v, want only the install's %q", fps, install.HostKeyFingerprint)
	}
	intents := env.sessions.intentList()
	if len(intents) != 1 {
		t.Fatalf("intents %+v", intents)
	}
	want := final.Identity.Intent(cleanupTestOwner, final.AttemptID)
	if intents[0] != want || intents[0].Kind != domain.ProbeHostActionCleanup || intents[0].Identity != install || !intents[0].Valid() {
		t.Fatalf("intent %+v, want %+v", intents[0], want)
	}
	if final.Identity.ClientKeyFingerprint != opInstallFingerprint(0x44) || final.Identity.RequestedBy != 9 {
		t.Fatalf("identity %+v", final.Identity)
	}
	if latest, err := env.svc.Latest(t.Context(), opInstallID); err != nil || latest.Identity.CleanupID != cleanupTestID {
		t.Fatalf("latest %+v %v", latest, err)
	}
}

func TestProbeCleanupOperationServiceRecordsEveryOutcomeHonestly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome domain.ProbeInstallResourceOutcome
		state   string
		code    string
	}{
		{"not started", domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}, domain.ProbeCleanupFailed, "not_started"},
		{"unknown", domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}, domain.ProbeCleanupInterrupted, "outcome_unknown"},
		{"invalid report is unknown", domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied, ErrorCode: "x"}, domain.ProbeCleanupInterrupted, "outcome_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallResourcesReady)
			env.sessions.outcome = tc.outcome
			if _, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID)); err != nil {
				t.Fatal(err)
			}
			if final := env.await(t, cleanupTestID); final.State != tc.state || final.ErrorCode != tc.code {
				t.Fatalf("final %s/%s, want %s/%s", final.State, final.ErrorCode, tc.state, tc.code)
			}
		})
	}
}

func TestProbeCleanupOperationServiceRefusalsCreateNothing(t *testing.T) {
	canary := errors.New("canary /root/.ssh/id_ed25519")
	for _, tc := range []struct {
		name  string
		setup func(e *cleanupEnv)
		want  error
	}{
		{"missing install", func(e *cleanupEnv) { e.installs.operation = nil }, ports.ErrNotFound},
		{"install lookup failure", func(e *cleanupEnv) { e.installs.err = canary }, canary},
		{"running install", func(e *cleanupEnv) { e.installs.operation.State = domain.ProbeInstallRunning }, domain.ErrProbeCleanupIneligible},
		{"queued install", func(e *cleanupEnv) { e.installs.operation.State = domain.ProbeInstallQueued }, domain.ErrProbeCleanupIneligible},
		{"cancel requested install", func(e *cleanupEnv) { e.installs.operation.State = domain.ProbeInstallCancelRequested }, domain.ErrProbeCleanupIneligible},
		{"bad key", func(e *cleanupEnv) { e.sessions.err = domain.ErrProbeSSHInput }, domain.ErrProbeSSHInput},
		{"session leak is internal", func(e *cleanupEnv) { e.sessions.err = canary }, domain.ErrInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallFailed)
			tc.setup(env)
			receipt, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID))
			if receipt != nil || !errors.Is(err, tc.want) {
				t.Fatalf("receipt=%+v err=%v, want %v", receipt, err, tc.want)
			}
			if tc.want != canary && strings.Contains(err.Error(), "canary") {
				t.Fatalf("leaked %q", err)
			}
			if env.store.creates != 0 || env.store.begins != 0 || len(env.sessions.intentList()) != 0 {
				t.Fatalf("creates=%d begins=%d intents=%d", env.store.creates, env.store.begins, len(env.sessions.intentList()))
			}
		})
	}
	t.Run("store ineligibility passes through", func(t *testing.T) {
		env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallFailed)
		env.store.createErr = domain.ErrProbeCleanupIneligible
		if _, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID)); !errors.Is(err, domain.ErrProbeCleanupIneligible) {
			t.Fatalf("err %v", err)
		}
		if env.store.begins != 0 || len(env.sessions.intentList()) != 0 {
			t.Fatal("dispatched after a refused create")
		}
	})
	t.Run("invalid submission does no I/O", func(t *testing.T) {
		env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallFailed)
		for _, bad := range []ProbeCleanupSubmission{
			{CleanupID: "x", OperationID: opInstallID, PrivateKey: []byte("k"), RequestedBy: 1},
			{CleanupID: cleanupTestID, OperationID: "x", PrivateKey: []byte("k"), RequestedBy: 1},
			{CleanupID: cleanupTestID, OperationID: opInstallID, RequestedBy: 1},
			{CleanupID: cleanupTestID, OperationID: opInstallID, PrivateKey: []byte("k")},
			{CleanupID: cleanupTestID, OperationID: opInstallID, PrivateKey: make([]byte, domain.MaxProbeInstallPrivateKeySize+1), RequestedBy: 1},
		} {
			if _, err := env.svc.Submit(t.Context(), bad); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("%+v: err %v", bad, err)
			}
		}
		if env.installs.calls != 0 || len(env.sessions.keys) != 0 {
			t.Fatal("invalid submission reached I/O")
		}
	})
}

func TestProbeCleanupOperationServiceExactRetryNeverDispatchesTwice(t *testing.T) {
	env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallCanceled)
	if _, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID)); err != nil {
		t.Fatal(err)
	}
	first := env.await(t, cleanupTestID)
	retry, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID))
	if err != nil || retry == nil || *retry != *first {
		t.Fatalf("retry %+v %v, want %+v", retry, err, first)
	}
	if env.store.begins != 1 || len(env.sessions.intentList()) != 1 {
		t.Fatalf("retry dispatched again: begins=%d intents=%d", env.store.begins, len(env.sessions.intentList()))
	}
}

func TestProbeCleanupOperationServiceBeginFailureAbandonsWithoutHostWork(t *testing.T) {
	env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallInterrupted)
	env.store.beginErr = errors.New("lost connection")
	if _, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID)); err != nil {
		t.Fatal(err)
	}
	final := env.await(t, cleanupTestID)
	if final.State != domain.ProbeCleanupInterrupted || final.ErrorCode != "owner_interrupted" || final.AttemptID != "" {
		t.Fatalf("final %+v", final)
	}
	if len(env.sessions.intentList()) != 0 || env.store.abandons != 1 {
		t.Fatalf("intents=%d abandons=%d", len(env.sessions.intentList()), env.store.abandons)
	}
}

func TestProbeCleanupOperationServiceCloseRecordsTheJoinedOutcome(t *testing.T) {
	env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallResourcesReady)
	entered := make(chan struct{})
	env.sessions.hook = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
	}
	env.sessions.outcome = domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}
	if _, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID)); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := env.svc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	final := env.store.row(cleanupTestID)
	if final == nil || final.State != domain.ProbeCleanupInterrupted || final.ErrorCode != "outcome_unknown" {
		t.Fatalf("Close returned before the outcome was recorded: %+v", final)
	}
	if len(env.store.recordCtxs) != 1 || env.store.recordCtxs[0] != nil {
		t.Fatalf("record ran under a canceled context: %v", env.store.recordCtxs)
	}
	if _, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestIDTwo)); !errors.Is(err, domain.ErrProbeInstallClosed) {
		t.Fatalf("submit after Close: %v", err)
	}
}

func TestProbeCleanupOperationServiceSaturationAnswersOnlyExactRetry(t *testing.T) {
	env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallResourcesReady)
	release := make(chan struct{})
	env.sessions.hook = func(context.Context) { <-release }
	if _, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestIDTwo)); !errors.Is(err, domain.ErrProbeInstallBusy) {
		t.Fatalf("second cleanup while saturated: %v", err)
	}
	retry, err := env.svc.Submit(t.Context(), cleanupTestSubmission(cleanupTestID))
	if err != nil || retry == nil || retry.Identity.CleanupID != cleanupTestID {
		t.Fatalf("saturated exact retry %+v %v", retry, err)
	}
	close(release)
	env.await(t, cleanupTestID)
	if env.store.creates != 1 {
		t.Fatalf("creates=%d, want only the first", env.store.creates)
	}
}

func TestProbeCleanupOperationServiceReadsValidateIDs(t *testing.T) {
	env := cleanupEnvFor(t, t.Context(), 1, domain.ProbeInstallResourcesReady)
	if _, err := env.svc.Observe(t.Context(), "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
	if _, err := env.svc.Latest(t.Context(), "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
	if _, err := env.svc.Latest(t.Context(), opInstallID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("latest without cleanups: %v", err)
	}
}
