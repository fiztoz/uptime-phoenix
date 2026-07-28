package services

import (
	"context"
	"fmt"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AccessService is the single authorization choke point for Phoenix.
//
// Every "may this user see / do this?" question in the HTTP and WebSocket layers
// resolves here, so the permission model exists in exactly one place instead of
// being re-derived (and eventually mis-derived) per handler.
//
// The model:
//
//   - Admin (domain.User.IsAdmin) — unrestricted. Sees every monitor and group in
//     the install regardless of owner, and may do anything. VisibleMonitorIDs and
//     VisibleGroupIDs return all=true for an admin; callers must then apply NO id
//     filter, not an empty one.
//
//   - Non-admin — sees ONLY what has been granted, via domain.UserPermission:
//     a direct monitor grant, or a group grant, which reaches either the whole
//     subtree or just the group's own monitors depending on the grant's
//     IncludeDescendants flag. See resolveScope.
//
//   - Capabilities — four independent flags on the user (CanManageNotifications,
//     CanManageMaintenance, CanCreateMonitors, CanCreateGroups) are the only write
//     powers a non-admin can hold. Admins implicitly hold all of them.
//
//   - Ownership — a non-admin who creates a monitor or group (having held the
//     matching create capability) owns it: Monitor.UserID / MonitorGroup.UserID
//     record the creator, and CanEditMonitor / CanEditGroup let an owner update,
//     clone and delete their own. Ownership is the ONLY route to writing a
//     monitor or group without being an admin.
//
// The two ways to reach a resource are deliberately independent, and conflating
// them is the mistake this file exists to prevent:
//
//	a GRANT answers "may I SEE this?" — never "may I change it?"
//	OWNERSHIP answers "may I change this?" — it is not a grant and cannot be
//	  revoked by editing someone's permissions.
//
// So a user granted view of fifty monitors can still edit only the ones they
// created, and revoking a creator's auto-grant hides their own monitor from
// their list without handing anyone else the right to touch it.
//
// Everything here fails CLOSED. A user that cannot be loaded is not an admin and
// sees nothing; a repo that is not wired contributes no visibility. There is no
// path through this file where an error or a missing dependency widens access.
type AccessService struct {
	users    ports.UserRepository
	perms    ports.UserPermissionRepository
	groups   ports.MonitorGroupRepository
	monitors ports.MonitorRepository
}

// NewAccessService creates the access service.
//
// users and perms are required. groups and monitors may be nil, in which case
// group-grant expansion degrades to "resolves to nothing" — a fail-closed
// degradation (a user sees FEWER monitors, never more). Production wiring passes
// all four; see internal/bootstrap/run.go.
func NewAccessService(
	users ports.UserRepository,
	perms ports.UserPermissionRepository,
	groups ports.MonitorGroupRepository,
	monitors ports.MonitorRepository,
) *AccessService {
	return &AccessService{users: users, perms: perms, groups: groups, monitors: monitors}
}

// maxGroupAncestorWalk bounds the ParentID walk used when pulling in the
// ancestors of a visible group, so a cycle in the stored tree (bad data — the
// group service refuses to create one) cannot spin forever.
const maxGroupAncestorWalk = 64

// --- User-level questions -------------------------------------------------

// IsAdmin reports whether the user holds the admin flag. A user that cannot be
// loaded is not an admin.
func (s *AccessService) IsAdmin(ctx context.Context, userID int64) (bool, error) {
	u, err := s.user(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.IsAdmin, nil
}

// CanManageNotifications reports whether the user may create/update/delete
// notifications and attach them to monitors they can view. Admins always may.
func (s *AccessService) CanManageNotifications(ctx context.Context, userID int64) (bool, error) {
	u, err := s.user(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.IsAdmin || u.CanManageNotifications, nil
}

// CanManageMaintenance reports whether the user may create/update/delete
// maintenance windows and assign monitors they can view. Admins always may.
func (s *AccessService) CanManageMaintenance(ctx context.Context, userID int64) (bool, error) {
	u, err := s.user(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.IsAdmin || u.CanManageMaintenance, nil
}

// CanCreateMonitors reports whether the user may create monitors. Admins always
// may.
//
// This answers only "may you make a NEW one?". What the user may then do to it
// is an ownership question — see CanEditMonitor.
func (s *AccessService) CanCreateMonitors(ctx context.Context, userID int64) (bool, error) {
	u, err := s.user(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.IsAdmin || u.CanCreateMonitors, nil
}

// CanCreateGroups reports whether the user may create monitor groups (folders).
// Admins always may. The group twin of CanCreateMonitors.
func (s *AccessService) CanCreateGroups(ctx context.Context, userID int64) (bool, error) {
	u, err := s.user(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.IsAdmin || u.CanCreateGroups, nil
}

// --- Ownership ------------------------------------------------------------

// CanEditMonitor reports whether the user may UPDATE, CLONE or DELETE one
// specific monitor.
//
// Two ways in, and no third:
//
//	admin                     — may edit anything;
//	monitor.UserID == userID  — the creator may edit what they created.
//
// Note what is deliberately absent: a grant. A user who has been granted view of
// a monitor — directly or through a group — gets nothing from it here. That
// separation is the point; see the type doc.
//
// The create capability is not consulted either, and must not be: it gates
// making NEW monitors, and a user who has since had it taken away still owns the
// ones they already made. Revoking CanCreateMonitors stops them adding more, it
// does not orphan their existing monitors.
//
// Fails closed: a monitor that cannot be loaded is not editable, and the error is
// returned rather than swallowed so the caller can tell "no such monitor" from
// "not yours".
func (s *AccessService) CanEditMonitor(ctx context.Context, userID, monitorID int64) (bool, error) {
	admin, err := s.IsAdmin(ctx, userID)
	if err != nil {
		return false, err
	}
	if admin {
		return true, nil
	}
	if s.monitors == nil {
		return false, nil // fail closed: no monitor store wired => cannot prove ownership
	}
	m, err := s.monitors.GetByID(ctx, monitorID)
	if err != nil {
		return false, fmt.Errorf("access service: load monitor %d: %w", monitorID, err)
	}
	return m.UserID == userID, nil
}

// CanEditGroup reports whether the user may UPDATE or DELETE one specific
// monitor group. The group twin of CanEditMonitor, with the same rules: admin or
// creator, never a grant.
func (s *AccessService) CanEditGroup(ctx context.Context, userID, groupID int64) (bool, error) {
	admin, err := s.IsAdmin(ctx, userID)
	if err != nil {
		return false, err
	}
	if admin {
		return true, nil
	}
	if s.groups == nil {
		return false, nil // fail closed: no group store wired => cannot prove ownership
	}
	g, err := s.groups.GetByID(ctx, groupID)
	if err != nil {
		return false, fmt.Errorf("access service: load monitor group %d: %w", groupID, err)
	}
	return g.UserID == userID, nil
}

func (s *AccessService) user(ctx context.Context, userID int64) (*domain.User, error) {
	if s.users == nil || userID <= 0 {
		return nil, fmt.Errorf("access service: %w: no such user", domain.ErrNotFound)
	}
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("access service: load user %d: %w", userID, err)
	}
	return u, nil
}

// --- Visibility -----------------------------------------------------------

// VisibleMonitorIDs resolves the set of monitors a user may see.
//
// all=true means "every monitor in the install" (admin) — the caller must apply
// no id filter at all. all=false means ids is the complete allowlist, and an
// EMPTY ids with all=false means the user may see nothing. That distinction is
// the whole ballgame: see ports.MonitorFilter.RestrictToIDs.
//
// The returned slice is always non-nil so a caller cannot accidentally treat
// "no grants" as "not computed".
func (s *AccessService) VisibleMonitorIDs(ctx context.Context, userID int64) (bool, []int64, error) {
	admin, err := s.IsAdmin(ctx, userID)
	if err != nil {
		return false, []int64{}, err
	}
	if admin {
		return true, []int64{}, nil
	}

	scope, err := s.resolveScope(ctx, userID)
	if err != nil {
		return false, []int64{}, err
	}
	return false, scope.monitorIDs, nil
}

// CanViewMonitor reports whether the user may see one specific monitor.
func (s *AccessService) CanViewMonitor(ctx context.Context, userID, monitorID int64) (bool, error) {
	all, ids, err := s.VisibleMonitorIDs(ctx, userID)
	if err != nil {
		return false, err
	}
	if all {
		return true, nil
	}
	for _, id := range ids {
		if id == monitorID {
			return true, nil
		}
	}
	return false, nil
}

// VisibleGroupIDs resolves the set of monitor groups a user may see.
//
// Same all/ids contract as VisibleMonitorIDs. For a non-admin the set is built
// so the tree it describes always renders coherently:
//
//   - every granted group, plus all of its descendant subgroups;
//   - every group that directly contains a monitor the user can see (otherwise a
//     directly-granted monitor would be filed under a folder the user cannot see);
//   - every ancestor of any group already in the set, so there are no dangling
//     parents in the tree the frontend has to draw.
//
// Being able to SEE a group is not permission to see everything in it: group
// visibility never widens the monitor set, which is computed independently above.
func (s *AccessService) VisibleGroupIDs(ctx context.Context, userID int64) (bool, []int64, error) {
	admin, err := s.IsAdmin(ctx, userID)
	if err != nil {
		return false, []int64{}, err
	}
	if admin {
		return true, []int64{}, nil
	}

	scope, err := s.resolveScope(ctx, userID)
	if err != nil {
		return false, []int64{}, err
	}
	return false, scope.groupIDs, nil
}

// CanViewGroup reports whether the user may see one specific group.
func (s *AccessService) CanViewGroup(ctx context.Context, userID, groupID int64) (bool, error) {
	all, ids, err := s.VisibleGroupIDs(ctx, userID)
	if err != nil {
		return false, err
	}
	if all {
		return true, nil
	}
	for _, id := range ids {
		if id == groupID {
			return true, nil
		}
	}
	return false, nil
}

// visibleScope is one non-admin user's fully-resolved view: which monitors and
// which groups they may see. Both slices are non-nil and deduplicated.
type visibleScope struct {
	monitorIDs []int64
	groupIDs   []int64
}

// resolveScope expands a non-admin's grants into concrete monitor and group id
// sets. It is the only place the group tree is walked, and it is cycle-safe: the
// descendant walk is a BFS with a visited set, and the ancestor walk is bounded.
func (s *AccessService) resolveScope(ctx context.Context, userID int64) (visibleScope, error) {
	scope := visibleScope{monitorIDs: []int64{}, groupIDs: []int64{}}

	if s.perms == nil {
		return scope, nil // fail closed: no grant store wired => no grants
	}
	grants, err := s.perms.ListByUser(ctx, userID)
	if err != nil {
		return scope, fmt.Errorf("access service: list grants for user %d: %w", userID, err)
	}
	if len(grants) == 0 {
		return scope, nil
	}

	// Group grants split by reach. deepGroups get the descendant walk below;
	// shallowGroups are merged in afterwards WITHOUT it, so they contribute
	// themselves and (via the monitor pass further down, which tests membership
	// in the final visible set) the monitors filed directly in them — but never
	// a subgroup.
	visibleMonitors := make(map[int64]bool)
	deepGroups := make(map[int64]bool)
	shallowGroups := make(map[int64]bool)
	for _, g := range grants {
		switch {
		case g.MonitorID != nil:
			visibleMonitors[*g.MonitorID] = true
		case g.GroupID != nil:
			if g.IncludeDescendants {
				deepGroups[*g.GroupID] = true
			} else {
				shallowGroups[*g.GroupID] = true
			}
		}
	}

	// Load the whole group tree once. ListAll (not List) because a non-admin's
	// granted group is owned by the admin who created it. This is needed even when
	// the user holds only monitor grants: a granted monitor sitting in a nested
	// folder still needs that folder's ancestors pulled in, or the frontend gets a
	// group with a parent it was never told about.
	var allGroups []*domain.MonitorGroup
	if s.groups != nil {
		allGroups, err = s.groups.ListAll(ctx)
		if err != nil {
			return scope, fmt.Errorf("access service: list groups: %w", err)
		}
	}
	tree := newGroupTree(allGroups)

	// Deep grants pull in their whole subtree; shallow grants join the set as
	// themselves and stop there. The union happens BEFORE the monitor pass below
	// on purpose — that pass makes every monitor filed under a visible group
	// visible, which is exactly the reach a shallow grant should have (its own
	// monitors, none of its subgroups'). Contrast containerGroups further down,
	// which must be merged AFTER that pass for the opposite reason.
	//
	// A shallow grant on a group that is already inside a deep grant's subtree is
	// redundant, not contradictory: the deep grant already exposed the subtree,
	// and a union cannot take that back. Grants are additive and there is no
	// "deny" — a shallow grant narrows nothing that another grant has widened.
	visibleGroups := tree.expand(deepGroups)
	for id := range shallowGroups {
		visibleGroups[id] = true
	}

	// Monitors: the direct grants, plus every monitor filed under a group inside
	// the expanded set. Listing all monitors once beats a query per group.
	//
	// The groups that become visible because they CONTAIN a visible monitor are
	// collected separately and merged only after the loop. Merging them into
	// visibleGroups inside the loop would be a real leak: adding folder F while
	// iterating means a later monitor that happens to sit in F would match the
	// "monitor in a visible group" test and become visible too — so granting one
	// monitor would silently expose every sibling in its folder.
	containerGroups := make(map[int64]bool)
	if s.monitors != nil {
		monitors, mErr := s.monitors.List(ctx, ports.MonitorFilter{})
		if mErr != nil {
			return scope, fmt.Errorf("access service: list monitors: %w", mErr)
		}
		for _, m := range monitors {
			if m.GroupID != nil && visibleGroups[*m.GroupID] {
				visibleMonitors[m.ID] = true
			}
		}
		// Second pass, over the now-final monitor set: a group holding a monitor the
		// user can see must itself be visible, or that monitor renders under a folder
		// the frontend was never told about.
		for _, m := range monitors {
			if visibleMonitors[m.ID] && m.GroupID != nil {
				containerGroups[*m.GroupID] = true
			}
		}
	}
	for id := range containerGroups {
		visibleGroups[id] = true
	}

	tree.addAncestors(visibleGroups)

	scope.monitorIDs = keys(visibleMonitors)
	scope.groupIDs = keys(visibleGroups)
	return scope, nil
}

// groupTree is a read-only index over every group in the install, used to expand
// group grants downward (descendants) and to close a visible set upward
// (ancestors). Both walks are cycle-safe: the stored tree is supposed to be
// acyclic (MonitorGroupService.validateParent refuses to create a cycle), but
// this is an authorization path and must terminate on bad data regardless.
type groupTree struct {
	byID       map[int64]*domain.MonitorGroup
	childrenOf map[int64][]*domain.MonitorGroup
}

func newGroupTree(groups []*domain.MonitorGroup) *groupTree {
	t := &groupTree{
		byID:       make(map[int64]*domain.MonitorGroup, len(groups)),
		childrenOf: make(map[int64][]*domain.MonitorGroup, len(groups)),
	}
	for _, g := range groups {
		t.byID[g.ID] = g
		if g.ParentID != nil {
			t.childrenOf[*g.ParentID] = append(t.childrenOf[*g.ParentID], g)
		}
	}
	return t
}

// expand returns the granted groups plus every descendant of each, as a set.
// BFS with a visited set: a cycle (A parent of B, B parent of A) terminates
// instead of hanging the request.
func (t *groupTree) expand(granted map[int64]bool) map[int64]bool {
	visible := make(map[int64]bool, len(granted))
	queue := make([]int64, 0, len(granted))
	for id := range granted {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visible[id] {
			continue
		}
		visible[id] = true
		for _, child := range t.childrenOf[id] {
			if !visible[child.ID] {
				queue = append(queue, child.ID)
			}
		}
	}
	return visible
}

// addAncestors adds, in place, every ancestor of every group already in the set,
// so the tree the frontend draws has no dangling parent. The per-chain walk is
// bounded by maxGroupAncestorWalk so a cycle cannot spin forever.
func (t *groupTree) addAncestors(visible map[int64]bool) {
	seeds := keys(visible)
	for _, id := range seeds {
		g := t.byID[id]
		for depth := 0; g != nil && g.ParentID != nil && depth < maxGroupAncestorWalk; depth++ {
			parentID := *g.ParentID
			if visible[parentID] {
				break // already in the set — and so is the rest of the chain above it
			}
			visible[parentID] = true
			g = t.byID[parentID]
		}
	}
}

// keys returns the true-valued keys of a set as a non-nil slice. Order is
// unspecified; every caller either filters a DB query with it or membership-tests
// it, so a stable order buys nothing.
func keys(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for k, v := range set {
		if v {
			out = append(out, k)
		}
	}
	return out
}

// --- Grant administration (admin API) -------------------------------------

// GroupGrant is one group grant as the admin API exchanges it: which folder, and
// how far down it reaches. It exists because a group grant is no longer just an
// id — see domain.UserPermission.IncludeDescendants.
type GroupGrant struct {
	GroupID int64
	// IncludeDescendants deep-grants the whole subtree. See
	// domain.UserPermission.IncludeDescendants for the exact reach of each mode.
	IncludeDescendants bool
}

// ListPermissions returns what a user has been granted directly: the monitor ids
// and the group grants, unexpanded. This is what the admin UI edits — the
// expanded view lives in VisibleMonitorIDs / VisibleGroupIDs.
//
// Both slices are non-nil.
func (s *AccessService) ListPermissions(ctx context.Context, userID int64) ([]int64, []GroupGrant, error) {
	monitorIDs := []int64{}
	groups := []GroupGrant{}
	if s.perms == nil {
		return monitorIDs, groups, nil
	}
	grants, err := s.perms.ListByUser(ctx, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("access service: list permissions for user %d: %w", userID, err)
	}
	for _, g := range grants {
		switch {
		case g.MonitorID != nil:
			monitorIDs = append(monitorIDs, *g.MonitorID)
		case g.GroupID != nil:
			groups = append(groups, GroupGrant{GroupID: *g.GroupID, IncludeDescendants: g.IncludeDescendants})
		}
	}
	return monitorIDs, groups, nil
}

// SetPermissions replaces a user's entire grant set with the supplied ids. It is
// the write half of GET/PUT /api/users/:id/permissions: the admin form always
// submits the complete set, so this is a replace, not a merge. Passing two empty
// slices revokes everything.
//
// Referenced monitors and groups must exist; a request naming an unknown id is a
// validation error and NOTHING is written, so a typo cannot silently half-apply.
func (s *AccessService) SetPermissions(ctx context.Context, userID int64, monitorIDs []int64, groups []GroupGrant) error {
	if s.perms == nil {
		return fmt.Errorf("access service: set permissions: no permission repository wired")
	}
	if _, err := s.user(ctx, userID); err != nil {
		return err
	}

	monitorIDs = dedupe(monitorIDs)
	groups = dedupeGroupGrants(groups)

	// Validate before mutating anything.
	for _, id := range monitorIDs {
		if s.monitors == nil {
			break
		}
		if _, err := s.monitors.GetByID(ctx, id); err != nil {
			return fmt.Errorf("access service: %w: monitor %d not found", domain.ErrValidation, id)
		}
	}
	for _, g := range groups {
		if s.groups == nil {
			break
		}
		if _, err := s.groups.GetByID(ctx, g.GroupID); err != nil {
			return fmt.Errorf("access service: %w: monitor group %d not found", domain.ErrValidation, g.GroupID)
		}
	}

	if err := s.perms.RevokeAll(ctx, userID); err != nil {
		return fmt.Errorf("access service: set permissions: revoke existing: %w", err)
	}
	for i := range monitorIDs {
		id := monitorIDs[i]
		if err := s.grant(ctx, &domain.UserPermission{UserID: userID, MonitorID: &id}); err != nil {
			return err
		}
	}
	for i := range groups {
		g := groups[i]
		if err := s.grant(ctx, &domain.UserPermission{
			UserID:             userID,
			GroupID:            &g.GroupID,
			IncludeDescendants: g.IncludeDescendants,
		}); err != nil {
			return err
		}
	}
	return nil
}

// GrantMonitor gives a user view access to one monitor. Idempotent.
func (s *AccessService) GrantMonitor(ctx context.Context, userID, monitorID int64) error {
	return s.grant(ctx, &domain.UserPermission{UserID: userID, MonitorID: &monitorID})
}

// GrantGroup gives a user view access to one group. includeDescendants decides
// whether the grant also covers the group's subgroups and their monitors, or
// stops at the monitors filed directly in it — see
// domain.UserPermission.IncludeDescendants. Idempotent.
func (s *AccessService) GrantGroup(ctx context.Context, userID, groupID int64, includeDescendants bool) error {
	return s.grant(ctx, &domain.UserPermission{
		UserID:             userID,
		GroupID:            &groupID,
		IncludeDescendants: includeDescendants,
	})
}

// RevokeMonitor removes a user's direct grant on one monitor. It does NOT remove
// visibility the user derives from a group grant that contains the monitor —
// revoke the group for that.
func (s *AccessService) RevokeMonitor(ctx context.Context, userID, monitorID int64) error {
	if s.perms == nil {
		return nil
	}
	if err := s.perms.RevokeMonitor(ctx, userID, monitorID); err != nil {
		return fmt.Errorf("access service: revoke monitor grant: %w", err)
	}
	return nil
}

// RevokeGroup removes a user's grant on one group (and with it every monitor the
// user could only see through that group).
func (s *AccessService) RevokeGroup(ctx context.Context, userID, groupID int64) error {
	if s.perms == nil {
		return nil
	}
	if err := s.perms.RevokeGroup(ctx, userID, groupID); err != nil {
		return fmt.Errorf("access service: revoke group grant: %w", err)
	}
	return nil
}

func (s *AccessService) grant(ctx context.Context, p *domain.UserPermission) error {
	if !p.Valid() {
		return fmt.Errorf("access service: %w: a grant must name exactly one of monitor_id / group_id", domain.ErrValidation)
	}
	if s.perms == nil {
		return fmt.Errorf("access service: grant: no permission repository wired")
	}
	if err := s.perms.Grant(ctx, p); err != nil {
		return fmt.Errorf("access service: grant: %w", err)
	}
	return nil
}

// dedupeGroupGrants collapses repeated group ids, keeping the FIRST occurrence.
//
// A payload naming the same folder twice with different reaches ("group 4 deep,
// group 4 shallow") is contradictory, and the DB would reject the second insert
// anyway on idx_user_permissions_user_group. Resolving it here — deterministically,
// and before RevokeAll has fired — turns what would be a half-applied write into
// a coherent one. First-wins rather than widest-wins: the admin UI cannot produce
// a duplicate at all, so this is defense against a hand-rolled API call, and
// silently upgrading such a call to the more permissive of the two reaches is the
// wrong instinct in an authorization path.
func dedupeGroupGrants(groups []GroupGrant) []GroupGrant {
	seen := make(map[int64]struct{}, len(groups))
	out := make([]GroupGrant, 0, len(groups))
	for _, g := range groups {
		if _, dup := seen[g.GroupID]; dup {
			continue
		}
		seen[g.GroupID] = struct{}{}
		out = append(out, g)
	}
	return out
}

func dedupe(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
