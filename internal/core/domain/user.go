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
	// CanCreateMonitors lets a non-admin create monitors. The creator is
	// recorded in Monitor.UserID and may then edit, clone and delete that
	// monitor — and only that monitor. The flag confers no power over monitors
	// somebody else created, however visible they are to this user: being able
	// to make monitors is not being able to touch other people's.
	CanCreateMonitors bool
	// CanCreateGroups lets a non-admin create monitor groups (folders), with
	// the same owner-scoped follow-on rights as CanCreateMonitors — see
	// MonitorGroup.UserID.
	CanCreateGroups bool
	Timezone        string
	TOTPSecret      string
	TOTPEnabled     bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
