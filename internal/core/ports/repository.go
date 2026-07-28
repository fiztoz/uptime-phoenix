// Package ports defines the interfaces that adapters must implement.
// No implementations, no external imports — only stdlib and domain types.
package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// MonitorFilter defines criteria for listing monitors.
type MonitorFilter struct {
	UserID  int64
	Active  *bool
	Type    string
	TagID   int64
	TagName string
	Search  string
	// GroupID restricts the listing to monitors filed under this group.
	// Nil means "any group"; use GroupIDIsNull for "top-level only".
	GroupID *int64
	// GroupIDIsNull restricts the listing to monitors in no group at all.
	GroupIDIsNull bool

	// RestrictToIDs turns MonitorIDs into a hard allowlist. This is the RBAC
	// gate for a non-admin listing, so its semantics are deliberately explicit
	// rather than inferred from the slice:
	//
	//	RestrictToIDs=false               → MonitorIDs is ignored; no ID filter.
	//	RestrictToIDs=true, MonitorIDs=[] → NO monitors are visible: the query
	//	                                    MUST return zero rows.
	//	RestrictToIDs=true, MonitorIDs=[…]→ only those ids.
	//
	// A nil-vs-empty slice check would be a data leak waiting to happen: a user
	// with zero grants produces an empty ID set, and an implementation that read
	// "empty means no filter" would hand them every monitor in the install.
	// Implementations MUST branch on RestrictToIDs, never on len(MonitorIDs).
	RestrictToIDs bool
	MonitorIDs    []int64

	Limit  int
	Offset int
}

// Aggregate1m is a 1-minute aggregation bucket.
type Aggregate1m struct {
	MonitorID    int64
	Bucket       time.Time
	UpCount      int
	DownCount    int
	PendingCount int
	MaintCount   int
	AvgPing      float64
	MinPing      int
	MaxPing      int
	TotalChecks  int
}

// Aggregate1h is a 1-hour aggregation bucket.
type Aggregate1h struct {
	MonitorID    int64
	Bucket       time.Time
	UpCount      int
	DownCount    int
	PendingCount int
	MaintCount   int
	AvgPing      float64
	MinPing      int
	MaxPing      int
	TotalChecks  int
}

// Aggregate1d is a 1-day aggregation bucket.
type Aggregate1d struct {
	MonitorID    int64
	Bucket       time.Time
	UpCount      int
	DownCount    int
	PendingCount int
	MaintCount   int
	AvgPing      float64
	MinPing      int
	MaxPing      int
	TotalChecks  int
}

// MonitorRepository defines persistence operations for monitors.
type MonitorRepository interface {
	Create(ctx context.Context, m *domain.Monitor) error
	GetByID(ctx context.Context, id int64) (*domain.Monitor, error)
	// GetByPushToken returns the monitor identified by its push_token (for push monitors).
	// Returns ErrNotFound if no matching active monitor.
	GetByPushToken(ctx context.Context, pushToken string) (*domain.Monitor, error)
	List(ctx context.Context, filter MonitorFilter) ([]*domain.Monitor, error)
	ListActive(ctx context.Context) ([]*domain.Monitor, error)
	Update(ctx context.Context, m *domain.Monitor) error
	Delete(ctx context.Context, id int64) error
	// ClaimBatch atomically claims up to batchSize active monitors for a worker.
	// Monitors are claimed if they are unclaimed (worker_id IS NULL) or the lease
	// has expired (leased_at < now - leaseTTL). Returns the claimed monitors.
	ClaimBatch(ctx context.Context, workerID string, batchSize int, leaseTTL time.Duration) ([]*domain.Monitor, error)
	// RefreshLease extends the lease for all monitors claimed by workerID.
	RefreshLease(ctx context.Context, workerID string) (int64, error)
	// ReleaseLeases releases all monitors claimed by workerID (sets worker_id=NULL).
	ReleaseLeases(ctx context.Context, workerID string) (int64, error)
}

// MonitorGroupRepository defines persistence operations for monitor groups.
type MonitorGroupRepository interface {
	Create(ctx context.Context, g *domain.MonitorGroup) error
	GetByID(ctx context.Context, id int64) (*domain.MonitorGroup, error)
	// List returns every group owned by userID, ordered by weight then name.
	// The caller assembles the tree from each group's ParentID.
	List(ctx context.Context, userID int64) ([]*domain.MonitorGroup, error)
	// ListAll returns every group in the install, ordered the same way as List.
	//
	// RBAC needs this: a non-admin's group grant points at a group OWNED BY THE
	// ADMIN, so the owner-scoped List(userID) would return nothing for them and
	// the grant could never be expanded down the tree. Only the AccessService and
	// admin-scoped callers should use it.
	ListAll(ctx context.Context) ([]*domain.MonitorGroup, error)
	// Update persists every mutable field EXCEPT last_status, which is owned by
	// ClaimStatusTransition. A group loaded by the admin API and written back
	// must not be able to clobber an alerting decision made by a worker in the
	// meantime.
	Update(ctx context.Context, g *domain.MonitorGroup) error
	// Delete removes the group only. Monitors filed under it, and any subgroups,
	// are re-homed to the deleted group's own parent (nil = top level) — deleting
	// a folder must never delete the monitors inside it.
	Delete(ctx context.Context, id int64) error
	// ClaimStatusTransition atomically moves the group's persisted last_status
	// from `from` to `to`, and reports whether THIS caller won the transition.
	//
	// It is a compare-and-set: the UPDATE only matches when the stored value is
	// still `from` (nil meaning SQL NULL — the group has never been evaluated).
	// won == true means RowsAffected == 1, i.e. nobody else moved the group first
	// and the caller may now send the group's alert. won == false means another
	// worker already claimed this exact transition and the caller MUST NOT alert.
	//
	// This is the whole reason last_status is persisted rather than kept in a map:
	// two workers can process heartbeats for two monitors in the same group in the
	// same instant, both recompute the rollup as DOWN, and both would otherwise
	// send. Implementations MUST NOT be called with to == *from (a no-op UPDATE
	// reports 0 rows affected on MariaDB and would read as "lost the race").
	ClaimStatusTransition(ctx context.Context, groupID int64, from *domain.Status, to domain.Status) (bool, error)
}

// HeartbeatRepository defines persistence operations for heartbeats.
type HeartbeatRepository interface {
	Save(ctx context.Context, h *domain.Heartbeat) error
	GetLatest(ctx context.Context, monitorID int64) (*domain.Heartbeat, error)
	ListByMonitor(ctx context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error)
	// DeleteByMonitor removes all heartbeats for a monitor (clear history).
	DeleteByMonitor(ctx context.Context, monitorID int64) error
	// DeleteOlderThan removes heartbeats with time strictly before before.
	// Implementations MUST force before.UTC() at the DB boundary (rule 6): a
	// local-zoned cutoff would delete rows up to the host offset newer than
	// intended. Used by the retention ticker.
	DeleteOlderThan(ctx context.Context, before time.Time) error
	SaveAggregate1m(ctx context.Context, agg *Aggregate1m) error
	SaveAggregate1h(ctx context.Context, agg *Aggregate1h) error
	SaveAggregate1d(ctx context.Context, agg *Aggregate1d) error
	GetAggregate1m(ctx context.Context, monitorID int64, from time.Time) ([]*Aggregate1m, error)
	GetAggregate1h(ctx context.Context, monitorID int64, from time.Time) ([]*Aggregate1h, error)
	GetAggregate1d(ctx context.Context, monitorID int64, from time.Time) ([]*Aggregate1d, error)
}

// HeartbeatBatchReader resolves the latest heartbeat for MANY monitors in one
// round trip. It exists to kill the N+1 that made WebSocket fan-out quadratic:
// the hub used to call GetLatest once per active monitor, once per status
// change, serialized in a single goroutine (Sprint D / R3.6).
//
// It is deliberately a SEPARATE interface rather than another method on
// HeartbeatRepository. Both real adapters implement it and production always
// gets the batched path, but widening HeartbeatRepository itself would break
// every hand-written test fake that implements it — including
// ws/hub_rbac_test.go, which Sprint D requires to stay green UNMODIFIED. A
// consumer therefore type-asserts for this interface and falls back to
// per-monitor GetLatest when it is absent (correct, just slower).
//
// Implementations MUST reproduce GetLatest's `time DESC, id DESC` tie-break.
// On MariaDB heartbeats.time has only SECOND precision, so a retry PENDING and
// the DOWN that confirms it share a timestamp; ordering by time alone lets the
// engine return the older row and a DOWN monitor reads back as PENDING. See the
// GetLatest doc comments in both adapters.
type HeartbeatBatchReader interface {
	// GetLatestForMonitors returns the newest heartbeat per monitor id.
	//
	// Monitors with no heartbeat at all are simply ABSENT from the map — that is
	// not an error. The returned map is never nil for a nil error.
	GetLatestForMonitors(ctx context.Context, monitorIDs []int64) (map[int64]*domain.Heartbeat, error)
}

// NotificationRepository defines persistence operations for notifications.
type NotificationRepository interface {
	Create(ctx context.Context, n *domain.Notification) error
	GetByID(ctx context.Context, id int64) (*domain.Notification, error)
	GetByMonitorID(ctx context.Context, monitorID int64) ([]*domain.Notification, error)
	List(ctx context.Context, userID int64) ([]*domain.Notification, error)
	// ListAll returns every notification in the install.
	//
	// Admins and users holding the can_manage_notifications capability see all
	// notifications install-wide: a capability holder who could not edit the
	// notifications the admin created would hold a useless grant.
	ListAll(ctx context.Context) ([]*domain.Notification, error)
	Update(ctx context.Context, n *domain.Notification) error
	Delete(ctx context.Context, id int64) error
}

// StatusPageRepository defines persistence operations for status pages.
type StatusPageRepository interface {
	Create(ctx context.Context, sp *domain.StatusPage) error
	GetByID(ctx context.Context, id int64) (*domain.StatusPage, error)
	GetBySlug(ctx context.Context, slug string) (*domain.StatusPage, error)
	List(ctx context.Context) ([]*domain.StatusPage, error)
	Update(ctx context.Context, sp *domain.StatusPage) error
	Delete(ctx context.Context, id int64) error
}

// TagRepository defines persistence operations for tags.
type TagRepository interface {
	Create(ctx context.Context, t *domain.Tag) error
	GetByID(ctx context.Context, id int64) (*domain.Tag, error)
	List(ctx context.Context) ([]*domain.Tag, error)
	Update(ctx context.Context, t *domain.Tag) error
	Delete(ctx context.Context, id int64) error
}

// MaintenanceRepository defines persistence operations for maintenance windows.
type MaintenanceRepository interface {
	Create(ctx context.Context, mw *domain.MaintenanceWindow) error
	GetByID(ctx context.Context, id int64) (*domain.MaintenanceWindow, error)
	List(ctx context.Context, userID int64) ([]*domain.MaintenanceWindow, error)
	// ListAll returns every maintenance window in the install. Same rationale as
	// NotificationRepository.ListAll: admins and can_manage_maintenance holders
	// manage the install's windows, not just the ones they happen to own.
	ListAll(ctx context.Context) ([]*domain.MaintenanceWindow, error)
	Update(ctx context.Context, mw *domain.MaintenanceWindow) error
	Delete(ctx context.Context, id int64) error
}

// ProxyRepository defines persistence operations for outbound proxies that
// monitor checks can be routed through.
type ProxyRepository interface {
	Create(ctx context.Context, p *domain.Proxy) error
	GetByID(ctx context.Context, id int64) (*domain.Proxy, error)
	// List returns every proxy owned by userID.
	List(ctx context.Context, userID int64) ([]*domain.Proxy, error)
	Update(ctx context.Context, p *domain.Proxy) error
	Delete(ctx context.Context, id int64) error
}

// APIKeyRepository defines persistence operations for API keys.
type APIKeyRepository interface {
	Create(ctx context.Context, ak *domain.APIKey) error
	GetByHash(ctx context.Context, hash string) (*domain.APIKey, error)
	List(ctx context.Context, userID int64) ([]*domain.APIKey, error)
	Update(ctx context.Context, ak *domain.APIKey) error
	Delete(ctx context.Context, id int64) error
}

// WebAuthnCredentialRepository defines persistence operations for WebAuthn
// (passkey) credentials.
type WebAuthnCredentialRepository interface {
	Create(ctx context.Context, c *domain.WebAuthnCredential) error
	// ListByUser returns all credentials owned by a user, oldest first.
	ListByUser(ctx context.Context, userID int64) ([]*domain.WebAuthnCredential, error)
	// GetByCredentialID looks a credential up by its raw credential ID bytes.
	GetByCredentialID(ctx context.Context, credentialID []byte) (*domain.WebAuthnCredential, error)
	// UpdateUsage persists the mutable authenticator state after a successful
	// assertion and bumps last_used_at.
	UpdateUsage(ctx context.Context, id int64, signCount uint32, flags byte, cloneWarning bool, attachment string, lastUsedAt time.Time) error
	// Delete removes a credential by primary key, scoped to a user so a caller
	// can only delete its own credentials.
	Delete(ctx context.Context, id, userID int64) error
}

// UserRepository defines persistence operations for users.
type UserRepository interface {
	Create(ctx context.Context, u *domain.User) error
	GetByID(ctx context.Context, id int64) (*domain.User, error)
	GetByUsername(ctx context.Context, username string) (*domain.User, error)
	Update(ctx context.Context, u *domain.User) error
	Delete(ctx context.Context, id int64) error
	// Count returns the total number of users in the system.
	Count(ctx context.Context) (int64, error)
	// List returns every user ordered by id ascending.
	List(ctx context.Context) ([]*domain.User, error)
}

// MonitorNotificationRepository defines persistence for monitor-notification links.
type MonitorNotificationRepository interface {
	Attach(ctx context.Context, monitorID, notificationID int64) error
	Detach(ctx context.Context, monitorID, notificationID int64) error
	ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.MonitorNotification, error)
	ListByNotification(ctx context.Context, notificationID int64) ([]*domain.MonitorNotification, error)
}

// GroupNotificationRepository defines persistence for monitor-group ↔ notification
// links — the alerting attachment of a FOLDER, which fires on the folder's own
// derived status and never inherits down to the monitors inside it.
//
// The link table carries UNIQUE(group_id, notification_id) and cascades on the
// delete of either side, so a deleted group or notification can never leave a
// dangling attachment that a later row reusing the id would silently inherit.
type GroupNotificationRepository interface {
	// Attach links a notification to a group. It is IDEMPOTENT: re-attaching an
	// existing pair is a no-op and returns nil, not ErrConflict — the UI toggles
	// this checkbox and a double-click must not 500. Both SQL adapters implement
	// this with an engine-specific insert-or-ignore; they are observably identical.
	Attach(ctx context.Context, groupID, notificationID int64) error
	// Detach removes the link. Detaching a pair that is not linked is not an error.
	Detach(ctx context.Context, groupID, notificationID int64) error
	// ListByGroup returns the link rows for one group.
	ListByGroup(ctx context.Context, groupID int64) ([]*domain.GroupNotification, error)
	// ListByNotification returns the link rows for one notification, across groups.
	// Used by backup export, which walks the user's notifications.
	ListByNotification(ctx context.Context, notificationID int64) ([]*domain.GroupNotification, error)
	// ListNotificationsByGroup returns the full notifications attached to a group,
	// ordered by id. This is the read the alert dispatch path and
	// GET /api/monitor-groups/:id/notifications both need; resolving the join in
	// SQL costs one query instead of one per link.
	ListNotificationsByGroup(ctx context.Context, groupID int64) ([]*domain.Notification, error)
}

// IncidentRepository defines persistence operations for incidents.
type IncidentRepository interface {
	Create(ctx context.Context, inc *domain.Incident) error
	GetByID(ctx context.Context, id int64) (*domain.Incident, error)
	ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.Incident, error)
	// ListAll returns every incident ordered by created_at descending.
	ListAll(ctx context.Context) ([]*domain.Incident, error)
	Update(ctx context.Context, inc *domain.Incident) error
	Delete(ctx context.Context, id int64) error
}

// IncidentUpdateRepository defines persistence operations for incident timeline updates.
type IncidentUpdateRepository interface {
	Create(ctx context.Context, update *domain.IncidentUpdate) error
	// ListByIncident returns timeline updates ordered by created_at ASC, id ASC.
	ListByIncident(ctx context.Context, incidentID int64) ([]*domain.IncidentUpdate, error)
	// ListByStatusPage returns timeline updates for every incident on a page,
	// ordered by incident_id ASC, created_at ASC, id ASC.
	ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.IncidentUpdate, error)
}

// StatusPageCNAMERepository defines persistence for status page custom domains.
type StatusPageCNAMERepository interface {
	Create(ctx context.Context, cname *domain.StatusPageCNAME) error
	Delete(ctx context.Context, id int64) error
	ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.StatusPageCNAME, error)
	GetByDomain(ctx context.Context, domain string) (*domain.StatusPageCNAME, error)
}

// StatusPageMonitorRepository defines persistence for status page-monitor links.
type StatusPageMonitorRepository interface {
	AddMonitor(ctx context.Context, spID, monitorID int64, displayOrder int) error
	RemoveMonitor(ctx context.Context, spID, monitorID int64) error
	ReorderMonitors(ctx context.Context, spID int64, monitorIDs []int64) error
	ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.StatusPageMonitor, error)
}

// StatusPageSubscriberRepository defines persistence for double-opt-in email
// subscribers and the per-page SMTP subscription channel.
type StatusPageSubscriberRepository interface {
	// Create inserts a pending or active subscriber. On success it assigns
	// ID, CreatedAt, and UpdatedAt.
	Create(ctx context.Context, sub *domain.StatusPageSubscriber) error
	// GetByID returns one subscriber by primary key.
	GetByID(ctx context.Context, id int64) (*domain.StatusPageSubscriber, error)
	// GetByPageAndEmail returns the subscriber for a page+email pair
	// (email must already be normalized lowercase).
	GetByPageAndEmail(ctx context.Context, statusPageID int64, email string) (*domain.StatusPageSubscriber, error)
	// Update persists mutable subscriber fields (Active, ConfirmedAt, Email, UpdatedAt).
	Update(ctx context.Context, sub *domain.StatusPageSubscriber) error
	// Delete removes a subscriber by ID. Deleting a missing row is not an error.
	Delete(ctx context.Context, id int64) error
	// ListByStatusPage returns every subscriber for a page (admin view).
	ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.StatusPageSubscriber, error)
	// ListConfirmedByStatusPage returns Active=true subscribers for fan-out.
	ListConfirmedByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.StatusPageSubscriber, error)

	// GetChannel returns the SMTP notification channel for a status page.
	GetChannel(ctx context.Context, statusPageID int64) (*domain.StatusPageSubscriptionChannel, error)
	// SetChannel upserts the single channel binding for a status page.
	SetChannel(ctx context.Context, channel *domain.StatusPageSubscriptionChannel) error
	// DeleteChannel removes the channel binding for a status page.
	DeleteChannel(ctx context.Context, statusPageID int64) error
	// ListStatusPageIDsForMonitors returns distinct status-page IDs that
	// include any of the given monitors (for maintenance fan-out).
	ListStatusPageIDsForMonitors(ctx context.Context, monitorIDs []int64) ([]int64, error)
}

// SettingRepository defines persistence for key-value app settings.
type SettingRepository interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
}

// MonitorTagRepository defines persistence for monitor-tag assignments (key-value tags on monitors).
type MonitorTagRepository interface {
	Assign(ctx context.Context, monitorID, tagID int64, value string) error
	Remove(ctx context.Context, monitorID, tagID int64) error
	ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.MonitorTag, error)
	// ListByMonitors returns the tag assignments for many monitors in ONE query,
	// keyed by monitor id. It exists so GET /api/monitors — which now embeds each
	// monitor's tags — costs 1 query instead of N.
	//
	// Monitors with no tags are simply absent from the map; callers must treat a
	// missing key as "no tags" and still emit an empty JSON array. An empty or
	// nil monitorIDs returns an empty map and issues no query.
	ListByMonitors(ctx context.Context, monitorIDs []int64) (map[int64][]*domain.MonitorTag, error)
}

// UserPermissionRepository defines persistence for per-user RBAC view grants.
//
// A grant names exactly one resource — a monitor or a group (never both, see
// domain.UserPermission). Grants are additive and view-only.
type UserPermissionRepository interface {
	// Grant persists a view grant. It is idempotent: re-granting an existing
	// (user, resource) pair is a no-op and returns nil, not ErrConflict.
	Grant(ctx context.Context, p *domain.UserPermission) error
	// RevokeMonitor removes a user's direct grant on one monitor. Revoking a
	// grant that does not exist is not an error.
	RevokeMonitor(ctx context.Context, userID, monitorID int64) error
	// RevokeGroup removes a user's grant on one group.
	RevokeGroup(ctx context.Context, userID, groupID int64) error
	// RevokeAll removes every grant held by one user. Used by the admin API to
	// replace a user's whole permission set in one shot.
	RevokeAll(ctx context.Context, userID int64) error
	// ListByUser returns every grant held by one user.
	ListByUser(ctx context.Context, userID int64) ([]*domain.UserPermission, error)
	// ListByMonitor returns every grant pointing at one monitor (across users).
	ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.UserPermission, error)
	// ListByGroup returns every grant pointing at one group (across users).
	ListByGroup(ctx context.Context, groupID int64) ([]*domain.UserPermission, error)
}

// ConfigKeyRepository defines persistence for operator-stable config keys
// used by declarative config-as-code (F5 Sprint 14).
type ConfigKeyRepository interface {
	// Upsert inserts or updates the mapping for (resource_type, key_name).
	// When the key already points at another resource_id it is reassigned.
	// When resource_id already has a different key for the same type, that
	// old key row is replaced so UNIQUE(type, resource_id) holds.
	Upsert(ctx context.Context, k *domain.ConfigKey) error
	// GetByKey looks up a mapping by type + key name.
	GetByKey(ctx context.Context, resourceType, keyName string) (*domain.ConfigKey, error)
	// GetByResource looks up a mapping by type + resource id.
	GetByResource(ctx context.Context, resourceType string, resourceID int64) (*domain.ConfigKey, error)
	// ListByType returns every key for one resource type, ordered by key_name.
	ListByType(ctx context.Context, resourceType string) ([]*domain.ConfigKey, error)
	// DeleteByKey removes one mapping. Missing is not an error.
	DeleteByKey(ctx context.Context, resourceType, keyName string) error
	// DeleteByResource removes the mapping for a resource id. Missing is not an error.
	DeleteByResource(ctx context.Context, resourceType string, resourceID int64) error
}

// OIDCIdentityRepository defines persistence for OIDC (issuer, subject) → user links.
//
// The linking key is always (issuer, subject). Email is never a primary key —
// see docs/F5-S13-OIDC-CONTRACTS.md.
type OIDCIdentityRepository interface {
	// Create inserts a new identity link. Returns ErrConflict when the
	// (issuer, subject) pair or (user_id, issuer) pair already exists.
	Create(ctx context.Context, id *domain.OIDCIdentity) error
	// GetByIssuerSubject looks up a link by the immutable OIDC pair.
	GetByIssuerSubject(ctx context.Context, issuer, subject string) (*domain.OIDCIdentity, error)
	// ListByUser returns every OIDC identity linked to a Phoenix user.
	ListByUser(ctx context.Context, userID int64) ([]*domain.OIDCIdentity, error)
	// TouchLogin updates email (last known) and last_login_at after a successful login.
	TouchLogin(ctx context.Context, id int64, email string, lastLoginAt time.Time) error
	// Delete removes one identity row by primary key.
	Delete(ctx context.Context, id int64) error
}

// MaintenanceWindowMonitorRepository defines persistence for maintenance window-monitor links.
type MaintenanceWindowMonitorRepository interface {
	Assign(ctx context.Context, maintenanceID, monitorID int64) error
	Remove(ctx context.Context, maintenanceID, monitorID int64) error
	ListByMaintenance(ctx context.Context, maintenanceID int64) ([]int64, error)
	ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.MaintenanceWindow, error)
}

// AlertFilter defines criteria for listing monitor alerts (F2.2).
//
// RestrictToMonitorIDs mirrors MonitorFilter: a non-admin with zero grants must
// see zero alerts, never "no filter". Implementations branch on the flag, not
// on len(MonitorIDs).
type AlertFilter struct {
	// Statuses restricts to these lifecycle states. Empty means all.
	Statuses []string
	// MonitorID, when non-nil, restricts to one monitor.
	MonitorID *int64
	// OpenOnly is a convenience for status ∈ {firing, acked}.
	OpenOnly bool

	RestrictToMonitorIDs bool
	MonitorIDs           []int64

	Limit  int
	Offset int
}

// AlertRepository defines persistence for monitor alert lifecycle records (F2.2).
type AlertRepository interface {
	Create(ctx context.Context, a *domain.Alert) error
	Update(ctx context.Context, a *domain.Alert) error
	GetByID(ctx context.Context, id int64) (*domain.Alert, error)
	// GetByAckToken looks up an alert by its deep-link acknowledgement token.
	GetByAckToken(ctx context.Context, token string) (*domain.Alert, error)
	// GetOpenByMonitorID returns the firing or acked alert for a monitor, or
	// ErrNotFound when none is open.
	GetOpenByMonitorID(ctx context.Context, monitorID int64) (*domain.Alert, error)
	List(ctx context.Context, filter AlertFilter) ([]*domain.Alert, error)
}

// TLSInfo holds cached TLS certificate information for a monitor.
//
// Certificate-alert state lives here (not a separate table) so a worker restart
// can resume "already sent for threshold N on this cert" without re-alerting.
// LastCertAlertThreshold is 0 when no threshold has been sent for the current
// certificate (LastCertAlertNotAfter). A renewed certificate (different
// NotAfter) resets both fields on the next successful evaluate+persist cycle.
type TLSInfo struct {
	MonitorID     int64
	DaysRemaining int
	NotAfter      time.Time
	Issuer        string
	CheckedAt     time.Time

	// LastCertAlertThreshold is the most urgent threshold already dispatched for
	// LastCertAlertNotAfter (30, 14, or 7). Zero means none.
	LastCertAlertThreshold int
	// LastCertAlertNotAfter is the certificate NotAfter that LastCertAlertThreshold
	// was recorded against. Empty when no alert has been marked sent.
	LastCertAlertNotAfter time.Time
}

// TLSInfoRepository defines persistence operations for cached TLS certificate info.
type TLSInfoRepository interface {
	Upsert(ctx context.Context, info *TLSInfo) error
	GetByMonitorID(ctx context.Context, monitorID int64) (*TLSInfo, error)
}
