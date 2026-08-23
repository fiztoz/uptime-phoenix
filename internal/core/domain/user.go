package domain

import "time"

// User represents an authenticated user of the system.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Active       bool
	// IsAdmin grants access to admin-only endpoints (user management). The
	// first user created via the bootstrap Register flow is always an
	// admin; every other user defaults to false unless explicitly created
	// or promoted by an existing admin.
	IsAdmin bool
	// CanManageNotifications lets a non-admin create/update/delete notifications
	// and attach/detach them to monitors they can view. Admins always may,
	// regardless of this flag.
	CanManageNotifications bool
	// CanManageMaintenance lets a non-admin create/update/delete maintenance
	// windows and assign/unassign monitors they can view. Admins always may,
	// regardless of this flag.
	CanManageMaintenance bool
	// CanViewExtensions lets a non-admin discover and launch operator-registered
	// extension pages through Phoenix. Admins always may, regardless of this flag.
	// The extension's direct Ingress path must enforce its own authorization.
	CanViewExtensions bool
	// CanCreateMonitors lets a non-admin create monitors inside groups covered
	// by one of their group grants. The creator is recorded in Monitor.UserID
	// and may then edit, clone and delete that monitor — and only that monitor.
	// The flag confers no power over monitors somebody else created, however
	// visible they are to this user: being able to make monitors is not being
	// able to touch other people's.
	CanCreateMonitors bool
	// CanCreateTopLevelMonitors lets a non-admin who also holds CanCreateMonitors
	// place new monitors outside any group (group_id null). Without this flag,
	// non-admin creates are limited to granted groups. Admins always may.
	// Independent of grants: it does not expand which groups the user can see.
	CanCreateTopLevelMonitors bool
	// CanCreateGroups lets a non-admin create monitor groups (folders), with
	// the same owner-scoped follow-on rights as CanCreateMonitors — see
	// MonitorGroup.UserID.
	CanCreateGroups bool
	// CanEditGroupMetadata lets a non-admin edit metadata on groups they can
	// VIEW (description, owner/contact, condition, threshold, weight, collapsed).
	// It does NOT allow renaming, re-parenting, or deleting a group — those stay
	// admin/creator-only via CanEditGroup. Independent of CanCreateGroups.
	CanEditGroupMetadata bool
	Timezone             string
	TOTPSecret           string
	TOTPEnabled          bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}
