package services

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- In-memory grant repo -------------------------------------------------

type accFakePermRepo struct {
	mu     sync.Mutex
	perms  map[int64]*domain.UserPermission
	nextID int64
	// listErr, when set, makes ListByUser fail. Used to prove the service fails
	// CLOSED (sees nothing) rather than open when the grant store is unreadable.
	listErr   error
	listCalls int
}

func newAccFakePermRepo() *accFakePermRepo {
	return &accFakePermRepo{perms: make(map[int64]*domain.UserPermission)}
}

func (r *accFakePermRepo) Grant(_ context.Context, p *domain.UserPermission) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.perms {
		if e.UserID == p.UserID && samePtr(e.MonitorID, p.MonitorID) && samePtr(e.GroupID, p.GroupID) {
			// Upsert, matching both SQL adapters' ON CONFLICT DO UPDATE: the row
			// keeps its identity and takes the new reach. Returning early without
			// this assignment would let a re-grant appear to change the reach in
			// tests while doing nothing — or the reverse.
			e.IncludeDescendants = p.IncludeDescendants
			p.ID = e.ID
			return nil
		}
	}
	r.nextID++
	cp := *p
	cp.ID = r.nextID
	cp.CreatedAt = time.Now().UTC()
	r.perms[cp.ID] = &cp
	p.ID = cp.ID
	return nil
}

func (r *accFakePermRepo) RevokeMonitor(_ context.Context, userID, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.perms {
		if p.UserID == userID && p.MonitorID != nil && *p.MonitorID == monitorID {
			delete(r.perms, id)
		}
	}
	return nil
}

func (r *accFakePermRepo) RevokeGroup(_ context.Context, userID, groupID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.perms {
		if p.UserID == userID && p.GroupID != nil && *p.GroupID == groupID {
			delete(r.perms, id)
		}
	}
	return nil
}

func (r *accFakePermRepo) RevokeAll(_ context.Context, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.perms {
		if p.UserID == userID {
			delete(r.perms, id)
		}
	}
	return nil
}

func (r *accFakePermRepo) ListByUser(_ context.Context, userID int64) ([]*domain.UserPermission, error) {
	r.mu.Lock()
	r.listCalls++
	r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.filter(func(p *domain.UserPermission) bool { return p.UserID == userID }), nil
}

func (r *accFakePermRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.UserPermission, error) {
	return r.filter(func(p *domain.UserPermission) bool {
		return p.MonitorID != nil && *p.MonitorID == monitorID
	}), nil
}

func (r *accFakePermRepo) ListByGroup(_ context.Context, groupID int64) ([]*domain.UserPermission, error) {
	return r.filter(func(p *domain.UserPermission) bool {
		return p.GroupID != nil && *p.GroupID == groupID
	}), nil
}

func (r *accFakePermRepo) filter(keep func(*domain.UserPermission) bool) []*domain.UserPermission {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.UserPermission, 0, len(r.perms))
	for _, p := range r.perms {
		if keep(p) {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func samePtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

var _ ports.UserPermissionRepository = (*accFakePermRepo)(nil)

// --- Harness --------------------------------------------------------------

type accessHarness struct {
	svc      *AccessService
	users    *inMemUserRepo
	perms    *accFakePermRepo
	groups   *grpFakeGroupRepo
	monitors *grpFakeMonitorRepo
}

func newAccessHarness(t *testing.T) *accessHarness {
	t.Helper()
	users := newInMemUserRepo()
	perms := newAccFakePermRepo()
	groups := newGrpFakeGroupRepo()
	monitors := newGrpFakeMonitorRepo()
	return &accessHarness{
		svc:      NewAccessService(users, perms, groups, monitors),
		users:    users,
		perms:    perms,
		groups:   groups,
		monitors: monitors,
	}
}

func (h *accessHarness) addUser(t *testing.T, name string, admin bool) int64 {
	t.Helper()
	u := &domain.User{Username: name, Active: true, IsAdmin: admin}
	if err := h.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u.ID
}

func (h *accessHarness) addGroup(t *testing.T, name string, parent *int64) int64 {
	t.Helper()
	g := &domain.MonitorGroup{UserID: 1, Name: name, ParentID: parent, Condition: domain.GroupConditionWorstOfChildren}
	if err := h.groups.Create(context.Background(), g); err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return g.ID
}

func (h *accessHarness) addMonitor(t *testing.T, name string, groupID *int64) int64 {
	t.Helper()
	m := &domain.Monitor{UserID: 1, Name: name, Type: "http", Active: true, Interval: 60, GroupID: groupID}
	if err := h.monitors.Create(context.Background(), m); err != nil {
		t.Fatalf("create monitor %s: %v", name, err)
	}
	return m.ID
}

// addMonitorOwnedBy is addMonitor with an explicit owner. The plain helpers hard-
// code UserID 1 (the admin), which is fine for visibility tests but useless for
// ownership ones — every monitor would belong to the same person.
func (h *accessHarness) addMonitorOwnedBy(t *testing.T, name string, owner int64) int64 {
	t.Helper()
	m := &domain.Monitor{UserID: owner, Name: name, Type: "http", Active: true, Interval: 60}
	if err := h.monitors.Create(context.Background(), m); err != nil {
		t.Fatalf("create monitor %s: %v", name, err)
	}
	return m.ID
}

func (h *accessHarness) addGroupOwnedBy(t *testing.T, name string, owner int64) int64 {
	t.Helper()
	g := &domain.MonitorGroup{UserID: owner, Name: name, Condition: domain.GroupConditionWorstOfChildren}
	if err := h.groups.Create(context.Background(), g); err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return g.ID
}

func idSet(ids []int64) map[int64]bool {
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func assertVisibleMonitors(t *testing.T, svc *AccessService, userID int64, want ...int64) {
	t.Helper()
	all, ids, err := svc.VisibleMonitorIDs(context.Background(), userID)
	if err != nil {
		t.Fatalf("VisibleMonitorIDs(%d): %v", userID, err)
	}
	if all {
		t.Fatalf("VisibleMonitorIDs(%d) returned all=true; want a bounded set", userID)
	}
	got := idSet(ids)
	if len(got) != len(want) {
		t.Fatalf("user %d sees monitors %v; want %v", userID, ids, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("user %d cannot see monitor %d; sees %v", userID, id, ids)
		}
	}
}

// --- Tests ----------------------------------------------------------------

// An admin is unrestricted: all=true, and the caller must apply NO id filter.
// This is the single-admin install, and it must keep working exactly as before.
func TestAccessService_AdminSeesEverything(t *testing.T) {
	h := newAccessHarness(t)
	admin := h.addUser(t, "admin", true)
	h.addMonitor(t, "m1", nil)
	h.addMonitor(t, "m2", nil)

	all, ids, err := h.svc.VisibleMonitorIDs(context.Background(), admin)
	if err != nil {
		t.Fatalf("VisibleMonitorIDs: %v", err)
	}
	if !all {
		t.Fatalf("admin got all=false (ids=%v); an admin must see every monitor in the install", ids)
	}

	allGroups, _, err := h.svc.VisibleGroupIDs(context.Background(), admin)
	if err != nil {
		t.Fatalf("VisibleGroupIDs: %v", err)
	}
	if !allGroups {
		t.Fatal("admin got all=false for groups; an admin must see every group")
	}

	// And every capability, without the flags being set.
	for name, fn := range map[string]func(context.Context, int64) (bool, error){
		"notifications": h.svc.CanManageNotifications,
		"maintenance":   h.svc.CanManageMaintenance,
		"extensions":    h.svc.CanViewExtensions,
		"view-all":      h.svc.CanViewAllMonitors,
	} {
		ok, err := fn(context.Background(), admin)
		if err != nil || !ok {
			t.Errorf("admin CanManage%s = (%v, %v); want (true, nil)", name, ok, err)
		}
	}
}

// A user with no grants sees NOTHING. This is the leak that RestrictToIDs exists
// to prevent: an empty allowlist must never be read as "no filter".
func TestAccessService_NoGrantsSeesNothing(t *testing.T) {
	h := newAccessHarness(t)
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)
	h.addMonitor(t, "m1", nil)
	h.addMonitor(t, "m2", nil)

	all, ids, err := h.svc.VisibleMonitorIDs(context.Background(), member)
	if err != nil {
		t.Fatalf("VisibleMonitorIDs: %v", err)
	}
	if all {
		t.Fatal("a user with no grants got all=true — every monitor in the install would be exposed")
	}
	if len(ids) != 0 {
		t.Fatalf("a user with no grants sees %v; want none", ids)
	}
	if len(ids) == 0 && ids == nil {
		t.Fatal("ids must be non-nil so a caller cannot confuse 'no grants' with 'not computed'")
	}

	can, err := h.svc.CanViewMonitor(context.Background(), member, 1)
	if err != nil || can {
		t.Errorf("CanViewMonitor(no grants) = (%v, %v); want (false, nil)", can, err)
	}
}

// A direct monitor grant makes exactly that monitor visible — and nothing else.
func TestAccessService_DirectMonitorGrant(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	granted := h.addMonitor(t, "granted", nil)
	other := h.addMonitor(t, "other", nil)

	if err := h.svc.GrantMonitor(ctx, member, granted); err != nil {
		t.Fatalf("GrantMonitor: %v", err)
	}

	assertVisibleMonitors(t, h.svc, member, granted)

	can, err := h.svc.CanViewMonitor(ctx, member, other)
	if err != nil || can {
		t.Errorf("CanViewMonitor(ungranted) = (%v, %v); want (false, nil)", can, err)
	}
}

// A GROUP grant expands recursively: the group, its subgroups (at any depth), and
// every monitor filed under any of them. A monitor in a sibling branch stays
// invisible.
func TestAccessService_GroupGrantExpandsToNestedSubgroupMonitors(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	//  root ─┬─ child ── grandchild
	//        └─ sibling
	root := h.addGroup(t, "root", nil)
	child := h.addGroup(t, "child", &root)
	grandchild := h.addGroup(t, "grandchild", &child)
	sibling := h.addGroup(t, "sibling", nil)

	mRoot := h.addMonitor(t, "in-root", &root)
	mChild := h.addMonitor(t, "in-child", &child)
	mGrand := h.addMonitor(t, "in-grandchild", &grandchild)
	mSibling := h.addMonitor(t, "in-sibling", &sibling)
	mLoose := h.addMonitor(t, "top-level", nil)

	if err := h.svc.GrantGroup(ctx, member, root, true); err != nil {
		t.Fatalf("GrantGroup: %v", err)
	}

	// Everything under root, at every depth — and nothing outside it.
	assertVisibleMonitors(t, h.svc, member, mRoot, mChild, mGrand)

	for _, hidden := range []int64{mSibling, mLoose} {
		can, err := h.svc.CanViewMonitor(ctx, member, hidden)
		if err != nil || can {
			t.Errorf("CanViewMonitor(monitor %d outside the granted tree) = (%v, %v); want (false, nil)", hidden, can, err)
		}
	}

	// The group tree the user sees must be the granted subtree, and must not
	// include the sibling branch.
	_, groupIDs, err := h.svc.VisibleGroupIDs(ctx, member)
	if err != nil {
		t.Fatalf("VisibleGroupIDs: %v", err)
	}
	got := idSet(groupIDs)
	for _, want := range []int64{root, child, grandchild} {
		if !got[want] {
			t.Errorf("group %d missing from the visible tree %v", want, groupIDs)
		}
	}
	if got[sibling] {
		t.Errorf("sibling group %d leaked into the visible tree %v", sibling, groupIDs)
	}
}

// A SHALLOW group grant (IncludeDescendants=false) reaches the folder and the
// monitors filed directly in it — and stops. The subgroup and everything in it
// stays invisible.
//
// This is the deep test's mirror image and they share a tree on purpose: the only
// difference between them is the flag, so if expansion ever stopped honoring it,
// exactly one of the two must fail.
func TestAccessService_ShallowGroupGrantDoesNotReachSubgroups(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	//  root ── child ── grandchild
	root := h.addGroup(t, "root", nil)
	child := h.addGroup(t, "child", &root)
	grandchild := h.addGroup(t, "grandchild", &child)

	mRoot := h.addMonitor(t, "in-root", &root)
	mChild := h.addMonitor(t, "in-child", &child)
	mGrand := h.addMonitor(t, "in-grandchild", &grandchild)

	if err := h.svc.GrantGroup(ctx, member, root, false); err != nil {
		t.Fatalf("GrantGroup(shallow): %v", err)
	}

	// The granted folder's own monitor, and nothing from below it.
	assertVisibleMonitors(t, h.svc, member, mRoot)
	for _, hidden := range []int64{mChild, mGrand} {
		can, err := h.svc.CanViewMonitor(ctx, member, hidden)
		if err != nil || can {
			t.Errorf("CanViewMonitor(monitor %d below a shallow grant) = (%v, %v); want (false, nil)", hidden, can, err)
		}
	}

	// The subgroups must not appear in the tree either.
	_, groupIDs, err := h.svc.VisibleGroupIDs(ctx, member)
	if err != nil {
		t.Fatalf("VisibleGroupIDs: %v", err)
	}
	got := idSet(groupIDs)
	if !got[root] {
		t.Errorf("the shallow-granted group %d is missing from the visible tree %v", root, groupIDs)
	}
	for _, hidden := range []int64{child, grandchild} {
		if got[hidden] {
			t.Errorf("subgroup %d leaked through a shallow grant: tree = %v", hidden, groupIDs)
		}
	}
}

// Monitor creation uses the reach of explicit group grants, not the broader
// set of groups made visible only so the frontend can render a coherent tree.
func TestAccessService_MonitorCreationIsLimitedToGrantedGroups(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)
	creator := &domain.User{Username: "creator", Active: true, CanCreateMonitors: true}
	if err := h.users.Create(ctx, creator); err != nil {
		t.Fatalf("create creator: %v", err)
	}

	root := h.addGroup(t, "root", nil)
	child := h.addGroup(t, "child", &root)
	grandchild := h.addGroup(t, "grandchild", &child)
	outside := h.addGroup(t, "outside", nil)

	if err := h.svc.GrantGroup(ctx, creator.ID, root, true); err != nil {
		t.Fatalf("GrantGroup(deep): %v", err)
	}

	for _, groupID := range []int64{root, child, grandchild} {
		allowed, err := h.svc.CanCreateMonitorInGroup(ctx, creator.ID, &groupID)
		if err != nil || !allowed {
			t.Errorf("CanCreateMonitorInGroup(%d) = (%v, %v); want (true, nil)", groupID, allowed, err)
		}
	}
	for name, groupID := range map[string]*int64{"top level": nil, "outside": &outside} {
		allowed, err := h.svc.CanCreateMonitorInGroup(ctx, creator.ID, groupID)
		if err != nil || allowed {
			t.Errorf("CanCreateMonitorInGroup(%s) = (%v, %v); want (false, nil)", name, allowed, err)
		}
	}

	// Explicit top-level capability opens group_id null without expanding groups.
	creator.CanCreateTopLevelMonitors = true
	if err := h.users.Update(ctx, creator); err != nil {
		t.Fatalf("enable top-level: %v", err)
	}
	h.svc.InvalidateUser(creator.ID)
	topOK, err := h.svc.CanCreateMonitorInGroup(ctx, creator.ID, nil)
	if err != nil || !topOK {
		t.Fatalf("CanCreateMonitorInGroup(top level with flag) = (%v, %v); want (true, nil)", topOK, err)
	}
	outsideOK, err := h.svc.CanCreateMonitorInGroup(ctx, creator.ID, &outside)
	if err != nil || outsideOK {
		t.Errorf("top-level flag must not open ungranted groups: (%v, %v)", outsideOK, err)
	}

	// Admins retain unrestricted placement, including top level.
	allowed, err := h.svc.CanCreateMonitorInGroup(ctx, admin, nil)
	if err != nil || !allowed {
		t.Errorf("CanCreateMonitorInGroup(admin, top level) = (%v, %v); want (true, nil)", allowed, err)
	}
}

// Top-level placement needs both create_monitors and the dedicated top-level flag.
func TestAccessService_TopLevelCreateRequiresBothFlags(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()

	onlyTop := &domain.User{Username: "only-top", Active: true, CanCreateTopLevelMonitors: true}
	if err := h.users.Create(ctx, onlyTop); err != nil {
		t.Fatalf("create: %v", err)
	}
	allowed, err := h.svc.CanCreateMonitorInGroup(ctx, onlyTop.ID, nil)
	if err != nil || allowed {
		t.Fatalf("top-level without create_monitors = (%v, %v); want (false, nil)", allowed, err)
	}
	onlyCreate := &domain.User{Username: "only-create", Active: true, CanCreateMonitors: true}
	if err := h.users.Create(ctx, onlyCreate); err != nil {
		t.Fatalf("create: %v", err)
	}
	allowed, err = h.svc.CanCreateMonitorInGroup(ctx, onlyCreate.ID, nil)
	if err != nil || allowed {
		t.Fatalf("create_monitors without top-level = (%v, %v); want (false, nil)", allowed, err)
	}
	both := &domain.User{
		Username: "both", Active: true,
		CanCreateMonitors: true, CanCreateTopLevelMonitors: true,
	}
	if err := h.users.Create(ctx, both); err != nil {
		t.Fatalf("create: %v", err)
	}
	allowed, err = h.svc.CanCreateMonitorInGroup(ctx, both.ID, nil)
	if err != nil || !allowed {
		t.Fatalf("both flags = (%v, %v); want (true, nil)", allowed, err)
	}
}

func TestAccessService_ShallowGrantLimitsMonitorCreationToExactGroup(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	creator := &domain.User{Username: "creator", Active: true, CanCreateMonitors: true}
	if err := h.users.Create(ctx, creator); err != nil {
		t.Fatalf("create creator: %v", err)
	}
	root := h.addGroup(t, "root", nil)
	child := h.addGroup(t, "child", &root)
	if err := h.svc.GrantGroup(ctx, creator.ID, root, false); err != nil {
		t.Fatalf("GrantGroup(shallow): %v", err)
	}

	rootAllowed, err := h.svc.CanCreateMonitorInGroup(ctx, creator.ID, &root)
	if err != nil || !rootAllowed {
		t.Fatalf("root allowed = (%v, %v); want (true, nil)", rootAllowed, err)
	}
	childAllowed, err := h.svc.CanCreateMonitorInGroup(ctx, creator.ID, &child)
	if err != nil || childAllowed {
		t.Errorf("child allowed = (%v, %v); want (false, nil)", childAllowed, err)
	}
}

// A direct monitor grant makes its container and ancestors visible for tree
// rendering, but must not turn those containers into monitor-creation targets.
func TestAccessService_ContainerVisibilityDoesNotAllowMonitorCreation(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	creator := &domain.User{Username: "creator", Active: true, CanCreateMonitors: true}
	if err := h.users.Create(ctx, creator); err != nil {
		t.Fatalf("create creator: %v", err)
	}
	root := h.addGroup(t, "root", nil)
	child := h.addGroup(t, "child", &root)
	monitor := h.addMonitor(t, "direct", &child)
	if err := h.svc.GrantMonitor(ctx, creator.ID, monitor); err != nil {
		t.Fatalf("GrantMonitor: %v", err)
	}

	_, visible, err := h.svc.VisibleGroupIDs(ctx, creator.ID)
	if err != nil {
		t.Fatalf("VisibleGroupIDs: %v", err)
	}
	if !idSet(visible)[root] || !idSet(visible)[child] {
		t.Fatalf("precondition: container tree is not visible: %v", visible)
	}
	for _, groupID := range []int64{root, child} {
		allowed, err := h.svc.CanCreateMonitorInGroup(ctx, creator.ID, &groupID)
		if err != nil || allowed {
			t.Errorf("creation in visible-only group %d = (%v, %v); want (false, nil)", groupID, allowed, err)
		}
	}
}

// Grants are additive and there is no deny: a shallow grant sitting inside a deep
// one cannot claw back what the deep grant already exposed. Worth pinning because
// the opposite ("the narrower grant wins") is a reasonable-sounding intuition,
// and someone will eventually try to use a shallow grant to punch a hole in a
// subtree. It does not work that way, and silently half-working would be worse.
func TestAccessService_ShallowGrantCannotNarrowADeepOne(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	root := h.addGroup(t, "root", nil)
	child := h.addGroup(t, "child", &root)
	mChild := h.addMonitor(t, "in-child", &child)
	grandchild := h.addGroup(t, "grandchild", &child)
	mGrand := h.addMonitor(t, "in-grandchild", &grandchild)

	if err := h.svc.SetPermissions(ctx, member, nil, []GroupGrant{
		{GroupID: root, IncludeDescendants: true},
		{GroupID: child, IncludeDescendants: false}, // does NOT hide grandchild
	}); err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}

	assertVisibleMonitors(t, h.svc, member, mChild, mGrand)
}

// A directly-granted monitor that lives inside a folder must drag that folder —
// and its ancestors — into the visible group set, or the frontend renders the
// monitor under a parent it was never told about.
func TestAccessService_MonitorGrantPullsInAncestorGroups(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	root := h.addGroup(t, "root", nil)
	child := h.addGroup(t, "child", &root)
	monitor := h.addMonitor(t, "deep", &child)

	if err := h.svc.GrantMonitor(ctx, member, monitor); err != nil {
		t.Fatalf("GrantMonitor: %v", err)
	}

	// Only the one monitor is visible…
	assertVisibleMonitors(t, h.svc, member, monitor)

	// …but the folders it sits in are, so the tree has no dangling parent.
	_, groupIDs, err := h.svc.VisibleGroupIDs(ctx, member)
	if err != nil {
		t.Fatalf("VisibleGroupIDs: %v", err)
	}
	got := idSet(groupIDs)
	if !got[child] || !got[root] {
		t.Fatalf("visible groups = %v; want the monitor's folder (%d) and its ancestor (%d)", groupIDs, child, root)
	}
}

// Seeing a GROUP is not permission to see everything inside it. A monitor grant
// that pulls a folder into view must not pull that folder's other monitors in
// with it.
func TestAccessService_VisibleGroupDoesNotWidenMonitorSet(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	folder := h.addGroup(t, "folder", nil)
	granted := h.addMonitor(t, "granted", &folder)
	sibling := h.addMonitor(t, "sibling-in-same-folder", &folder)

	if err := h.svc.GrantMonitor(ctx, member, granted); err != nil {
		t.Fatalf("GrantMonitor: %v", err)
	}

	assertVisibleMonitors(t, h.svc, member, granted)

	can, err := h.svc.CanViewMonitor(ctx, member, sibling)
	if err != nil || can {
		t.Errorf("CanViewMonitor(sibling in a visible folder) = (%v, %v); want (false, nil) — "+
			"folder visibility must never widen the monitor set", can, err)
	}
}

// A cycle in the stored group tree is bad data the group service refuses to
// create — but an authorization path must terminate on it regardless, not hang
// the request holding a lock. The test fails by TIMING OUT if expansion loops.
func TestAccessService_CycleInGroupTreeTerminates(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	// A → B → A, planted directly on the repo (Update bypasses the service's
	// cycle check, which is the point: this is corrupt data, not a legal edit).
	a := h.addGroup(t, "a", nil)
	b := h.addGroup(t, "b", &a)
	groupA, err := h.groups.GetByID(ctx, a)
	if err != nil {
		t.Fatalf("GetByID(a): %v", err)
	}
	groupA.ParentID = &b
	if err := h.groups.Update(ctx, groupA); err != nil {
		t.Fatalf("plant cycle: %v", err)
	}

	monitor := h.addMonitor(t, "in-a", &a)
	// Deep: the cycle this test plants is only reachable via the descendant walk,
	// so a shallow grant would never enter it and the test would pass vacuously.
	if err := h.svc.GrantGroup(ctx, member, a, true); err != nil {
		t.Fatalf("GrantGroup: %v", err)
	}

	done := make(chan struct{})
	var ids []int64
	var groupIDs []int64
	go func() {
		defer close(done)
		_, ids, _ = h.svc.VisibleMonitorIDs(ctx, member)
		_, groupIDs, _ = h.svc.VisibleGroupIDs(ctx, member)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("VisibleMonitorIDs did not terminate on a cyclic group tree — the expansion is looping")
	}

	if !idSet(ids)[monitor] {
		t.Errorf("visible monitors = %v; want the monitor inside the granted (cyclic) group", ids)
	}
	if !idSet(groupIDs)[a] || !idSet(groupIDs)[b] {
		t.Errorf("visible groups = %v; want both groups in the cycle", groupIDs)
	}
}

// Capability flags are independent of each other, and independent of monitor
// visibility.
func TestAccessService_CapabilitiesAreIndependent(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()

	notifier := &domain.User{Username: "notifier", Active: true, CanManageNotifications: true}
	if err := h.users.Create(ctx, notifier); err != nil {
		t.Fatalf("create: %v", err)
	}
	maintainer := &domain.User{Username: "maintainer", Active: true, CanManageMaintenance: true}
	if err := h.users.Create(ctx, maintainer); err != nil {
		t.Fatalf("create: %v", err)
	}
	extensionViewer := &domain.User{Username: "extension-viewer", Active: true, CanViewExtensions: true}
	if err := h.users.Create(ctx, extensionViewer); err != nil {
		t.Fatalf("create: %v", err)
	}

	cases := []struct {
		userID                        int64
		wantNotif, wantMain, wantExts bool
	}{
		{notifier.ID, true, false, false},
		{maintainer.ID, false, true, false},
		{extensionViewer.ID, false, false, true},
	}
	for _, tc := range cases {
		gotNotif, err := h.svc.CanManageNotifications(ctx, tc.userID)
		if err != nil || gotNotif != tc.wantNotif {
			t.Errorf("CanManageNotifications(%d) = (%v, %v); want %v", tc.userID, gotNotif, err, tc.wantNotif)
		}
		gotMain, err := h.svc.CanManageMaintenance(ctx, tc.userID)
		if err != nil || gotMain != tc.wantMain {
			t.Errorf("CanManageMaintenance(%d) = (%v, %v); want %v", tc.userID, gotMain, err, tc.wantMain)
		}
		gotExts, err := h.svc.CanViewExtensions(ctx, tc.userID)
		if err != nil || gotExts != tc.wantExts {
			t.Errorf("CanViewExtensions(%d) = (%v, %v); want %v", tc.userID, gotExts, err, tc.wantExts)
		}
		gotViewAll, err := h.svc.CanViewAllMonitors(ctx, tc.userID)
		if err != nil || gotViewAll {
			t.Errorf("CanViewAllMonitors(%d) = (%v, %v); want false", tc.userID, gotViewAll, err)
		}
		// An account-level capability is NOT monitor visibility: holding one grants
		// no monitors. CanViewAllMonitors is the exception — see
		// TestAccessService_ViewAllMonitorsSeesEverything.
		assertVisibleMonitors(t, h.svc, tc.userID)
	}
}

// CanViewAllMonitors is the read-only variant of admin visibility: all=true for
// monitors AND groups, including resources created after the flag was set, and
// with no write implied.
func TestAccessService_ViewAllMonitorsSeesEverything(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	viewer := &domain.User{Username: "viewer", Active: true, CanViewAllMonitors: true}
	if err := h.users.Create(ctx, viewer); err != nil {
		t.Fatalf("create: %v", err)
	}
	m1 := h.addMonitor(t, "m1", nil)
	g1 := h.addGroup(t, "g1", nil)
	m2 := h.addMonitor(t, "m2", &g1)

	all, ids, err := h.svc.VisibleMonitorIDs(ctx, viewer.ID)
	if err != nil {
		t.Fatalf("VisibleMonitorIDs: %v", err)
	}
	if !all {
		t.Fatalf("view-all got all=false (ids=%v); want every monitor in the install", ids)
	}

	allGroups, _, err := h.svc.VisibleGroupIDs(ctx, viewer.ID)
	if err != nil {
		t.Fatalf("VisibleGroupIDs: %v", err)
	}
	if !allGroups {
		t.Fatal("view-all got all=false for groups; want every group")
	}

	ok, err := h.svc.CanViewMonitor(ctx, viewer.ID, m1)
	if err != nil || !ok {
		t.Errorf("CanViewMonitor(m1) = (%v, %v); want (true, nil)", ok, err)
	}
	ok, err = h.svc.CanViewMonitor(ctx, viewer.ID, m2)
	if err != nil || !ok {
		t.Errorf("CanViewMonitor(m2) = (%v, %v); want (true, nil)", ok, err)
	}
	ok, err = h.svc.CanViewGroup(ctx, viewer.ID, g1)
	if err != nil || !ok {
		t.Errorf("CanViewGroup = (%v, %v); want (true, nil)", ok, err)
	}

	// Resources created after the flag still match — that is the gap a grant
	// expansion at login time cannot close.
	m3 := h.addMonitor(t, "m3", nil)
	ok, err = h.svc.CanViewMonitor(ctx, viewer.ID, m3)
	if err != nil || !ok {
		t.Errorf("CanViewMonitor(new monitor) = (%v, %v); want (true, nil)", ok, err)
	}

	effective, err := h.svc.CanViewAllMonitors(ctx, viewer.ID)
	if err != nil || !effective {
		t.Errorf("CanViewAllMonitors = (%v, %v); want (true, nil)", effective, err)
	}
}

// View-all is visibility only. The viewer may see someone else's monitor and
// still may not edit it — that remains admin or owner.
func TestAccessService_ViewAllMonitorsDoesNotGrantWrite(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)
	alice := h.addUser(t, "alice", false)
	viewer := &domain.User{Username: "viewer", Active: true, CanViewAllMonitors: true}
	if err := h.users.Create(ctx, viewer); err != nil {
		t.Fatalf("create: %v", err)
	}

	alices := h.addMonitorOwnedBy(t, "alices-monitor", alice)
	alicesGroup := h.addGroupOwnedBy(t, "alices-group", alice)

	ok, err := h.svc.CanViewMonitor(ctx, viewer.ID, alices)
	if err != nil || !ok {
		t.Fatalf("viewer should see alice's monitor: (%v, %v)", ok, err)
	}
	ok, err = h.svc.CanEditMonitor(ctx, viewer.ID, alices)
	if err != nil {
		t.Fatalf("CanEditMonitor: %v", err)
	}
	if ok {
		t.Error("view-all must not grant CanEditMonitor on someone else's monitor")
	}
	ok, err = h.svc.CanEditGroup(ctx, viewer.ID, alicesGroup)
	if err != nil {
		t.Fatalf("CanEditGroup: %v", err)
	}
	if ok {
		t.Error("view-all must not grant CanEditGroup on someone else's group")
	}
	ok, err = h.svc.CanCreateMonitors(ctx, viewer.ID)
	if err != nil || ok {
		t.Errorf("view-all CanCreateMonitors = (%v, %v); want (false, nil)", ok, err)
	}
	ok, err = h.svc.CanCreateMonitorInGroup(ctx, viewer.ID, nil)
	if err != nil || ok {
		t.Errorf("view-all CanCreateMonitorInGroup(top-level) = (%v, %v); want (false, nil)", ok, err)
	}
	ok, err = h.svc.CanCreateMonitorInGroup(ctx, viewer.ID, &alicesGroup)
	if err != nil || ok {
		t.Errorf("view-all CanCreateMonitorInGroup(alice's folder) = (%v, %v); want (false, nil)", ok, err)
	}
	ok, err = h.svc.CanEditGroupMetadata(ctx, viewer.ID, alicesGroup)
	if err != nil || ok {
		t.Errorf("view-all without CanEditGroupMetadata = (%v, %v); want (false, nil)", ok, err)
	}

	// Contrast: admin still writes, owner still writes their own.
	if ok, err = h.svc.CanEditMonitor(ctx, admin, alices); err != nil || !ok {
		t.Errorf("admin CanEditMonitor = (%v, %v); want (true, nil)", ok, err)
	}
	if ok, err = h.svc.CanEditMonitor(ctx, alice, alices); err != nil || !ok {
		t.Errorf("owner CanEditMonitor = (%v, %v); want (true, nil)", ok, err)
	}
}

// --- Ownership ------------------------------------------------------------

// The central rule of the ownership model: you may edit what you made, and only
// what you made. Being granted a view of something is not permission to change
// it, and a grant must never be mistaken for one.
func TestAccessService_CanEditMonitor_OwnerOnly(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)
	alice := h.addUser(t, "alice", false)
	bob := h.addUser(t, "bob", false)

	alices := h.addMonitorOwnedBy(t, "alices-monitor", alice)
	bobs := h.addMonitorOwnedBy(t, "bobs-monitor", bob)

	// Alice can see Bob's monitor. She still may not touch it — this grant is the
	// whole point of the test: view is not write.
	if err := h.svc.GrantMonitor(ctx, alice, bobs); err != nil {
		t.Fatalf("GrantMonitor: %v", err)
	}

	cases := []struct {
		name    string
		user    int64
		monitor int64
		want    bool
	}{
		{"owner edits their own", alice, alices, true},
		{"owner cannot edit another's, even when granted a view of it", alice, bobs, false},
		{"non-owner with no grant at all", bob, alices, false},
		{"admin edits anything", admin, alices, true},
		{"admin edits anything, again", admin, bobs, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.svc.CanEditMonitor(ctx, tc.user, tc.monitor)
			if err != nil {
				t.Fatalf("CanEditMonitor: %v", err)
			}
			if got != tc.want {
				t.Errorf("CanEditMonitor(user %d, monitor %d) = %v; want %v", tc.user, tc.monitor, got, tc.want)
			}
		})
	}
}

// Metadata editors may change non-structural fields on groups they can see,
// but never gain full edit/delete via the grant alone.
func TestAccessService_CanEditGroupMetadata_RequiresViewAndFlag(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	_ = h.addUser(t, "admin", true)
	editor := &domain.User{Username: "meta", Active: true, CanEditGroupMetadata: true}
	if err := h.users.Create(ctx, editor); err != nil {
		t.Fatalf("create editor: %v", err)
	}
	viewer := h.addUser(t, "viewer", false)
	folder := h.addGroup(t, "folder", nil)
	hidden := h.addGroup(t, "hidden", nil)

	if err := h.svc.GrantGroup(ctx, editor.ID, folder, true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := h.svc.GrantGroup(ctx, viewer, folder, true); err != nil {
		t.Fatalf("grant viewer: %v", err)
	}

	metaOK, err := h.svc.CanEditGroupMetadata(ctx, editor.ID, folder)
	if err != nil || !metaOK {
		t.Fatalf("metadata editor on granted group = (%v, %v); want (true, nil)", metaOK, err)
	}
	fullOK, err := h.svc.CanEditGroup(ctx, editor.ID, folder)
	if err != nil || fullOK {
		t.Fatalf("metadata editor full edit = (%v, %v); want (false, nil)", fullOK, err)
	}
	hiddenMeta, err := h.svc.CanEditGroupMetadata(ctx, editor.ID, hidden)
	if err != nil || hiddenMeta {
		t.Fatalf("metadata on ungranted group = (%v, %v); want (false, nil)", hiddenMeta, err)
	}
	viewOnly, err := h.svc.CanEditGroupMetadata(ctx, viewer, folder)
	if err != nil || viewOnly {
		t.Fatalf("viewer without flag = (%v, %v); want (false, nil)", viewOnly, err)
	}
}

// The group twin. Same rule, same trap.
func TestAccessService_CanEditGroup_OwnerOnly(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)
	alice := h.addUser(t, "alice", false)
	bob := h.addUser(t, "bob", false)

	alices := h.addGroupOwnedBy(t, "alices-folder", alice)
	bobs := h.addGroupOwnedBy(t, "bobs-folder", bob)

	if err := h.svc.GrantGroup(ctx, alice, bobs, true); err != nil {
		t.Fatalf("GrantGroup: %v", err)
	}

	for _, tc := range []struct {
		name  string
		user  int64
		group int64
		want  bool
	}{
		{"owner edits their own", alice, alices, true},
		{"owner cannot edit another's, even when granted a deep view of it", alice, bobs, false},
		{"admin edits anything", admin, bobs, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.svc.CanEditGroup(ctx, tc.user, tc.group)
			if err != nil {
				t.Fatalf("CanEditGroup: %v", err)
			}
			if got != tc.want {
				t.Errorf("CanEditGroup(user %d, group %d) = %v; want %v", tc.user, tc.group, got, tc.want)
			}
		})
	}
}

// Capabilities gate CREATION, ownership gates EDITING, and they are not wired to
// each other. Taking create_monitors away stops a user adding more monitors; it
// does not orphan the ones they already made.
func TestAccessService_CreateCapabilityIsIndependentOfOwnership(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()

	u := &domain.User{Username: "carol", Active: true, CanCreateMonitors: true}
	if err := h.users.Create(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	hers := h.addMonitorOwnedBy(t, "carols-monitor", u.ID)

	can, err := h.svc.CanCreateMonitors(ctx, u.ID)
	if err != nil || !can {
		t.Fatalf("CanCreateMonitors = (%v, %v); want (true, nil)", can, err)
	}

	// Revoke the capability. She keeps what she made.
	u.CanCreateMonitors = false
	if err := h.users.Update(ctx, u); err != nil {
		t.Fatalf("update user: %v", err)
	}
	h.svc.InvalidateUser(u.ID)

	can, err = h.svc.CanCreateMonitors(ctx, u.ID)
	if err != nil || can {
		t.Errorf("CanCreateMonitors after revoke = (%v, %v); want (false, nil)", can, err)
	}
	edit, err := h.svc.CanEditMonitor(ctx, u.ID, hers)
	if err != nil {
		t.Fatalf("CanEditMonitor: %v", err)
	}
	if !edit {
		t.Error("revoking create_monitors also stripped the owner's edit rights on a monitor they already created")
	}
}

// Admins hold every capability implicitly, with no flags set. Pinning this
// because the flags on an admin row really are false, and any check that reads
// them without the IsAdmin short-circuit would lock admins out.
func TestAccessService_AdminImpliesEveryCreateCapability(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true) // no capability flags set

	for name, fn := range map[string]func(context.Context, int64) (bool, error){
		"CanCreateMonitors":         h.svc.CanCreateMonitors,
		"CanCreateTopLevelMonitors": h.svc.CanCreateTopLevelMonitors,
		"CanCreateGroups":           h.svc.CanCreateGroups,
	} {
		got, err := fn(ctx, admin)
		if err != nil || !got {
			t.Errorf("%s(admin) = (%v, %v); want (true, nil)", name, got, err)
		}
	}
}

// SetPermissions REPLACES the grant set. Sending empty lists revokes everything —
// a merge-semantics implementation would make the last grant impossible to remove.
func TestAccessService_SetPermissionsReplacesTheWholeSet(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	m1 := h.addMonitor(t, "m1", nil)
	m2 := h.addMonitor(t, "m2", nil)
	g1 := h.addGroup(t, "g1", nil)

	if err := h.svc.SetPermissions(ctx, member, []int64{m1, m2}, []GroupGrant{{GroupID: g1, IncludeDescendants: true}}); err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}
	monitorIDs, groups, err := h.svc.ListPermissions(ctx, member)
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(monitorIDs) != 2 || len(groups) != 1 {
		t.Fatalf("after set: monitors=%v groups=%v; want 2 and 1", monitorIDs, groups)
	}

	// Narrow to a single monitor grant.
	if err := h.svc.SetPermissions(ctx, member, []int64{m2}, nil); err != nil {
		t.Fatalf("SetPermissions (narrow): %v", err)
	}
	assertVisibleMonitors(t, h.svc, member, m2)

	// Revoke everything.
	if err := h.svc.SetPermissions(ctx, member, nil, nil); err != nil {
		t.Fatalf("SetPermissions (revoke all): %v", err)
	}
	assertVisibleMonitors(t, h.svc, member)
}

// A grant naming a monitor that does not exist is rejected, and NOTHING is
// written — a typo must not half-apply and silently drop the grants that were
// already there.
func TestAccessService_SetPermissionsRejectsUnknownIDsAtomically(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)
	m1 := h.addMonitor(t, "m1", nil)

	if err := h.svc.SetPermissions(ctx, member, []int64{m1}, nil); err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}

	err := h.svc.SetPermissions(ctx, member, []int64{m1, 9999}, nil)
	if err == nil {
		t.Fatal("SetPermissions with an unknown monitor id succeeded; want a validation error")
	}

	// The pre-existing grant must survive the rejected call.
	assertVisibleMonitors(t, h.svc, member, m1)
}

// Every error path fails CLOSED. An unreadable grant store must make a user see
// nothing — never everything.
func TestAccessService_FailsClosedOnRepoError(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)
	h.addMonitor(t, "m1", nil)

	h.perms.listErr = context.DeadlineExceeded

	all, ids, err := h.svc.VisibleMonitorIDs(ctx, member)
	if err == nil {
		t.Fatal("VisibleMonitorIDs swallowed a repo failure; the caller must be told")
	}
	if all {
		t.Fatal("VisibleMonitorIDs returned all=true on a repo failure — a broken DB must not hand out the install")
	}
	if len(ids) != 0 {
		t.Fatalf("VisibleMonitorIDs returned %v on a repo failure; want an empty set", ids)
	}

	can, err := h.svc.CanViewMonitor(ctx, member, 1)
	if can {
		t.Fatalf("CanViewMonitor = true on a repo failure (err=%v); must fail closed", err)
	}
}

// An unknown user is not an admin and sees nothing.
func TestAccessService_UnknownUserIsNotAdminAndSeesNothing(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	h.addMonitor(t, "m1", nil)

	admin, err := h.svc.IsAdmin(ctx, 4242)
	if admin {
		t.Fatalf("IsAdmin(unknown user) = true (err=%v); must be false", err)
	}
	all, ids, _ := h.svc.VisibleMonitorIDs(ctx, 4242)
	if all || len(ids) != 0 {
		t.Fatalf("unknown user sees all=%v ids=%v; want nothing", all, ids)
	}
}

func TestAccessService_UserLookupIsCached(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)

	if _, err := h.svc.IsAdmin(ctx, admin); err != nil {
		t.Fatalf("IsAdmin: %v", err)
	}
	for range 20 {
		ok, err := h.svc.CanViewMonitor(ctx, admin, 1)
		if err != nil {
			t.Fatalf("CanViewMonitor: %v", err)
		}
		if !ok {
			t.Fatal("admin should see the monitor")
		}
	}
	if h.users.getByIDCalls != 1 {
		t.Fatalf("GetByID calls = %d; want 1 (20 CanViewMonitor must reuse the user cache)", h.users.getByIDCalls)
	}
}

func TestAccessService_UserCacheExpires(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)
	h.svc.userTTL = 15 * time.Millisecond

	if _, err := h.svc.IsAdmin(ctx, admin); err != nil {
		t.Fatalf("IsAdmin: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := h.svc.IsAdmin(ctx, admin); err != nil {
		t.Fatalf("IsAdmin after TTL: %v", err)
	}
	if h.users.getByIDCalls != 2 {
		t.Fatalf("GetByID calls = %d; want 2 after the user cache TTL elapsed", h.users.getByIDCalls)
	}
}

func TestAccessService_ScopeCacheInvalidatedOnGrant(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	member := h.addUser(t, "member", false)
	mon := h.addMonitor(t, "m1", nil)

	can, err := h.svc.CanViewMonitor(ctx, member, mon)
	if err != nil {
		t.Fatalf("CanViewMonitor: %v", err)
	}
	if can {
		t.Fatal("member saw a monitor before any grant")
	}
	firstLists := h.perms.listCalls

	if _, err := h.svc.CanViewMonitor(ctx, member, mon); err != nil {
		t.Fatalf("CanViewMonitor cached: %v", err)
	}
	if h.perms.listCalls != firstLists {
		t.Fatalf("ListByUser calls = %d after a cached lookup; want %d", h.perms.listCalls, firstLists)
	}

	if err := h.svc.GrantMonitor(ctx, member, mon); err != nil {
		t.Fatalf("GrantMonitor: %v", err)
	}
	can, err = h.svc.CanViewMonitor(ctx, member, mon)
	if err != nil {
		t.Fatalf("CanViewMonitor after grant: %v", err)
	}
	if !can {
		t.Fatal("grant did not take effect — scope cache must drop on GrantMonitor")
	}
}
