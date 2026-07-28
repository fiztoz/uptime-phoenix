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

// --- fakes for MonitorGroupService ------------------------------------

// grpFakeGroupRepo is an in-memory ports.MonitorGroupRepository.
type grpFakeGroupRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.MonitorGroup
	nextID int64
	// monitors, when set, lets Delete re-home the monitors filed under the group
	// it removes — the same guarantee the SQL repos make in their transaction.
	monitors *grpFakeMonitorRepo
}

func newGrpFakeGroupRepo() *grpFakeGroupRepo {
	return &grpFakeGroupRepo{byID: make(map[int64]*domain.MonitorGroup)}
}

func (r *grpFakeGroupRepo) Create(_ context.Context, g *domain.MonitorGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	g.ID = r.nextID
	cp := *g
	r.byID[g.ID] = &cp
	return nil
}

func (r *grpFakeGroupRepo) GetByID(_ context.Context, id int64) (*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *g
	return &cp, nil
}

func (r *grpFakeGroupRepo) List(_ context.Context, userID int64) ([]*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MonitorGroup, 0)
	for _, g := range r.byID {
		if userID > 0 && g.UserID != userID {
			continue
		}
		cp := *g
		out = append(out, &cp)
	}
	return out, nil
}

func (r *grpFakeGroupRepo) ListAll(ctx context.Context) ([]*domain.MonitorGroup, error) {
	return r.List(ctx, 0)
}

func (r *grpFakeGroupRepo) Update(_ context.Context, g *domain.MonitorGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.byID[g.ID]
	if !ok {
		return ports.ErrNotFound
	}
	cp := *g
	// last_status is owned by ClaimStatusTransition — the real repos exclude it
	// from Update so a stale group round-tripping through the admin API cannot
	// clobber a worker's alerting decision. A fake that wrote it here would model
	// a contract production does not have.
	cp.LastStatus = existing.LastStatus
	r.byID[g.ID] = &cp
	return nil
}

// ClaimStatusTransition models the repositories' compare-and-set: the write only
// lands when the stored value still matches `from`, and `won` reports whether
// this caller is the one that moved it.
//
// A fake that simply assigned and returned true would model a CAS that always
// wins — and the concurrent double-send it exists to prevent would sail straight
// through every test.
func (r *grpFakeGroupRepo) ClaimStatusTransition(_ context.Context, groupID int64, from *domain.Status, to domain.Status) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byID[groupID]
	if !ok {
		return false, ports.ErrNotFound
	}
	if !statusPtrEqual(g.LastStatus, from) {
		return false, nil
	}
	next := to
	g.LastStatus = &next
	return true, nil
}

// statusPtrEqual is the null-safe comparison the SQL adapters get from
// `IS` (SQLite) / `<=>` (MariaDB): two NULLs are equal.
func statusPtrEqual(a, b *domain.Status) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Delete mirrors the real repos: it re-homes the deleted group's children — both
// monitors and subgroups — to the group's own parent, and never cascades. A fake
// that merely dropped the row would model a contract the production code does not
// have, and would happily pass a service that silently orphaned every monitor.
func (r *grpFakeGroupRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	g, ok := r.byID[id]
	if !ok {
		r.mu.Unlock()
		return ports.ErrNotFound
	}
	newParent := g.ParentID
	for _, other := range r.byID {
		if other.ParentID != nil && *other.ParentID == id {
			other.ParentID = newParent
		}
	}
	delete(r.byID, id)
	monitors := r.monitors
	r.mu.Unlock()

	if monitors != nil {
		monitors.mu.Lock()
		for _, m := range monitors.byID {
			if m.GroupID != nil && *m.GroupID == id {
				m.GroupID = newParent
			}
		}
		monitors.mu.Unlock()
	}
	return nil
}

// grpFakeMonitorRepo is an in-memory ports.MonitorRepository that actually
// honors MonitorFilter.UserID, unlike some of the lighter fakes elsewhere in
// this package — ResolveStatuses depends on that filtering being real.
type grpFakeMonitorRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Monitor
	nextID int64
}

func newGrpFakeMonitorRepo() *grpFakeMonitorRepo {
	return &grpFakeMonitorRepo{byID: make(map[int64]*domain.Monitor)}
}

func (r *grpFakeMonitorRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	m.ID = r.nextID
	cp := *m
	r.byID[m.ID] = &cp
	return nil
}

func (r *grpFakeMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (r *grpFakeMonitorRepo) GetByPushToken(_ context.Context, _ string) (*domain.Monitor, error) {
	return nil, ports.ErrNotFound
}

func (r *grpFakeMonitorRepo) List(_ context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Honor RestrictToIDs the way the SQL adapters do: branch on the FLAG, never
	// on len(MonitorIDs). An empty allowlist means "no monitors are visible", and
	// a fake that read it as "no filter" would hide a full cross-tenant data leak.
	var allowed map[int64]bool
	if filter.RestrictToIDs {
		allowed = make(map[int64]bool, len(filter.MonitorIDs))
		for _, id := range filter.MonitorIDs {
			allowed[id] = true
		}
	}

	out := make([]*domain.Monitor, 0)
	for _, m := range r.byID {
		if filter.RestrictToIDs && !allowed[m.ID] {
			continue
		}
		if filter.UserID > 0 && m.UserID != filter.UserID {
			continue
		}
		// Honor the group filters exactly as the real repos do. A fake that
		// ignored them would quietly widen every GroupID-filtered query to "all
		// monitors" and let a broken caller pass.
		if filter.GroupIDIsNull && m.GroupID != nil {
			continue
		}
		if filter.GroupID != nil && (m.GroupID == nil || *m.GroupID != *filter.GroupID) {
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	return out, nil
}

func (r *grpFakeMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *grpFakeMonitorRepo) Update(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *m
	r.byID[m.ID] = &cp
	return nil
}
func (r *grpFakeMonitorRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}
func (r *grpFakeMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *grpFakeMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error)  { return 0, nil }
func (r *grpFakeMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) { return 0, nil }

func newGroupTestService() (*MonitorGroupService, *grpFakeMonitorRepo, *fakeHeartbeatRepo) {
	groups := newGrpFakeGroupRepo()
	monitors := newGrpFakeMonitorRepo()
	groups.monitors = monitors // so a group delete re-homes monitors, as the real repos do
	hb := newFakeHeartbeatRepo()
	svc := NewMonitorGroupService(groups, monitors, hb, testLogger())
	return svc, monitors, hb
}

// --- Validation ----------------------------------------------------------

func TestMonitorGroupService_Create_Validation(t *testing.T) {
	svc, _, _ := newGroupTestService()
	ctx := context.Background()

	t.Run("name required", func(t *testing.T) {
		g := &domain.MonitorGroup{UserID: 1, Name: "   "}
		err := svc.Create(ctx, g)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(blank name) error = %v, want domain.ErrValidation", err)
		}
	})

	t.Run("empty condition defaults to worst_of_children", func(t *testing.T) {
		g := &domain.MonitorGroup{UserID: 1, Name: "Payments"}
		if err := svc.Create(ctx, g); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if g.Condition != domain.GroupConditionWorstOfChildren {
			t.Fatalf("Condition = %q, want %q", g.Condition, domain.GroupConditionWorstOfChildren)
		}
	})

	t.Run("invalid condition rejected", func(t *testing.T) {
		g := &domain.MonitorGroup{UserID: 1, Name: "Bad", Condition: "nonsense"}
		err := svc.Create(ctx, g)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(bad condition) error = %v, want domain.ErrValidation", err)
		}
	})

	t.Run("threshold count below 1 rejected", func(t *testing.T) {
		g := &domain.MonitorGroup{UserID: 1, Name: "T1", Condition: domain.GroupConditionThreshold, Threshold: 0}
		err := svc.Create(ctx, g)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(threshold=0) error = %v, want domain.ErrValidation", err)
		}
	})

	t.Run("threshold count valid accepted", func(t *testing.T) {
		g := &domain.MonitorGroup{UserID: 1, Name: "T2", Condition: domain.GroupConditionThreshold, Threshold: 3}
		if err := svc.Create(ctx, g); err != nil {
			t.Fatalf("Create(threshold=3): %v", err)
		}
	})

	t.Run("percent threshold out of range rejected", func(t *testing.T) {
		g := &domain.MonitorGroup{
			UserID: 1, Name: "T3", Condition: domain.GroupConditionThreshold,
			Threshold: 101, ThresholdIsPercent: true,
		}
		err := svc.Create(ctx, g)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(percent=101) error = %v, want domain.ErrValidation", err)
		}
		g2 := &domain.MonitorGroup{
			UserID: 1, Name: "T4", Condition: domain.GroupConditionThreshold,
			Threshold: 0, ThresholdIsPercent: true,
		}
		err = svc.Create(ctx, g2)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(percent=0) error = %v, want domain.ErrValidation", err)
		}
	})

	t.Run("percent threshold in range accepted", func(t *testing.T) {
		g := &domain.MonitorGroup{
			UserID: 1, Name: "T5", Condition: domain.GroupConditionThreshold,
			Threshold: 50, ThresholdIsPercent: true,
		}
		if err := svc.Create(ctx, g); err != nil {
			t.Fatalf("Create(percent=50): %v", err)
		}
	})

	t.Run("nonexistent parent rejected", func(t *testing.T) {
		missing := int64(9999)
		g := &domain.MonitorGroup{UserID: 1, Name: "Orphan", ParentID: &missing}
		err := svc.Create(ctx, g)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(missing parent) error = %v, want domain.ErrValidation", err)
		}
	})

	t.Run("cross-user parent rejected", func(t *testing.T) {
		other := &domain.MonitorGroup{UserID: 2, Name: "OtherUsersFolder"}
		if err := svc.Create(ctx, other); err != nil {
			t.Fatalf("seed other user's group: %v", err)
		}
		g := &domain.MonitorGroup{UserID: 1, Name: "CrossTenant", ParentID: &other.ID}
		err := svc.Create(ctx, g)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(cross-tenant parent) error = %v, want domain.ErrValidation", err)
		}
	})

	t.Run("valid parent accepted", func(t *testing.T) {
		parent := &domain.MonitorGroup{UserID: 1, Name: "Parent"}
		if err := svc.Create(ctx, parent); err != nil {
			t.Fatalf("seed parent: %v", err)
		}
		child := &domain.MonitorGroup{UserID: 1, Name: "Child", ParentID: &parent.ID}
		if err := svc.Create(ctx, child); err != nil {
			t.Fatalf("Create(valid parent): %v", err)
		}
	})
	// keep reference for potential future assertions
}

func TestMonitorGroupService_SelfParentRejected(t *testing.T) {
	svc, _, _ := newGroupTestService()
	ctx := context.Background()

	g := &domain.MonitorGroup{UserID: 1, Name: "X"}
	if err := svc.Create(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}

	g.ParentID = &g.ID
	err := svc.Update(ctx, g)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Update(self parent) error = %v, want domain.ErrValidation", err)
	}
}

// TestMonitorGroupService_CycleRejection builds A -> B -> C (B's parent is A,
// C's parent is B) then attempts to reparent A under C, which would close the
// loop A -> C -> B -> A. That must be rejected.
func TestMonitorGroupService_CycleRejection(t *testing.T) {
	svc, _, _ := newGroupTestService()
	ctx := context.Background()

	a := &domain.MonitorGroup{UserID: 1, Name: "A"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("create A: %v", err)
	}
	b := &domain.MonitorGroup{UserID: 1, Name: "B", ParentID: &a.ID}
	if err := svc.Create(ctx, b); err != nil {
		t.Fatalf("create B: %v", err)
	}
	c := &domain.MonitorGroup{UserID: 1, Name: "C", ParentID: &b.ID}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatalf("create C: %v", err)
	}

	a.ParentID = &c.ID
	err := svc.Update(ctx, a)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Update(A under C) error = %v, want domain.ErrValidation (cycle)", err)
	}

	// A must remain unchanged in storage (the rejected update wasn't persisted).
	stored, getErr := svc.GetByID(ctx, a.ID)
	if getErr != nil {
		t.Fatalf("get A: %v", getErr)
	}
	if stored.ParentID != nil {
		t.Fatalf("A.ParentID = %v after rejected update, want nil (unchanged)", stored.ParentID)
	}
}

// --- ResolveStatuses -------------------------------------------------------

func seedHeartbeat(t *testing.T, hb *fakeHeartbeatRepo, monitorID int64, status domain.Status) {
	t.Helper()
	if err := hb.Save(context.Background(), &domain.Heartbeat{
		MonitorID: monitorID,
		Status:    status,
		Time:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
}

// TestMonitorGroupService_ResolveStatuses_NestedSubgroup rolls a status up
// through two levels: monitor -> subgroup -> top-level group, and asserts the
// actual derived status at each level, not just that the call succeeded.
func TestMonitorGroupService_ResolveStatuses_NestedSubgroup(t *testing.T) {
	svc, monitors, hb := newGroupTestService()
	ctx := context.Background()
	const userID = int64(1)

	top := &domain.MonitorGroup{UserID: userID, Name: "Top", Condition: domain.GroupConditionWorstOfChildren}
	if err := svc.Create(ctx, top); err != nil {
		t.Fatalf("create top: %v", err)
	}
	sub := &domain.MonitorGroup{UserID: userID, Name: "Sub", Condition: domain.GroupConditionWorstOfChildren, ParentID: &top.ID}
	if err := svc.Create(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}

	m1 := &domain.Monitor{UserID: userID, Name: "M1", Type: "http", GroupID: &sub.ID}
	if err := monitors.Create(ctx, m1); err != nil {
		t.Fatalf("create m1: %v", err)
	}
	seedHeartbeat(t, hb, m1.ID, domain.StatusDown)

	// A top-level monitor with no group must not affect any group's rollup.
	unrelated := &domain.Monitor{UserID: userID, Name: "Unrelated", Type: "http"}
	if err := monitors.Create(ctx, unrelated); err != nil {
		t.Fatalf("create unrelated: %v", err)
	}
	seedHeartbeat(t, hb, unrelated.ID, domain.StatusUp)

	// An empty group (no children with a status) must be absent from the map.
	empty := &domain.MonitorGroup{UserID: userID, Name: "Empty"}
	if err := svc.Create(ctx, empty); err != nil {
		t.Fatalf("create empty group: %v", err)
	}

	// An ignore-condition group with a DOWN child must still be absent.
	ignored := &domain.MonitorGroup{UserID: userID, Name: "Ignored", Condition: domain.GroupConditionIgnore}
	if err := svc.Create(ctx, ignored); err != nil {
		t.Fatalf("create ignored group: %v", err)
	}
	ignoredMon := &domain.Monitor{UserID: userID, Name: "InIgnored", Type: "http", GroupID: &ignored.ID}
	if err := monitors.Create(ctx, ignoredMon); err != nil {
		t.Fatalf("create monitor in ignored group: %v", err)
	}
	seedHeartbeat(t, hb, ignoredMon.ID, domain.StatusDown)

	statuses, err := svc.ResolveStatuses(ctx, userID)
	if err != nil {
		t.Fatalf("ResolveStatuses: %v", err)
	}

	subStatus, ok := statuses[sub.ID]
	if !ok {
		t.Fatalf("sub group missing from resolved statuses: %+v", statuses)
	}
	if subStatus != domain.StatusDown {
		t.Fatalf("sub status = %v, want %v (DOWN monitor inside it)", subStatus, domain.StatusDown)
	}

	topStatus, ok := statuses[top.ID]
	if !ok {
		t.Fatalf("top group missing from resolved statuses: %+v", statuses)
	}
	if topStatus != domain.StatusDown {
		t.Fatalf("top status = %v, want %v (rolled up from DOWN subgroup)", topStatus, domain.StatusDown)
	}

	if _, ok := statuses[empty.ID]; ok {
		t.Fatalf("empty group must be absent from resolved statuses, got %v", statuses[empty.ID])
	}
	if _, ok := statuses[ignored.ID]; ok {
		t.Fatalf("ignore-condition group must be absent from resolved statuses, got %v", statuses[ignored.ID])
	}
}

// TestMonitorGroupService_ResolveStatuses_ScopedToUser ensures groups and
// monitors belonging to another user never leak into the result.
func TestMonitorGroupService_ResolveStatuses_ScopedToUser(t *testing.T) {
	svc, monitors, hb := newGroupTestService()
	ctx := context.Background()

	mine := &domain.MonitorGroup{UserID: 1, Name: "Mine"}
	if err := svc.Create(ctx, mine); err != nil {
		t.Fatalf("create mine: %v", err)
	}
	theirs := &domain.MonitorGroup{UserID: 2, Name: "Theirs"}
	if err := svc.Create(ctx, theirs); err != nil {
		t.Fatalf("create theirs: %v", err)
	}

	mMine := &domain.Monitor{UserID: 1, Name: "MineMon", Type: "http", GroupID: &mine.ID}
	if err := monitors.Create(ctx, mMine); err != nil {
		t.Fatalf("create mine monitor: %v", err)
	}
	seedHeartbeat(t, hb, mMine.ID, domain.StatusUp)

	mTheirs := &domain.Monitor{UserID: 2, Name: "TheirMon", Type: "http", GroupID: &theirs.ID}
	if err := monitors.Create(ctx, mTheirs); err != nil {
		t.Fatalf("create their monitor: %v", err)
	}
	seedHeartbeat(t, hb, mTheirs.ID, domain.StatusDown)

	statuses, err := svc.ResolveStatuses(ctx, 1)
	if err != nil {
		t.Fatalf("ResolveStatuses: %v", err)
	}
	if _, ok := statuses[theirs.ID]; ok {
		t.Fatalf("other user's group leaked into result: %+v", statuses)
	}
	if statuses[mine.ID] != domain.StatusUp {
		t.Fatalf("mine status = %v, want %v", statuses[mine.ID], domain.StatusUp)
	}
}

// --- group delete announces the monitors it re-homed -------------------

// grpFakeBus records everything published so tests can assert on the wire
// traffic rather than on a bare "no error".
type grpFakeBus struct {
	mu     sync.Mutex
	events []ports.Event
}

func (b *grpFakeBus) Publish(_ context.Context, e ports.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
	return nil
}
func (b *grpFakeBus) Subscribe(string) <-chan ports.Event { return nil }
func (b *grpFakeBus) Close()                              {}

// Deleting a group re-homes its monitors to the group's parent. If that move is
// not published, every already-open browser keeps rendering those monitors
// inside a folder that no longer exists — the delete "works" and the UI lies.
func TestMonitorGroupService_Delete_PublishesRehomedMonitors(t *testing.T) {
	ctx := context.Background()
	svc, monitors, _ := newGroupTestService()
	bus := &grpFakeBus{}
	svc.SetEventBus(bus)

	// parent > child, one monitor inside child, one monitor outside it entirely.
	parent := &domain.MonitorGroup{UserID: 1, Name: "parent", Condition: domain.GroupConditionWorstOfChildren}
	if err := svc.Create(ctx, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &domain.MonitorGroup{UserID: 1, Name: "child", ParentID: &parent.ID, Condition: domain.GroupConditionWorstOfChildren}
	if err := svc.Create(ctx, child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	inside := &domain.Monitor{UserID: 1, Name: "inside", GroupID: &child.ID}
	if err := monitors.Create(ctx, inside); err != nil {
		t.Fatalf("create inside: %v", err)
	}
	outside := &domain.Monitor{UserID: 1, Name: "outside"}
	if err := monitors.Create(ctx, outside); err != nil {
		t.Fatalf("create outside: %v", err)
	}

	if err := svc.Delete(ctx, child.ID); err != nil {
		t.Fatalf("delete child group: %v", err)
	}

	// The monitor must survive the folder delete, re-homed to the grandparent.
	survived, err := monitors.GetByID(ctx, inside.ID)
	if err != nil {
		t.Fatalf("monitor did not survive its group being deleted: %v", err)
	}
	if survived.GroupID == nil || *survived.GroupID != parent.ID {
		t.Fatalf("re-homed GroupID = %v, want parent %d", survived.GroupID, parent.ID)
	}

	// Exactly one monitor.update, for the monitor that actually moved.
	moved := make([]int64, 0, len(bus.events))
	for _, e := range bus.events {
		if e.Type != "monitor.update" {
			continue
		}
		m, ok := e.Payload.(*domain.Monitor)
		if !ok {
			t.Fatalf("monitor.update payload = %T, want *domain.Monitor", e.Payload)
		}
		moved = append(moved, m.ID)
		if m.GroupID == nil || *m.GroupID != parent.ID {
			t.Fatalf("published GroupID = %v, want parent %d", m.GroupID, parent.ID)
		}
	}
	if len(moved) != 1 || moved[0] != inside.ID {
		t.Fatalf("published monitor.update for %v, want exactly [%d] (the untouched monitor must not be announced)", moved, inside.ID)
	}
}
