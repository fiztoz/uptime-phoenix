package domain

import "time"

// UserPermission grants a single non-admin user VIEW access to exactly one
// resource: either a monitor (MonitorID set) or a monitor group (GroupID set).
//
// Exactly one of MonitorID / GroupID is non-nil — see Valid. A group grant may
// be recursive over the group tree; IncludeDescendants decides. Expansion of
// that tree is the AccessService's job, not the domain's.
//
// Permissions are additive. A grant by itself is view-only: there is no "deny"
// grant, and a grant never confers edit/delete access. The independent account-
// level capabilities a non-admin can hold live on domain.User
// (CanManageNotifications, CanManageMaintenance, CanViewExtensions,
// CanCreateMonitors, CanCreateGroups), not here. The one intentional composition is monitor creation: CanCreateMonitors is install-
// level, while group grants bound which folders the new monitor may be placed in.
//
// Note what that means for the two creation capabilities: they let a user make
// a monitor or group and then edit and delete it, but that follow-on right
// comes from OWNERSHIP (Monitor.UserID / MonitorGroup.UserID matching the
// user), never from a grant. A user auto-granted view of the thing they just
// created holds two independent things — a grant that makes it visible, and
// ownership that makes it editable. Granting somebody else view of it does not
// make them an owner.
//
// The two are independent but not unordered. Visibility is the outer gate: the
// HTTP layer checks "may I see this?" before "is it mine?" (see
// requireMonitorEditAccess), so revoking a creator's grant does not strip their
// ownership — CanEditMonitor still says yes — but it does put the resource out
// of reach, and the API answers 404. An admin who revokes a creator's grant on
// their own monitor is choosing to park it: nobody but an admin can touch it
// until the grant comes back. That is deliberate (an admin must be able to take
// something away completely) and it is reversible (re-grant and the owner has it
// again), but it is a real consequence of unticking a box, not a no-op.
type UserPermission struct {
	ID        int64
	UserID    int64
	MonitorID *int64
	GroupID   *int64
	// IncludeDescendants controls how far a GROUP grant reaches. It is
	// meaningless on a monitor grant — a monitor has no descendants — and the
	// AccessService reads it only on the group branch.
	//
	//	true  (deep):    the group, every descendant subgroup, and every monitor
	//	                 filed under any of them.
	//	false (shallow): the group and the monitors filed DIRECTLY in it;
	//	                 subgroups and their contents stay invisible.
	//
	// The zero value is false, which is NOT the default a grant should get:
	// every caller building a group grant must set this explicitly, and the DB
	// column defaults to true to preserve the always-recursive behavior that
	// predates it. See migration 011.
	IncludeDescendants bool
	CreatedAt          time.Time
}

// Valid reports whether the permission names exactly one resource. A row with
// both or neither set is meaningless and must never be persisted — the DB
// enforces the same rule with a CHECK constraint, this is the in-process guard
// so a bad grant is rejected before it reaches the driver.
func (p UserPermission) Valid() bool {
	return (p.MonitorID != nil) != (p.GroupID != nil)
}
