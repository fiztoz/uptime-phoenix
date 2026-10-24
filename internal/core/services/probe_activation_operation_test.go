package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	actOpID    = "11111111-1111-4111-8111-111111111111"
	actOpOwner = "33333333-3333-4333-8333-333333333333"
)

type actOpInstalls struct {
	op  *domain.ProbeInstallOperation
	err error
}

func (f *actOpInstalls) Observe(context.Context, string) (*domain.ProbeInstallOperation, error) {
	return f.op, f.err
}

type actOpRepo struct {
	mu         sync.Mutex
	created    []domain.ProbeActivationIdentity
	createOut  *domain.ProbeActivation
	createNew  bool
	createErr  error
	observeOut *domain.ProbeActivation
	observeErr error
	cancels    int
	cancelOut  *domain.ProbeActivation
}

func (r *actOpRepo) CreateActivation(_ context.Context, identity domain.ProbeActivationIdentity, owner string) (*domain.ProbeActivation, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created = append(r.created, identity)
	if r.createErr != nil {
		return nil, false, r.createErr
	}
	out := *r.createOut
	out.Identity = identity
	if r.createNew {
		out.OwnerID = owner
	}
	return &out, r.createNew, nil
}

func (r *actOpRepo) ObserveActivation(context.Context, string) (*domain.ProbeActivation, error) {
	if r.observeOut == nil && r.observeErr == nil {
		return nil, ports.ErrNotFound // like the real store for an unknown ID
	}
	return r.observeOut, r.observeErr
}

func (r *actOpRepo) BeginActivation(context.Context, string, string) (*domain.ProbeActivation, error) {
	return nil, domain.ErrInternal
}

func (r *actOpRepo) RequestActivationCancellation(context.Context, string) (*domain.ProbeActivation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels++
	return r.cancelOut, nil
}

func (r *actOpRepo) FinishActivation(context.Context, string, string, domain.ProbeInstallOutcome) (*domain.ProbeActivation, error) {
	return nil, domain.ErrInternal
}

type actOpSessions struct {
	calls   int
	hostPin string
	err     error
}

func (s *actOpSessions) NewActivationSession(_ context.Context, key []byte, hostPin string) (ports.ProbeActivationHostSession, string, error) {
	s.calls++
	s.hostPin = hostPin
	if s.err != nil {
		return nil, "", s.err
	}
	return &actExecHost{}, actRunnerFingerprint(0x09), nil
}

type actOpHarness struct {
	svc      *ProbeActivationOperationService
	repo     *actOpRepo
	installs *actOpInstalls
	sessions *actOpSessions
	runs     chan domain.ProbeActivationIdentity
	release  chan struct{}
	executor chan *ProbeActivationExecutor
}

func newActOpHarness(t *testing.T, capacity int) *actOpHarness {
	t.Helper()
	install := actRunnerIdentity(actOpID).Install
	h := &actOpHarness{
		repo:     &actOpRepo{createNew: true, createOut: &domain.ProbeActivation{State: domain.ProbeActivationQueued}},
		installs: &actOpInstalls{op: &domain.ProbeInstallOperation{Identity: install, State: domain.ProbeInstallResourcesReady}},
		sessions: &actOpSessions{},
		runs:     make(chan domain.ProbeActivationIdentity, 4),
		release:  make(chan struct{}),
		executor: make(chan *ProbeActivationExecutor, 4),
	}
	run := func(ctx context.Context, executor ports.ProbeActivationStepExecutor, identity domain.ProbeActivationIdentity, owner string, attempts []string) (*domain.ProbeActivation, error) {
		if owner != actOpOwner || len(attempts) != 4 {
			t.Errorf("run owner=%q attempts=%d", owner, len(attempts))
		}
		e := executor.(*ProbeActivationExecutor)
		e.mu.Lock()
		e.token = "held"
		e.mu.Unlock()
		h.executor <- e
		h.runs <- identity
		select {
		case <-h.release:
		case <-ctx.Done():
		}
		return nil, nil
	}
	svc, err := newProbeActivationOperationService(t.Context(), h.repo, h.installs, h.sessions, &actExecAdmin{}, run, actOpOwner, capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	h.svc = svc
	return h
}

func actOpSubmission() ProbeActivationSubmission {
	return ProbeActivationSubmission{OperationID: actOpID, PrivateKey: []byte("key"), ProbeKey: "edge-1", Name: "Edge One", Location: "Bangkok", RequestedBy: 7}
}

func TestProbeActivationOperationServiceDispatchesOnceFromStoredInstall(t *testing.T) {
	h := newActOpHarness(t, 1)
	got, err := h.svc.Submit(t.Context(), actOpSubmission())
	if err != nil || got == nil {
		t.Fatalf("submit: %v", err)
	}
	identity := <-h.runs
	install := actRunnerIdentity(actOpID).Install
	if identity.Install != install || identity.ListenPort != domain.ProbeActivationDefaultPort ||
		identity.ClientKeyFingerprint != actRunnerFingerprint(0x09) || identity.RequestedBy != 7 || identity.ProbeKey != "edge-1" {
		t.Fatalf("identity %+v", identity)
	}
	if h.sessions.hostPin != install.HostKeyFingerprint {
		t.Fatalf("session used host pin %q, not the stored install's", h.sessions.hostPin)
	}
	// Capacity 1 is held by the running activation.
	if _, err := h.svc.Submit(t.Context(), func() ProbeActivationSubmission {
		s := actOpSubmission()
		s.OperationID = "44444444-4444-4444-8444-444444444444"
		return s
	}()); !errors.Is(err, domain.ErrProbeInstallBusy) {
		t.Fatalf("busy: %v", err)
	}
	executor := <-h.executor
	close(h.release)
	if err := h.svc.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	executor.mu.Lock()
	held := executor.token
	executor.mu.Unlock()
	if held != "" {
		t.Fatal("token outlived the run")
	}
}

func TestProbeActivationOperationServiceRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*actOpHarness, *ProbeActivationSubmission)
		want   error
	}{
		"bad id":          {func(_ *actOpHarness, s *ProbeActivationSubmission) { s.OperationID = "x" }, domain.ErrValidation},
		"empty key":       {func(_ *actOpHarness, s *ProbeActivationSubmission) { s.PrivateKey = nil }, domain.ErrValidation},
		"no actor":        {func(_ *actOpHarness, s *ProbeActivationSubmission) { s.RequestedBy = 0 }, domain.ErrValidation},
		"bad probe key":   {func(_ *actOpHarness, s *ProbeActivationSubmission) { s.ProbeKey = "Local" }, domain.ErrValidation},
		"reserved key":    {func(_ *actOpHarness, s *ProbeActivationSubmission) { s.ProbeKey = "local" }, domain.ErrValidation},
		"privileged port": {func(_ *actOpHarness, s *ProbeActivationSubmission) { s.ListenPort = 443 }, domain.ErrValidation},
		"install missing": {func(h *actOpHarness, _ *ProbeActivationSubmission) { h.installs.err = ports.ErrNotFound }, ports.ErrNotFound},
		"install not ready": {func(h *actOpHarness, _ *ProbeActivationSubmission) {
			h.installs.op.State = domain.ProbeInstallRunning
		}, ports.ErrConflict},
		"session refused": {func(h *actOpHarness, _ *ProbeActivationSubmission) { h.sessions.err = domain.ErrProbeSSHInput }, domain.ErrProbeSSHInput},
		"session canary": {func(h *actOpHarness, _ *ProbeActivationSubmission) {
			h.sessions.err = errors.New("sensitive-canary")
		}, domain.ErrInternal},
		"create conflict": {func(h *actOpHarness, _ *ProbeActivationSubmission) { h.repo.createErr = ports.ErrConflict }, ports.ErrConflict},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newActOpHarness(t, 2)
			s := actOpSubmission()
			tc.mutate(h, &s)
			got, err := h.svc.Submit(t.Context(), s)
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("got %v err %v, want %v", got, err, tc.want)
			}
			if err != nil && err.Error() == "sensitive-canary" {
				t.Fatal("raw session error leaked")
			}
			select {
			case <-h.runs:
				t.Fatal("refused submission dispatched a run")
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestProbeActivationOperationServiceExactRetryNeverDispatches(t *testing.T) {
	h := newActOpHarness(t, 2)
	h.repo.createNew = false
	h.repo.createOut = &domain.ProbeActivation{OwnerID: "55555555-5555-4555-8555-555555555555", State: domain.ProbeActivationActive}
	got, err := h.svc.Submit(t.Context(), actOpSubmission())
	if err != nil || got.State != domain.ProbeActivationActive || got.OwnerID == actOpOwner {
		t.Fatalf("retry: %+v %v", got, err)
	}
	select {
	case <-h.runs:
		t.Fatal("exact retry dispatched")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestProbeActivationOperationServiceCancelLeavesTerminalUnchanged(t *testing.T) {
	h := newActOpHarness(t, 2)
	active := &domain.ProbeActivation{State: domain.ProbeActivationActive}
	h.repo.observeOut = active
	if got, err := h.svc.Cancel(t.Context(), actOpID); err != nil || got != active || h.repo.cancels != 0 {
		t.Fatalf("cancel active: %v %v calls=%d", got, err, h.repo.cancels)
	}
	running := &domain.ProbeActivation{State: domain.ProbeActivationRunning}
	requested := &domain.ProbeActivation{State: domain.ProbeActivationCancelRequested}
	h.repo.observeOut, h.repo.cancelOut = running, requested
	if got, err := h.svc.Cancel(t.Context(), actOpID); err != nil || got != requested || h.repo.cancels != 1 {
		t.Fatalf("cancel running: %v %v calls=%d", got, err, h.repo.cancels)
	}
	h.repo.observeErr = ports.ErrNotFound
	if _, err := h.svc.Cancel(t.Context(), actOpID); !errors.Is(err, ports.ErrNotFound) || h.repo.cancels != 1 {
		t.Fatalf("cancel missing: %v calls=%d", err, h.repo.cancels)
	}
	if _, err := h.svc.Observe(t.Context(), "bad"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("observe bad id: %v", err)
	}
}

func TestProbeActivationOperationServiceCloseStopsAdmissionAndJoins(t *testing.T) {
	h := newActOpHarness(t, 2)
	if _, err := h.svc.Submit(t.Context(), actOpSubmission()); err != nil {
		t.Fatal(err)
	}
	<-h.runs
	done := make(chan error, 1)
	go func() { done <- h.svc.Close(context.Background()) }()
	select {
	case err := <-done:
		// The fake run returns on root cancellation, so Close joins it.
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join the run")
	}
	if _, err := h.svc.Submit(t.Context(), actOpSubmission()); !errors.Is(err, domain.ErrProbeInstallClosed) {
		t.Fatalf("submit after close: %v", err)
	}
}
