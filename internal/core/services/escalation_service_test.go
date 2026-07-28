package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- fakes -----------------------------------------------------------------
//
// The monitor and group fakes embed the port interface rather than stubbing
// every method. An unimplemented method therefore PANICS when called instead of
// returning a zero value that looks like success — if this suite ever starts
// exercising a method nobody thought about, it fails loudly (AGENTS.md rule 7).

type escFakeMonitorRepo struct {
	ports.MonitorRepository
	byID map[int64]*domain.Monitor
}

func (r *escFakeMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

type escFakeGroupRepo struct {
	ports.MonitorGroupRepository
	byID map[int64]*domain.MonitorGroup
}

func (r *escFakeGroupRepo) GetByID(_ context.Context, id int64) (*domain.MonitorGroup, error) {
	g, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *g
	return &cp, nil
}

type escFakePolicyRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.EscalationPolicy
	nextID int64
}

func newEscFakePolicyRepo() *escFakePolicyRepo {
	return &escFakePolicyRepo{byID: make(map[int64]*domain.EscalationPolicy), nextID: 1}
}

func (r *escFakePolicyRepo) clone(p *domain.EscalationPolicy) *domain.EscalationPolicy {
	cp := *p
	cp.Steps = make([]domain.EscalationStep, len(p.Steps))
	for i, st := range p.Steps {
		cp.Steps[i] = st
		cp.Steps[i].NotificationIDs = append([]int64(nil), st.NotificationIDs...)
	}
	return &cp
}

func (r *escFakePolicyRepo) Create(_ context.Context, p *domain.EscalationPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.ID = r.nextID
	r.nextID++
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	r.byID[p.ID] = r.clone(p)
	return nil
}

func (r *escFakePolicyRepo) Update(_ context.Context, p *domain.EscalationPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[p.ID]; !ok {
		return ports.ErrNotFound
	}
	p.UpdatedAt = time.Now().UTC()
	r.byID[p.ID] = r.clone(p)
	return nil
}

func (r *escFakePolicyRepo) GetByID(_ context.Context, id int64) (*domain.EscalationPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return r.clone(p), nil
}

func (r *escFakePolicyRepo) List(_ context.Context) ([]*domain.EscalationPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.EscalationPolicy, 0, len(r.byID))
	for _, p := range r.byID {
		out = append(out, r.clone(p))
	}
	return out, nil
}

func (r *escFakePolicyRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

// seed inserts a ready-made policy and returns it.
func (r *escFakePolicyRepo) seed(enabled bool, steps ...domain.EscalationStep) *domain.EscalationPolicy {
	p := &domain.EscalationPolicy{Name: "p", Enabled: enabled, Steps: steps}
	_ = r.Create(context.Background(), p)
	return p
}

type escFakeAssignRepo struct {
	mu       sync.Mutex
	monitors map[int64]int64
	groups   map[int64]int64
}

func newEscFakeAssignRepo() *escFakeAssignRepo {
	return &escFakeAssignRepo{monitors: map[int64]int64{}, groups: map[int64]int64{}}
}

func (r *escFakeAssignRepo) AssignMonitor(_ context.Context, monitorID, policyID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.monitors[monitorID] = policyID
	return nil
}

func (r *escFakeAssignRepo) UnassignMonitor(_ context.Context, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.monitors, monitorID)
	return nil
}

func (r *escFakeAssignRepo) PolicyIDForMonitor(_ context.Context, monitorID int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.monitors[monitorID]
	if !ok {
		return 0, ports.ErrNotFound
	}
	return id, nil
}

func (r *escFakeAssignRepo) AssignGroup(_ context.Context, groupID, policyID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groups[groupID] = policyID
	return nil
}

func (r *escFakeAssignRepo) UnassignGroup(_ context.Context, groupID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.groups, groupID)
	return nil
}

func (r *escFakeAssignRepo) PolicyIDForGroup(_ context.Context, groupID int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.groups[groupID]
	if !ok {
		return 0, ports.ErrNotFound
	}
	return id, nil
}

func (r *escFakeAssignRepo) ListMonitorsByPolicy(_ context.Context, policyID int64) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, 0)
	for monitorID, pid := range r.monitors {
		if pid == policyID {
			out = append(out, monitorID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (r *escFakeAssignRepo) ListGroupsByPolicy(_ context.Context, policyID int64) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, 0)
	for groupID, pid := range r.groups {
		if pid == policyID {
			out = append(out, groupID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// escFakeStateRepo implements the compare-and-set lease for real. A fake that
// simply handed every caller the same rows would make the multi-worker test
// prove nothing.
type escFakeStateRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.AlertEscalation
	nextID int64

	// createdNextRunAt records what Create was handed, so a test can assert the
	// bound crossed the boundary in UTC. An in-memory fake CANNOT catch a zone
	// bug by comparing instants (AGENTS.md rule 6) — only by inspecting
	// Location(), which is what TestEscalationStart_NextRunAtCrossesBoundaryInUTC
	// does with this field.
	createdNextRunAt []time.Time
}

func newEscFakeStateRepo() *escFakeStateRepo {
	return &escFakeStateRepo{byID: make(map[int64]*domain.AlertEscalation), nextID: 1}
}

func (r *escFakeStateRepo) clone(e *domain.AlertEscalation) *domain.AlertEscalation {
	cp := *e
	if e.LeaseOwner != nil {
		s := *e.LeaseOwner
		cp.LeaseOwner = &s
	}
	if e.LeaseUntil != nil {
		t := *e.LeaseUntil
		cp.LeaseUntil = &t
	}
	return &cp
}

func (r *escFakeStateRepo) Create(_ context.Context, e *domain.AlertEscalation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.byID {
		if existing.AlertID == e.AlertID {
			return ports.ErrConflict
		}
	}
	e.ID = r.nextID
	r.nextID++
	now := time.Now().UTC()
	e.CreatedAt, e.UpdatedAt = now, now
	r.createdNextRunAt = append(r.createdNextRunAt, e.NextRunAt)
	r.byID[e.ID] = r.clone(e)
	return nil
}

func (r *escFakeStateRepo) GetByAlertID(_ context.Context, alertID int64) (*domain.AlertEscalation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.byID {
		if e.AlertID == alertID {
			return r.clone(e), nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *escFakeStateRepo) ListByAlertIDs(_ context.Context, alertIDs []int64) (map[int64]*domain.AlertEscalation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := make(map[int64]struct{}, len(alertIDs))
	for _, id := range alertIDs {
		want[id] = struct{}{}
	}
	out := make(map[int64]*domain.AlertEscalation, len(alertIDs))
	for _, e := range r.byID {
		if _, ok := want[e.AlertID]; ok {
			out[e.AlertID] = r.clone(e)
		}
	}
	return out, nil
}

func (r *escFakeStateRepo) ClaimDue(_ context.Context, claimToken string, now, leaseUntil time.Time) ([]*domain.AlertEscalation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.AlertEscalation, 0, len(r.byID))
	for _, e := range r.byID {
		if e.Status != domain.EscalationStatePending {
			continue
		}
		if e.NextRunAt.After(now) {
			continue
		}
		if e.LeaseUntil != nil && e.LeaseUntil.After(now) {
			continue // still owned by someone else
		}
		token := claimToken
		until := leaseUntil
		e.LeaseOwner = &token
		e.LeaseUntil = &until
		out = append(out, r.clone(e))
	}
	return out, nil
}

func (r *escFakeStateRepo) Advance(_ context.Context, id int64, claimToken string, nextStep int, nextRunAt time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok || e.Status != domain.EscalationStatePending || e.LeaseOwner == nil || *e.LeaseOwner != claimToken {
		return false, nil
	}
	e.NextStep = nextStep
	e.NextRunAt = nextRunAt
	e.LeaseOwner, e.LeaseUntil = nil, nil
	return true, nil
}

func (r *escFakeStateRepo) Finish(_ context.Context, id int64, claimToken, status string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok || e.Status != domain.EscalationStatePending || e.LeaseOwner == nil || *e.LeaseOwner != claimToken {
		return false, nil
	}
	e.Status = status
	e.LeaseOwner, e.LeaseUntil = nil, nil
	return true, nil
}

func (r *escFakeStateRepo) CancelByAlertID(_ context.Context, alertID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.byID {
		if e.AlertID == alertID && e.Status == domain.EscalationStatePending {
			e.Status = domain.EscalationStateCanceled
			e.LeaseOwner, e.LeaseUntil = nil, nil
		}
	}
	return nil
}

type escSend struct {
	ids     []int64
	message string
}

type escFakeNotifier struct {
	mu    sync.Mutex
	sends []escSend
	err   error
}

func (n *escFakeNotifier) DispatchToNotificationIDs(_ context.Context, ids []int64, alert domain.AlertContext) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sends = append(n.sends, escSend{ids: append([]int64(nil), ids...), message: alert.Message})
	return n.err
}

func (n *escFakeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.sends)
}

func (n *escFakeNotifier) all() []escSend {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]escSend(nil), n.sends...)
}

// escHookAlertRepo wraps the F2.2 alert fake so a test can mutate the alert
// exactly between ClaimDue and the runner's re-read.
type escHookAlertRepo struct {
	*fakeAlertRepo
	beforeGet func()
}

func (r *escHookAlertRepo) GetByID(ctx context.Context, id int64) (*domain.Alert, error) {
	if r.beforeGet != nil {
		hook := r.beforeGet
		r.beforeGet = nil // fire once
		hook()
	}
	return r.fakeAlertRepo.GetByID(ctx, id)
}

// --- harness ---------------------------------------------------------------

type escHarness struct {
	svc      *EscalationService
	alertSvc *AlertService
	policies *escFakePolicyRepo
	assign   *escFakeAssignRepo
	state    *escFakeStateRepo
	alerts   *escHookAlertRepo
	monitors *escFakeMonitorRepo
	groups   *escFakeGroupRepo
	notifier *escFakeNotifier
	clock    time.Time
}

func newEscHarness(t *testing.T) *escHarness {
	t.Helper()
	h := &escHarness{
		policies: newEscFakePolicyRepo(),
		assign:   newEscFakeAssignRepo(),
		state:    newEscFakeStateRepo(),
		alerts:   &escHookAlertRepo{fakeAlertRepo: newFakeAlertRepo()},
		monitors: &escFakeMonitorRepo{byID: map[int64]*domain.Monitor{}},
		groups:   &escFakeGroupRepo{byID: map[int64]*domain.MonitorGroup{}},
		notifier: &escFakeNotifier{},
		clock:    time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
	}
	h.svc = NewEscalationService(h.policies, h.assign, h.state, h.alerts, h.monitors, h.groups, h.notifier)
	h.svc.now = func() time.Time { return h.clock }
	h.alertSvc = NewAlertService(h.alerts)
	h.alertSvc.now = func() time.Time { return h.clock }
	h.alertSvc.SetEscalationCanceller(h.svc)
	return h
}

// monitor registers the single monitor these tests exercise. One monitor is
// enough: the fan-out behavior lives in ClaimDue, which the repository contract
// covers against real SQL.
func (h *escHarness) monitor(groupID *int64) *domain.Monitor {
	m := &domain.Monitor{ID: 1, Name: "monitor-1", GroupID: groupID}
	h.monitors.byID[m.ID] = m
	return m
}

func (h *escHarness) group(id int64, parent *int64) {
	h.groups.byID[id] = &domain.MonitorGroup{ID: id, Name: fmt.Sprintf("group-%d", id), ParentID: parent}
}

func step(order, wait int, ids ...int64) domain.EscalationStep {
	return domain.EscalationStep{StepOrder: order, WaitMinutes: wait, NotificationIDs: ids}
}

func i64(v int64) *int64 { return &v }

// openFiringAlert opens a firing alert for the monitor and starts its ladder.
func (h *escHarness) openFiringAlert(t *testing.T, m *domain.Monitor) *domain.Alert {
	t.Helper()
	a, err := h.alertSvc.OpenOnDown(context.Background(), m, h.clock)
	if err != nil {
		t.Fatalf("OpenOnDown: %v", err)
	}
	if err := h.svc.StartForAlert(context.Background(), a, m); err != nil {
		t.Fatalf("StartForAlert: %v", err)
	}
	return a
}

func (h *escHarness) runDue(t *testing.T) int {
	t.Helper()
	sent, err := h.svc.RunDue(context.Background())
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	return sent
}

// --- Contract 1: precedence -------------------------------------------------

func TestEscalationResolve_MonitorPolicyBeatsAncestorGroup(t *testing.T) {
	h := newEscHarness(t)
	h.group(10, nil)
	m := h.monitor(i64(10))

	monitorPolicy := h.policies.seed(true, step(1, 5, 100))
	groupPolicy := h.policies.seed(true, step(1, 5, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, monitorPolicy.ID)
	_ = h.assign.AssignGroup(context.Background(), 10, groupPolicy.ID)

	got, err := h.svc.ResolvePolicy(context.Background(), m)
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if got == nil || got.ID != monitorPolicy.ID {
		t.Fatalf("resolved %v; want the monitor's own policy %d", got, monitorPolicy.ID)
	}
}

func TestEscalationResolve_NearestAncestorWins(t *testing.T) {
	h := newEscHarness(t)
	h.group(10, nil)     // root
	h.group(11, i64(10)) // middle
	m := h.monitor(i64(11))

	rootPolicy := h.policies.seed(true, step(1, 5, 100))
	midPolicy := h.policies.seed(true, step(1, 5, 200))
	_ = h.assign.AssignGroup(context.Background(), 10, rootPolicy.ID)
	_ = h.assign.AssignGroup(context.Background(), 11, midPolicy.ID)

	got, err := h.svc.ResolvePolicy(context.Background(), m)
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if got == nil || got.ID != midPolicy.ID {
		t.Fatalf("resolved %v; want the nearest ancestor's policy %d", got, midPolicy.ID)
	}
}

func TestEscalationResolve_RootAncestorWinsWhenMiddleHasNone(t *testing.T) {
	h := newEscHarness(t)
	h.group(10, nil)
	h.group(11, i64(10))
	m := h.monitor(i64(11))

	rootPolicy := h.policies.seed(true, step(1, 5, 100))
	_ = h.assign.AssignGroup(context.Background(), 10, rootPolicy.ID)

	got, err := h.svc.ResolvePolicy(context.Background(), m)
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if got == nil || got.ID != rootPolicy.ID {
		t.Fatalf("resolved %v; want the root's policy %d", got, rootPolicy.ID)
	}
}

// A disabled policy is "assigned but inert": it STOPS the walk. Falling through
// to the parent would silently page a different set of humans, which is worse
// than paging nobody.
func TestEscalationResolve_DisabledPolicyDoesNotFallThrough(t *testing.T) {
	h := newEscHarness(t)
	h.group(10, nil)
	h.group(11, i64(10))
	m := h.monitor(i64(11))

	rootPolicy := h.policies.seed(true, step(1, 5, 100))
	disabled := h.policies.seed(false, step(1, 5, 200))
	_ = h.assign.AssignGroup(context.Background(), 10, rootPolicy.ID)
	_ = h.assign.AssignGroup(context.Background(), 11, disabled.ID)

	got, err := h.svc.ResolvePolicy(context.Background(), m)
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved policy %d; want nil (disabled must not fall through to the root)", got.ID)
	}
}

func TestEscalationResolve_EmptyPolicyDoesNotFallThrough(t *testing.T) {
	h := newEscHarness(t)
	h.group(10, nil)
	m := h.monitor(i64(10))

	groupPolicy := h.policies.seed(true, step(1, 5, 100))
	empty := h.policies.seed(true)
	_ = h.assign.AssignGroup(context.Background(), 10, groupPolicy.ID)
	_ = h.assign.AssignMonitor(context.Background(), m.ID, empty.ID)

	got, err := h.svc.ResolvePolicy(context.Background(), m)
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved policy %d; want nil (a step-less policy must not fall through)", got.ID)
	}
}

func TestEscalationResolve_NoAssignmentAnywhere(t *testing.T) {
	h := newEscHarness(t)
	h.group(10, nil)
	m := h.monitor(i64(10))

	got, err := h.svc.ResolvePolicy(context.Background(), m)
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved %d; want nil", got.ID)
	}
}

func TestEscalationResolve_GroupCycleTerminates(t *testing.T) {
	h := newEscHarness(t)
	// A parent_id cycle should be impossible (MonitorGroupService rejects them),
	// but the resolver runs on the alerting path and must terminate regardless.
	h.group(10, i64(11))
	h.group(11, i64(10))
	m := h.monitor(i64(10))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := h.svc.ResolvePolicy(context.Background(), m); err != nil {
			t.Errorf("ResolvePolicy: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ResolvePolicy did not terminate on a group cycle")
	}
}

// --- Contract 2: step zero --------------------------------------------------

// The initial DOWN notification belongs to the dispatcher. Starting a ladder
// must send NOTHING extra, and must schedule step 1 rather than a step 0.
func TestEscalationStart_DoesNotTouchStepZero(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100), step(2, 10, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)

	a := h.openFiringAlert(t, m)

	if got := h.notifier.count(); got != 0 {
		t.Fatalf("escalation sent %d notifications at start; want 0 — step zero is the dispatcher's", got)
	}
	e, err := h.state.GetByAlertID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByAlertID: %v", err)
	}
	if e.NextStep != 1 {
		t.Fatalf("NextStep = %d; want 1 (step 0 is never owned by a policy)", e.NextStep)
	}
	want := a.FiredAt.UTC().Add(5 * time.Minute)
	if !e.NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %v; want %v (fired_at + step 1 wait)", e.NextRunAt, want)
	}
}

// A dispatcher wired end to end must send the initial alert exactly once, and
// the escalation must not duplicate it.
func TestEscalationStart_DispatcherSendsInitialAlertExactlyOnce(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 0, 100))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)

	stepZero := &fakeNotifier{}
	d := newDispatcher(stepZero, &fakeMaintenance{})
	d.now = func() time.Time { return h.clock }
	d.SetAlertLifecycle(h.alertSvc)
	d.SetEscalationStarter(h.svc)

	d.OnHeartbeat(context.Background(), m, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusUp))

	if got := stepZero.count(); got != 1 {
		t.Fatalf("step-zero notifications = %d; want exactly 1", got)
	}
	if got := h.notifier.count(); got != 0 {
		t.Fatalf("escalation sends before the runner ticks = %d; want 0", got)
	}
	if _, err := h.state.GetByAlertID(context.Background(), 1); err != nil {
		t.Fatalf("escalation was not started: %v", err)
	}
}

// An in-memory fake cannot catch a zone bug by comparing instants — they are
// zone-independent. Assert the Location() of what crossed the boundary.
func TestEscalationStart_NextRunAtCrossesBoundaryInUTC(t *testing.T) {
	h := newEscHarness(t)
	bangkok, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)

	// A caller that hands the service a local-zoned FiredAt must not be able to
	// push a local wall-clock into the repository.
	a := &domain.Alert{ID: 99, MonitorID: m.ID, FiredAt: time.Date(2026, 7, 26, 17, 0, 0, 0, bangkok)}
	if err := h.svc.StartForAlert(context.Background(), a, m); err != nil {
		t.Fatalf("StartForAlert: %v", err)
	}
	if len(h.state.createdNextRunAt) != 1 {
		t.Fatalf("Create calls = %d; want 1", len(h.state.createdNextRunAt))
	}
	if loc := h.state.createdNextRunAt[0].Location(); loc != time.UTC {
		t.Fatalf("NextRunAt Location = %v; want UTC", loc)
	}
}

func TestEscalationStart_NoPolicyStartsNothing(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	a := h.openFiringAlert(t, m)
	if _, err := h.state.GetByAlertID(context.Background(), a.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("escalation state exists without a policy (err = %v)", err)
	}
}

func TestEscalationStart_IsIdempotent(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)

	a := h.openFiringAlert(t, m)
	// A second worker racing the same confirmed DOWN must not error.
	if err := h.svc.StartForAlert(context.Background(), a, m); err != nil {
		t.Fatalf("second StartForAlert: %v", err)
	}
	if len(h.state.byID) != 1 {
		t.Fatalf("escalation rows = %d; want 1", len(h.state.byID))
	}
}

// --- The runner -------------------------------------------------------------

func TestEscalationRun_NotDueSendsNothing(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	h.openFiringAlert(t, m)

	h.clock = h.clock.Add(4*time.Minute + 59*time.Second)
	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("sent = %d one second before due; want 0", sent)
	}
	if got := h.notifier.count(); got != 0 {
		t.Fatalf("notifications = %d; want 0", got)
	}
}

func TestEscalationRun_ExactlyDueSendsTheStepsChannels(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100, 101))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	h.openFiringAlert(t, m)

	h.clock = h.clock.Add(5 * time.Minute) // exactly the boundary
	if sent := h.runDue(t); sent != 1 {
		t.Fatalf("sent = %d at the exact wait boundary; want 1", sent)
	}
	sends := h.notifier.all()
	if len(sends) != 1 {
		t.Fatalf("dispatch calls = %d; want 1", len(sends))
	}
	if len(sends[0].ids) != 2 || sends[0].ids[0] != 100 || sends[0].ids[1] != 101 {
		t.Fatalf("dispatched to %v; want exactly [100 101]", sends[0].ids)
	}
}

func TestEscalationRun_AdvancesWithCumulativeWait(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100), step(2, 10, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	a := h.openFiringAlert(t, m)

	h.clock = h.clock.Add(5 * time.Minute)
	h.runDue(t)

	e, err := h.state.GetByAlertID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByAlertID: %v", err)
	}
	if e.NextStep != 2 {
		t.Fatalf("NextStep = %d; want 2", e.NextStep)
	}
	// Cumulative: step 2 is due ten minutes after step 1 RAN, not after fired_at.
	want := h.clock.Add(10 * time.Minute)
	if !e.NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %v; want %v", e.NextRunAt, want)
	}

	// Not yet due.
	h.clock = h.clock.Add(9 * time.Minute)
	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("step 2 sent %d one minute early; want 0", sent)
	}
	// Due.
	h.clock = h.clock.Add(1 * time.Minute)
	if sent := h.runDue(t); sent != 1 {
		t.Fatalf("step 2 sent = %d; want 1", sent)
	}
	sends := h.notifier.all()
	if len(sends) != 2 || len(sends[1].ids) != 1 || sends[1].ids[0] != 200 {
		t.Fatalf("second dispatch = %v; want exactly [200]", sends)
	}

	e, _ = h.state.GetByAlertID(context.Background(), a.ID)
	if e.Status != domain.EscalationStateDone {
		t.Fatalf("status after the last step = %s; want done", e.Status)
	}

	// Nothing more, ever.
	h.clock = h.clock.Add(time.Hour)
	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("sent = %d after the ladder finished; want 0", sent)
	}
}

// Acknowledgement stops escalating. It must NOT close the outage: OpenMonitorID
// stays set so resends remain suppressed (handoff §4.8).
func TestEscalationRun_AckCancelsFutureStepsWithoutClosingTheOutage(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100), step(2, 5, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	a := h.openFiringAlert(t, m)

	h.clock = h.clock.Add(5 * time.Minute)
	h.runDue(t) // step 1 fires

	uid := int64(3)
	if _, err := h.alertSvc.Acknowledge(context.Background(), a.ID, &uid); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	h.clock = h.clock.Add(10 * time.Minute)
	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("sent = %d after ack; want 0", sent)
	}
	if got := h.notifier.count(); got != 1 {
		t.Fatalf("total escalation sends = %d; want 1 (only the pre-ack step)", got)
	}

	e, err := h.state.GetByAlertID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByAlertID: %v", err)
	}
	if e.Status != domain.EscalationStateCanceled {
		t.Fatalf("escalation status = %s; want canceled", e.Status)
	}

	acked, err := h.alerts.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if acked.Status != domain.AlertStatusAcked {
		t.Fatalf("alert status = %s; want acked", acked.Status)
	}
	if acked.OpenMonitorID == nil {
		t.Fatal("OpenMonitorID was cleared on ack; the outage must stay open")
	}
}

func TestEscalationRun_ResolveCancels(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100), step(2, 5, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	a := h.openFiringAlert(t, m)

	if err := h.alertSvc.ResolveOpen(context.Background(), m.ID, h.clock.Add(time.Minute)); err != nil {
		t.Fatalf("ResolveOpen: %v", err)
	}

	h.clock = h.clock.Add(time.Hour)
	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("sent = %d after recovery; want 0", sent)
	}
	e, err := h.state.GetByAlertID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByAlertID: %v", err)
	}
	if e.Status != domain.EscalationStateCanceled {
		t.Fatalf("escalation status = %s; want canceled", e.Status)
	}
}

// The window cancellation alone does NOT close: an ack landing after ClaimDue
// but before the send. The runner's re-read of the alert inside the claim is
// what makes acknowledgement authoritative.
func TestEscalationRun_AckBetweenClaimAndSendSendsNothing(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	a := h.openFiringAlert(t, m)

	h.clock = h.clock.Add(5 * time.Minute)
	h.alerts.beforeGet = func() {
		if _, err := h.alertSvc.Acknowledge(context.Background(), a.ID, nil); err != nil {
			t.Errorf("ack inside the claim window: %v", err)
		}
	}

	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("sent = %d for an alert acked mid-claim; want 0", sent)
	}
	if got := h.notifier.count(); got != 0 {
		t.Fatalf("notifications = %d; want 0", got)
	}
}

// Progress is a row, not memory: a brand-new service instance over the same
// repositories resumes at NextStep.
func TestEscalationRun_RestartResumesAtNextStep(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100), step(2, 5, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	h.openFiringAlert(t, m)

	h.clock = h.clock.Add(5 * time.Minute)
	h.runDue(t) // step 1

	// "Restart": a fresh service, nothing carried over in memory.
	restarted := NewEscalationService(h.policies, h.assign, h.state, h.alerts, h.monitors, h.groups, h.notifier)
	restarted.now = func() time.Time { return h.clock }
	restarted.SetWorkerID("worker-b")

	h.clock = h.clock.Add(5 * time.Minute)
	sent, err := restarted.RunDue(context.Background())
	if err != nil {
		t.Fatalf("RunDue after restart: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent = %d after restart; want 1 (resume at step 2)", sent)
	}
	sends := h.notifier.all()
	if len(sends) != 2 || sends[1].ids[0] != 200 {
		t.Fatalf("post-restart dispatch = %v; want step 2's channel [200]", sends)
	}
}

// Two workers polling the same instant must send the step exactly once.
func TestEscalationClaim_TwoWorkersSendTheStepOnce(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	h.openFiringAlert(t, m)
	h.clock = h.clock.Add(5 * time.Minute)

	workerA := NewEscalationService(h.policies, h.assign, h.state, h.alerts, h.monitors, h.groups, h.notifier)
	workerA.now = func() time.Time { return h.clock }
	workerA.SetWorkerID("worker-a")
	workerB := NewEscalationService(h.policies, h.assign, h.state, h.alerts, h.monitors, h.groups, h.notifier)
	workerB.now = func() time.Time { return h.clock }
	workerB.SetWorkerID("worker-b")

	var wg sync.WaitGroup
	var total int
	var mu sync.Mutex
	for _, w := range []*EscalationService{workerA, workerB} {
		wg.Add(1)
		go func(svc *EscalationService) {
			defer wg.Done()
			sent, err := svc.RunDue(context.Background())
			if err != nil {
				t.Errorf("RunDue: %v", err)
			}
			mu.Lock()
			total += sent
			mu.Unlock()
		}(w)
	}
	wg.Wait()

	if total != 1 {
		t.Fatalf("total steps sent across two workers = %d; want exactly 1", total)
	}
	if got := h.notifier.count(); got != 1 {
		t.Fatalf("dispatch calls = %d; want exactly 1", got)
	}
}

// A dead channel must not freeze every later rung behind it.
func TestEscalationRun_DispatchFailureStillAdvances(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100), step(2, 5, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	a := h.openFiringAlert(t, m)

	h.notifier.err = errors.New("smtp down")
	h.clock = h.clock.Add(5 * time.Minute)
	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("sent = %d for a failed dispatch; want 0 reported as delivered", sent)
	}
	e, err := h.state.GetByAlertID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByAlertID: %v", err)
	}
	if e.NextStep != 2 {
		t.Fatalf("NextStep = %d after a failed step; want 2 — a dead channel must not stall the ladder", e.NextStep)
	}
}

func TestEscalationRun_DisabledMidFlightCancels(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100), step(2, 5, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	a := h.openFiringAlert(t, m)

	stored, _ := h.policies.GetByID(context.Background(), p.ID)
	stored.Enabled = false
	if err := h.policies.Update(context.Background(), stored); err != nil {
		t.Fatalf("Update: %v", err)
	}

	h.clock = h.clock.Add(5 * time.Minute)
	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("sent = %d after the policy was disabled; want 0", sent)
	}
	e, _ := h.state.GetByAlertID(context.Background(), a.ID)
	if e.Status != domain.EscalationStateCanceled {
		t.Fatalf("status = %s; want canceled", e.Status)
	}
}

func TestEscalationRun_LadderShortenedMidFlightFinishes(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 5, 100), step(2, 5, 200))
	_ = h.assign.AssignMonitor(context.Background(), m.ID, p.ID)
	a := h.openFiringAlert(t, m)

	h.clock = h.clock.Add(5 * time.Minute)
	h.runDue(t) // step 1 fires, NextStep becomes 2

	stored, _ := h.policies.GetByID(context.Background(), p.ID)
	stored.Steps = stored.Steps[:1] // operator deletes step 2
	if err := h.policies.Update(context.Background(), stored); err != nil {
		t.Fatalf("Update: %v", err)
	}

	h.clock = h.clock.Add(5 * time.Minute)
	if sent := h.runDue(t); sent != 0 {
		t.Fatalf("sent = %d for a deleted step; want 0", sent)
	}
	e, _ := h.state.GetByAlertID(context.Background(), a.ID)
	if e.Status != domain.EscalationStateDone {
		t.Fatalf("status = %s; want done", e.Status)
	}
}

// --- Validation -------------------------------------------------------------

func TestEscalationPolicy_ValidationAndRenumbering(t *testing.T) {
	h := newEscHarness(t)
	ctx := context.Background()

	if err := h.svc.CreatePolicy(ctx, &domain.EscalationPolicy{Name: "  "}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank name err = %v; want ErrValidation", err)
	}
	if err := h.svc.CreatePolicy(ctx, &domain.EscalationPolicy{
		Name:  "p",
		Steps: []domain.EscalationStep{step(1, -1, 100)},
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative wait err = %v; want ErrValidation", err)
	}
	if err := h.svc.CreatePolicy(ctx, &domain.EscalationPolicy{
		Name:  "p",
		Steps: []domain.EscalationStep{step(1, maxEscalationWaitMinutes+1, 100)},
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("over-long wait err = %v; want ErrValidation", err)
	}
	if err := h.svc.CreatePolicy(ctx, &domain.EscalationPolicy{
		Name:  "p",
		Steps: []domain.EscalationStep{step(1, 5)},
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("channel-less step err = %v; want ErrValidation", err)
	}

	// Sparse, out-of-order input is renumbered to a dense 1..N and duplicate
	// channels inside one step collapse.
	p := &domain.EscalationPolicy{
		Name: "ladder",
		Steps: []domain.EscalationStep{
			step(9, 10, 200),
			step(3, 5, 100, 100, 101),
		},
	}
	if err := h.svc.CreatePolicy(ctx, p); err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if len(p.Steps) != 2 || p.Steps[0].StepOrder != 1 || p.Steps[1].StepOrder != 2 {
		t.Fatalf("step orders = %d,%d; want 1,2", p.Steps[0].StepOrder, p.Steps[1].StepOrder)
	}
	if len(p.Steps[0].NotificationIDs) != 2 {
		t.Fatalf("step 1 channels = %v; want the duplicate collapsed", p.Steps[0].NotificationIDs)
	}
	if p.Steps[0].WaitMinutes != 5 {
		t.Fatalf("renumbering reordered the waits: got %d; want 5 first", p.Steps[0].WaitMinutes)
	}
}

func TestEscalationAssign_RejectsUnknownPolicy(t *testing.T) {
	h := newEscHarness(t)
	if err := h.svc.AssignMonitor(context.Background(), 1, 4242); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("assign unknown policy err = %v; want ErrNotFound", err)
	}
	if err := h.svc.AssignGroup(context.Background(), 1, 4242); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("assign group unknown policy err = %v; want ErrNotFound", err)
	}
}

func TestEscalationListAssignments_EmptyAndUnknown(t *testing.T) {
	h := newEscHarness(t)
	ctx := context.Background()
	p := h.policies.seed(true, step(1, 5, 1))

	view, err := h.svc.ListAssignments(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListAssignments empty: %v", err)
	}
	if len(view.Monitors) != 0 || len(view.Groups) != 0 {
		t.Fatalf("empty assignments = %+v; want empty", view)
	}

	if _, err := h.svc.ListAssignments(ctx, 9999); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown policy err = %v; want ErrNotFound", err)
	}
}

func TestEscalationListAssignments_AfterAssign(t *testing.T) {
	h := newEscHarness(t)
	ctx := context.Background()
	p := h.policies.seed(true, step(1, 5, 1))
	m := h.monitor(nil)
	h.group(10, nil)

	if err := h.svc.AssignMonitor(ctx, m.ID, p.ID); err != nil {
		t.Fatalf("AssignMonitor: %v", err)
	}
	if err := h.svc.AssignGroup(ctx, 10, p.ID); err != nil {
		t.Fatalf("AssignGroup: %v", err)
	}

	view, err := h.svc.ListAssignments(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListAssignments: %v", err)
	}
	if len(view.Monitors) != 1 || view.Monitors[0].ID != m.ID || view.Monitors[0].Name != m.Name {
		t.Fatalf("monitors = %+v; want [{%d %q}]", view.Monitors, m.ID, m.Name)
	}
	if len(view.Groups) != 1 || view.Groups[0].ID != 10 || view.Groups[0].Name != "group-10" {
		t.Fatalf("groups = %+v; want [{10 group-10}]", view.Groups)
	}

	// Direct assignments only — a sibling policy's links must not appear.
	other := h.policies.seed(true, step(1, 1, 1))
	m2 := &domain.Monitor{ID: 2, Name: "other-monitor"}
	h.monitors.byID[m2.ID] = m2
	_ = h.svc.AssignMonitor(ctx, m2.ID, other.ID)

	view, err = h.svc.ListAssignments(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListAssignments again: %v", err)
	}
	if len(view.Monitors) != 1 || view.Monitors[0].ID != m.ID {
		t.Fatalf("after sibling assign, monitors = %+v; want only monitor 1", view.Monitors)
	}
}
