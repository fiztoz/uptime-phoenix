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

const actExecToken = "phx_probe_enroll_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq-_0123"

// actExecReceipt builds a valid running receipt with steps before index
// applied and steps[index] in flight.
func actExecReceipt(t *testing.T, index int) domain.ProbeActivation {
	t.Helper()
	identity := actRunnerIdentity("11111111-1111-4111-8111-111111111111")
	base := actRunnerBaseTime()
	attempts := actRunnerAttempts(4)
	steps := make([]domain.ProbeInstallResource, 4)
	for i, spec := range identity.Steps() {
		steps[i] = domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned}
		start := base.Add(time.Duration(i+1) * time.Second)
		switch {
		case i < index:
			steps[i].State, steps[i].AttemptID = domain.ProbeInstallResourceApplied, attempts[i]
			steps[i].StartedAt, steps[i].FinishedAt = start, start.Add(500*time.Millisecond)
		case i == index:
			steps[i].State, steps[i].AttemptID, steps[i].StartedAt = domain.ProbeInstallResourceInFlight, attempts[i], start
		}
	}
	a := domain.ProbeActivation{
		Identity: identity, OwnerID: "22222222-2222-4222-8222-222222222222", State: domain.ProbeActivationRunning,
		CreatedAt: base, UpdatedAt: base.Add(10 * time.Second), DeadlineAt: base.Add(domain.ProbeActivationLifetime), Steps: steps,
	}
	if index > 0 {
		a.Facts = actRunnerFacts()
	}
	if !a.Valid() {
		t.Fatalf("fixture receipt for step %d invalid", index)
	}
	return a
}

// actReady is a readiness check that is always ready (or returns err).
type actReady struct{ err error }

func (r actReady) WaitReady(context.Context, string) error { return r.err }

type actExecHost struct {
	initOutcome  domain.ProbeInstallResourceOutcome
	initFacts    domain.ProbeActivationFacts
	initToken    string
	startOutcome domain.ProbeInstallResourceOutcome
	intents      []domain.ProbeHostActionIntent
}

func (h *actExecHost) ProbeInit(_ context.Context, intent domain.ProbeHostActionIntent) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, string) {
	h.intents = append(h.intents, intent)
	return h.initOutcome, h.initFacts, h.initToken
}

func (h *actExecHost) ServiceStart(_ context.Context, intent domain.ProbeHostActionIntent) domain.ProbeInstallResourceOutcome {
	h.intents = append(h.intents, intent)
	return h.startOutcome
}

type actExecAdmin struct {
	mu        sync.Mutex
	regErr    error
	regs      []ProbeRegistrationRequest
	enrollOp  *domain.ProbeOperation
	enrollErr error
	enrolls   []string // probeID|token|stream
}

func (a *actExecAdmin) CreateRegistration(_ context.Context, req ProbeRegistrationRequest) (*domain.Probe, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.regs = append(a.regs, req)
	if a.regErr != nil {
		return nil, a.regErr
	}
	return &domain.Probe{ID: req.ProbeID}, nil
}

func (a *actExecAdmin) EnrollSource(_ context.Context, probeID, token, streamID string) (*domain.ProbeOperation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.enrolls = append(a.enrolls, probeID+"|"+token+"|"+streamID)
	return a.enrollOp, a.enrollErr
}

var (
	actApplied    = domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}
	actNotStarted = domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
	actUnknown    = domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}
)

func actExecNew(t *testing.T, host *actExecHost, admin *actExecAdmin) *ProbeActivationExecutor {
	t.Helper()
	e, err := newProbeActivationExecutor(host, admin, actReady{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func actExecApply(t *testing.T, e *ProbeActivationExecutor, a domain.ProbeActivation, kind string) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts) {
	t.Helper()
	outcome, facts, err := e.ApplyStep(t.Context(), a, kind)
	if err != nil || !outcome.Valid() {
		t.Fatalf("%s: outcome %+v err %v", kind, outcome, err)
	}
	return outcome, facts
}

func TestProbeActivationExecutorFullSequenceHoldsTokenOnlyUntilEnrollment(t *testing.T) {
	host := &actExecHost{initOutcome: actApplied, initFacts: actRunnerFacts(), initToken: actExecToken, startOutcome: actApplied}
	admin := &actExecAdmin{enrollOp: &domain.ProbeOperation{Status: domain.ProbeOperationSucceeded}}
	e := actExecNew(t, host, admin)

	outcome, facts := actExecApply(t, e, actExecReceipt(t, 0), domain.ProbeActivationProbeInit)
	if outcome != actApplied || facts != actRunnerFacts() {
		t.Fatalf("probe_init: %+v %+v", outcome, facts)
	}
	if e.token != actExecToken {
		t.Fatal("token not held after probe_init")
	}
	outcome, facts = actExecApply(t, e, actExecReceipt(t, 1), domain.ProbeActivationRegistration)
	if outcome != actApplied || facts != (domain.ProbeActivationFacts{}) {
		t.Fatalf("registration: %+v %+v", outcome, facts)
	}
	identity := actRunnerIdentity("11111111-1111-4111-8111-111111111111")
	wantReg := ProbeRegistrationRequest{ProbeID: actRunnerFacts().ProbeID, Key: "edge-1", Name: "Edge One", Location: "Bangkok",
		Endpoint: "wss://edge.example:8443/ws/probe/v1", TLSPin: actRunnerFacts().CertificateFingerprint}
	if len(admin.regs) != 1 || admin.regs[0] != wantReg || identity.Endpoint() != wantReg.Endpoint {
		t.Fatalf("registration request %+v", admin.regs)
	}
	if outcome, _ = actExecApply(t, e, actExecReceipt(t, 2), domain.ProbeActivationServiceStart); outcome != actApplied {
		t.Fatalf("service_start: %+v", outcome)
	}
	if outcome, _ = actExecApply(t, e, actExecReceipt(t, 3), domain.ProbeActivationEnrollment); outcome != actApplied {
		t.Fatalf("enrollment: %+v", outcome)
	}
	if len(admin.enrolls) != 1 || admin.enrolls[0] != actRunnerFacts().ProbeID+"|"+actExecToken+"|"+actRunnerFacts().StreamID {
		t.Fatalf("enroll call %v", admin.enrolls)
	}
	if e.token != "" {
		t.Fatal("token retained after enrollment")
	}
	// A second enrollment attempt has no token to send.
	if outcome, _ = actExecApply(t, e, actExecReceipt(t, 3), domain.ProbeActivationEnrollment); outcome != actNotStarted || len(admin.enrolls) != 1 {
		t.Fatalf("replayed enrollment: %+v calls=%d", outcome, len(admin.enrolls))
	}
	// Host intents carry the install identity, the activation owner and the
	// step's own attempt.
	for i, intent := range host.intents {
		kind := []string{domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart}[i]
		step := actExecReceipt(t, actRunnerKindIndex(kind)).Steps[actRunnerKindIndex(kind)]
		if intent.Kind != kind || intent.Identity != identity.Install || intent.OwnerID != "22222222-2222-4222-8222-222222222222" || intent.AttemptID != step.AttemptID {
			t.Fatalf("host intent %d: %+v", i, intent)
		}
	}
}

func TestProbeActivationExecutorTokenClearedOnEveryEnrollmentOutcome(t *testing.T) {
	for name, admin := range map[string]*actExecAdmin{
		"failed exchange":    {enrollOp: &domain.ProbeOperation{Status: domain.ProbeOperationFailed}},
		"ambiguous error":    {enrollErr: errors.New("connection reset")},
		"definite refusal":   {enrollErr: ErrInvalidOperation},
		"nil operation":      {},
		"not found registry": {enrollErr: ports.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			e := actExecNew(t, &actExecHost{initOutcome: actApplied, initFacts: actRunnerFacts(), initToken: actExecToken}, admin)
			actExecApply(t, e, actExecReceipt(t, 0), domain.ProbeActivationProbeInit)
			outcome, _ := actExecApply(t, e, actExecReceipt(t, 3), domain.ProbeActivationEnrollment)
			want := actUnknown
			if admin.enrollErr != nil && probeActivationDefiniteRefusal(admin.enrollErr) {
				want = actNotStarted
			}
			if outcome != want || e.token != "" {
				t.Fatalf("outcome %+v (want %+v) token held=%v", outcome, want, e.token != "")
			}
		})
	}
	e := actExecNew(t, &actExecHost{initOutcome: actApplied, initFacts: actRunnerFacts(), initToken: actExecToken}, &actExecAdmin{})
	actExecApply(t, e, actExecReceipt(t, 0), domain.ProbeActivationProbeInit)
	e.Forget()
	if e.token != "" {
		t.Fatal("Forget kept the token")
	}
}

func TestProbeActivationExecutorProbeInitRefusesUnusableResults(t *testing.T) {
	for name, host := range map[string]*actExecHost{
		"invalid facts":       {initOutcome: actApplied, initFacts: domain.ProbeActivationFacts{ProbeID: "local"}, initToken: actExecToken},
		"invalid token":       {initOutcome: actApplied, initFacts: actRunnerFacts(), initToken: "not-a-token"},
		"empty token":         {initOutcome: actApplied, initFacts: actRunnerFacts()},
		"invalid outcome":     {initOutcome: domain.ProbeInstallResourceOutcome{State: "weird"}},
		"not started":         {initOutcome: actNotStarted, initFacts: actRunnerFacts(), initToken: actExecToken},
		"unknown with facts":  {initOutcome: actUnknown, initFacts: actRunnerFacts(), initToken: actExecToken},
		"applied, short tok":  {initOutcome: actApplied, initFacts: actRunnerFacts(), initToken: "phx_probe_enroll_short"},
		"applied, bad prefix": {initOutcome: actApplied, initFacts: actRunnerFacts(), initToken: strings.Replace(actExecToken, "phx_", "xyz_", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			e := actExecNew(t, host, &actExecAdmin{})
			outcome, facts := actExecApply(t, e, actExecReceipt(t, 0), domain.ProbeActivationProbeInit)
			if outcome == actApplied || facts != (domain.ProbeActivationFacts{}) || e.token != "" {
				t.Fatalf("outcome %+v facts %+v token held=%v", outcome, facts, e.token != "")
			}
			if host.initOutcome == actNotStarted && outcome != actNotStarted {
				t.Fatalf("not_started rewritten to %+v", outcome)
			}
		})
	}
}

func TestProbeActivationExecutorRegistrationOutcomes(t *testing.T) {
	cases := map[string]struct {
		err  error
		want domain.ProbeInstallResourceOutcome
	}{
		"created":            {nil, actApplied},
		"invalid":            {ErrInvalidRegistration, actNotStarted},
		"conflict":           {ports.ErrConflict, actNotStarted},
		"validation wrapped": {errors.Join(errors.New("x"), domain.ErrValidation), actNotStarted},
		"storage failure":    {errors.New("commit lost"), actUnknown},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := actExecNew(t, &actExecHost{}, &actExecAdmin{regErr: tc.err})
			if outcome, _ := actExecApply(t, e, actExecReceipt(t, 1), domain.ProbeActivationRegistration); outcome != tc.want {
				t.Fatalf("got %+v want %+v", outcome, tc.want)
			}
		})
	}
}

func TestProbeActivationExecutorRefusesMismatchedReceiptsWithoutWork(t *testing.T) {
	host := &actExecHost{initOutcome: actApplied, initFacts: actRunnerFacts(), initToken: actExecToken, startOutcome: actApplied}
	admin := &actExecAdmin{enrollOp: &domain.ProbeOperation{Status: domain.ProbeOperationSucceeded}}
	e := actExecNew(t, host, admin)
	for name, tc := range map[string]struct {
		receipt domain.ProbeActivation
		kind    string
	}{
		"kind not in flight":   {actExecReceipt(t, 0), domain.ProbeActivationRegistration},
		"later kind requested": {actExecReceipt(t, 1), domain.ProbeActivationEnrollment},
		"unknown kind":         {actExecReceipt(t, 0), "data_directory"},
		"not running": {func() domain.ProbeActivation {
			a := actExecReceipt(t, 0)
			a.State = domain.ProbeActivationCancelRequested
			return a
		}(), domain.ProbeActivationProbeInit},
		"invalid receipt": {func() domain.ProbeActivation {
			a := actExecReceipt(t, 0)
			a.OwnerID = "bad"
			return a
		}(), domain.ProbeActivationProbeInit},
	} {
		outcome, facts, err := e.ApplyStep(t.Context(), tc.receipt, tc.kind)
		if err != nil || outcome != actNotStarted || facts != (domain.ProbeActivationFacts{}) {
			t.Fatalf("%s: %+v %+v %v", name, outcome, facts, err)
		}
	}
	if len(host.intents) != 0 || len(admin.regs) != 0 || len(admin.enrolls) != 0 {
		t.Fatal("mismatched receipts reached the host or hub")
	}
}

func TestProbeActivationExecutorConstructorAndNilSafety(t *testing.T) {
	var typedHost *actExecHost
	if _, err := newProbeActivationExecutor(nil, &actExecAdmin{}, actReady{}); err == nil {
		t.Fatal("nil host accepted")
	}
	if _, err := newProbeActivationExecutor(typedHost, &actExecAdmin{}, actReady{}); err == nil {
		t.Fatal("typed-nil host accepted")
	}
	if _, err := newProbeActivationExecutor(&actExecHost{}, nil, actReady{}); err == nil {
		t.Fatal("nil admin accepted")
	}
	if _, err := NewProbeActivationExecutor(&actExecHost{}, nil, actReady{}); err == nil {
		t.Fatal("nil admin service accepted")
	}
	var e *ProbeActivationExecutor
	e.Forget()
	if outcome, _, err := e.ApplyStep(t.Context(), actExecReceipt(t, 0), domain.ProbeActivationProbeInit); err != nil || outcome != actNotStarted {
		t.Fatalf("nil executor: %+v %v", outcome, err)
	}
}

// A started probe that never accepts connections never receives the token:
// enrollment is a definite not_started and the token is still cleared.
func TestProbeActivationExecutorEnrollmentWaitsForReadinessBeforeSendingToken(t *testing.T) {
	host := &actExecHost{initOutcome: actApplied, initFacts: actRunnerFacts(), initToken: actExecToken}
	admin := &actExecAdmin{enrollOp: &domain.ProbeOperation{Status: domain.ProbeOperationSucceeded}}
	e, err := newProbeActivationExecutor(host, admin, actReady{err: context.DeadlineExceeded})
	if err != nil {
		t.Fatal(err)
	}
	actExecApply(t, e, actExecReceipt(t, 0), domain.ProbeActivationProbeInit)
	if outcome, _ := actExecApply(t, e, actExecReceipt(t, 3), domain.ProbeActivationEnrollment); outcome != actNotStarted {
		t.Fatalf("unready enrollment: %+v", outcome)
	}
	if len(admin.enrolls) != 0 || e.token != "" {
		t.Fatalf("token sent=%d held=%v", len(admin.enrolls), e.token != "")
	}
	if _, err := newProbeActivationExecutor(host, admin, nil); err == nil {
		t.Fatal("nil readiness accepted")
	}
}
