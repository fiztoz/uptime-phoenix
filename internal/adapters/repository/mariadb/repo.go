// Package mariadb provides the MariaDB adapter for repository ports.
// Uses Bun as the query builder.
package mariadb

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// translateError converts low-level SQL errors to domain/ports errors.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrNotFound
	}
	msg := err.Error()
	if strings.Contains(msg, "1062") || strings.Contains(msg, "Duplicate entry") {
		return ports.ErrConflict
	}
	return err
}

// ---------------------------------------------------------------------------
// UserRepo implements ports.UserRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// UserRepo implements ports.UserRepository.
type UserRepo struct{ db *bun.DB }

// NewUserRepo creates a MariaDB-backed user repository.
func NewUserRepo(db *bun.DB) *UserRepo { return &UserRepo{db: db} }

func (r *UserRepo) Create(ctx context.Context, u *domain.User) error {
	m := repository.UserModelFromDomain(u)
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	u.ID = m.ID
	u.CreatedAt = m.CreatedAt
	u.UpdatedAt = m.UpdatedAt
	return nil
}

func (r *UserRepo) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	m := new(repository.UserModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *UserRepo) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	m := new(repository.UserModel)
	if err := r.db.NewSelect().Model(m).Where("username = ?", username).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *UserRepo) Update(ctx context.Context, u *domain.User) error {
	m := repository.UserModelFromDomain(u)
	m.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *UserRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.UserModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

func (r *UserRepo) Count(ctx context.Context) (int64, error) {
	count, err := r.db.NewSelect().Model((*repository.UserModel)(nil)).Count(ctx)
	return int64(count), err
}

// List returns every user ordered by id ascending.
func (r *UserRepo) List(ctx context.Context) ([]*domain.User, error) {
	var models []*repository.UserModel
	if err := r.db.NewSelect().Model(&models).Order("id ASC").Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.User, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// MonitorRepo implements ports.MonitorRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// MonitorRepo implements ports.MonitorRepository.
type MonitorRepo struct{ db *bun.DB }

// NewMonitorRepo creates a MariaDB-backed monitor repository.
func NewMonitorRepo(db *bun.DB) *MonitorRepo { return &MonitorRepo{db: db} }

func (r *MonitorRepo) Create(ctx context.Context, m *domain.Monitor) error {
	model := repository.MonitorModelFromDomain(m)
	now := time.Now().UTC()
	model.CreatedAt = now
	model.UpdatedAt = now
	if model.Config == nil {
		model.Config = repository.JSONField{}
	}
	if len(model.AcceptedStatusCodes) == 0 {
		model.AcceptedStatusCodes = repository.StringListField{"200-299"}
	}
	_, err := r.db.NewInsert().Model(model).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	m.ID = model.ID
	m.CreatedAt = model.CreatedAt
	m.UpdatedAt = model.UpdatedAt
	return nil
}

func (r *MonitorRepo) GetByID(ctx context.Context, id int64) (*domain.Monitor, error) {
	m := new(repository.MonitorModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// GetByPushToken implements ports.MonitorRepository.
func (r *MonitorRepo) GetByPushToken(ctx context.Context, pushToken string) (*domain.Monitor, error) {
	m := new(repository.MonitorModel)
	if err := r.db.NewSelect().Model(m).Where("push_token = ?", pushToken).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *MonitorRepo) List(ctx context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	var models []*repository.MonitorModel
	q := r.db.NewSelect().Model(&models)
	if filter.UserID > 0 {
		q = q.Where("user_id = ?", filter.UserID)
	}
	if filter.Active != nil {
		q = q.Where("active = ?", *filter.Active)
	}
	if filter.Type != "" {
		q = q.Where("type = ?", filter.Type)
	}
	if filter.Search != "" {
		q = q.Where("name LIKE ?", "%"+filter.Search+"%")
	}
	if filter.GroupIDIsNull {
		q = q.Where("group_id IS NULL")
	} else if filter.GroupID != nil {
		q = q.Where("group_id = ?", *filter.GroupID)
	}
	// RBAC allowlist. Branch on the flag, NEVER on len(filter.MonitorIDs): an
	// empty allowlist means "this user may see nothing", and treating it as
	// "no filter" would hand them the whole install. Bun's WhereGroup/In with an
	// empty slice is not a safe way to express that, so the zero-rows case is
	// forced explicitly with a false predicate.
	if filter.RestrictToIDs {
		if len(filter.MonitorIDs) == 0 {
			q = q.Where("1 = 0")
		} else {
			q = q.Where("id IN (?)", bun.List(filter.MonitorIDs))
		}
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	// Display order: weight (manual sort), then name, then id as a stable tie-break.
	q = q.Order("weight ASC", "name ASC", "id ASC")
	if err := q.Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Monitor, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *MonitorRepo) ListActive(ctx context.Context) ([]*domain.Monitor, error) {
	var models []*repository.MonitorModel
	if err := r.db.NewSelect().Model(&models).Where("active = TRUE").Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Monitor, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *MonitorRepo) Update(ctx context.Context, m *domain.Monitor) error {
	model := repository.MonitorModelFromDomain(m)
	model.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(model).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *MonitorRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.MonitorModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// ClaimBatch atomically claims up to batchSize active monitors for a worker.
// Uses UPDATE ... WHERE to set worker_id and leased_at for unclaimed or expired leases.
func (r *MonitorRepo) ClaimBatch(ctx context.Context, workerID string, batchSize int, leaseTTL time.Duration) ([]*domain.Monitor, error) {
	now := time.Now().UTC()
	leaseExpiry := now.Add(-leaseTTL)

	// Step 1: Claim monitors by updating their lease columns.
	// This is atomic — only one worker can claim each monitor.
	_, err := r.db.NewRaw(
		"UPDATE monitors SET worker_id = ?, leased_at = ? WHERE active = TRUE AND (worker_id IS NULL OR leased_at < ? OR worker_id = ?) ORDER BY id LIMIT ?",
		workerID, now, leaseExpiry, workerID, batchSize,
	).Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim monitors: %w", err)
	}

	// Step 2: Select the monitors we just claimed.
	var models []*repository.MonitorModel
	if err := r.db.NewSelect().Model(&models).
		Where("worker_id = ? AND leased_at = ?", workerID, now).
		Order("id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("select claimed monitors: %w", err)
	}

	out := make([]*domain.Monitor, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// RefreshLease extends the lease for all monitors claimed by workerID.
func (r *MonitorRepo) RefreshLease(ctx context.Context, workerID string) (int64, error) {
	now := time.Now().UTC()
	res, err := r.db.NewRaw(
		"UPDATE monitors SET leased_at = ? WHERE worker_id = ?",
		now, workerID,
	).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("refresh lease: %w", err)
	}
	return res.RowsAffected()
}

// ReleaseLeases releases all monitors claimed by workerID.
func (r *MonitorRepo) ReleaseLeases(ctx context.Context, workerID string) (int64, error) {
	res, err := r.db.NewRaw(
		"UPDATE monitors SET worker_id = NULL, leased_at = NULL WHERE worker_id = ?",
		workerID,
	).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("release leases: %w", err)
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// MonitorGroupRepo implements ports.MonitorGroupRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// MonitorGroupRepo implements ports.MonitorGroupRepository.
type MonitorGroupRepo struct{ db *bun.DB }

// NewMonitorGroupRepo creates a MariaDB-backed monitor group repository.
func NewMonitorGroupRepo(db *bun.DB) *MonitorGroupRepo { return &MonitorGroupRepo{db: db} }

func (r *MonitorGroupRepo) Create(ctx context.Context, g *domain.MonitorGroup) error {
	m := repository.MonitorGroupModelFromDomain(g)
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	g.ID = m.ID
	g.CreatedAt = m.CreatedAt
	g.UpdatedAt = m.UpdatedAt
	return nil
}

func (r *MonitorGroupRepo) GetByID(ctx context.Context, id int64) (*domain.MonitorGroup, error) {
	m := new(repository.MonitorGroupModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// List returns every group owned by userID, ordered by weight then name.
func (r *MonitorGroupRepo) List(ctx context.Context, userID int64) ([]*domain.MonitorGroup, error) {
	var models []*repository.MonitorGroupModel
	err := r.db.NewSelect().Model(&models).
		Where("user_id = ?", userID).
		Order("weight ASC", "name ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.MonitorGroup, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ListAll returns every group in the install, ordered like List. RBAC needs the
// whole tree: a non-admin's grant points at a group owned by the admin, so an
// owner-scoped list would never find it.
func (r *MonitorGroupRepo) ListAll(ctx context.Context) ([]*domain.MonitorGroup, error) {
	var models []*repository.MonitorGroupModel
	err := r.db.NewSelect().Model(&models).
		Order("weight ASC", "name ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.MonitorGroup, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// Update persists every mutable column EXCEPT last_status.
//
// last_status is owned by ClaimStatusTransition. If a normal Update wrote it too,
// an admin PUT that loaded the group a second earlier would write back a stale
// value and silently undo a transition a worker had just claimed — the group
// would then re-alert on the next heartbeat, or (worse) never alert again.
func (r *MonitorGroupRepo) Update(ctx context.Context, g *domain.MonitorGroup) error {
	m := repository.MonitorGroupModelFromDomain(g)
	m.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(m).WherePK().ExcludeColumn("last_status").Exec(ctx)
	return translateError(err)
}

// ClaimStatusTransition compare-and-sets the group's last_status.
//
// `last_status <=> ?` is MariaDB's NULL-safe equality, so a nil `from` matches a
// row that has never been evaluated. The SQLite adapter uses `IS` for exactly the
// same semantics — the two must stay observably identical.
//
// `updated_at = updated_at` is not redundant. monitor_groups.updated_at carries
// ON UPDATE CURRENT_TIMESTAMP in MariaDB (and nothing of the sort in SQLite), so
// without the explicit self-assignment — which suppresses the automatic update —
// claiming a transition would bump the group's updated_at on MariaDB only, and
// the two engines would stop being observably identical.
//
// Returns true iff this call moved the row (RowsAffected == 1), which is the
// caller's license to send the group's alert. False means another worker got
// there first and this one must stay quiet. This is sound because the caller
// never issues a CAS with to == *from: go-sql-driver reports CHANGED rows, not
// matched rows, so a no-op UPDATE would come back as 0 and read as a lost race.
func (r *MonitorGroupRepo) ClaimStatusTransition(ctx context.Context, groupID int64, from *domain.Status, to domain.Status) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		"UPDATE monitor_groups SET last_status = ?, updated_at = updated_at WHERE id = ? AND last_status <=> ?",
		int(to), groupID, statusParam(from),
	)
	if err != nil {
		return false, translateError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim status transition: rows affected: %w", err)
	}
	return n == 1, nil
}

// statusParam renders a nullable status as a SQL bind value: NULL when unset.
// Binding the *domain.Status directly would hand the driver a pointer to a named
// int type, which database/sql refuses.
func statusParam(s *domain.Status) any {
	if s == nil {
		return nil
	}
	return int(*s)
}

// Delete removes the group only: child monitors and child subgroups are
// re-homed to the deleted group's own parent (nil = top level) inside a
// single transaction — deleting a folder must never delete the monitors
// filed under it, nor orphan its subgroups.
func (r *MonitorGroupRepo) Delete(ctx context.Context, id int64) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		g := new(repository.MonitorGroupModel)
		if err := tx.NewSelect().Model(g).Where("id = ?", id).Scan(ctx); err != nil {
			return translateError(err)
		}

		if _, err := tx.NewUpdate().Model((*repository.MonitorModel)(nil)).
			Set("group_id = ?", g.ParentID).
			Where("group_id = ?", id).
			Exec(ctx); err != nil {
			return translateError(err)
		}

		if _, err := tx.NewUpdate().Model((*repository.MonitorGroupModel)(nil)).
			Set("parent_id = ?", g.ParentID).
			Where("parent_id = ?", id).
			Exec(ctx); err != nil {
			return translateError(err)
		}

		res, err := tx.NewDelete().Model((*repository.MonitorGroupModel)(nil)).Where("id = ?", id).Exec(ctx)
		if err != nil {
			return translateError(err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ports.ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// HeartbeatRepo implements ports.HeartbeatRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// HeartbeatRepo implements ports.HeartbeatRepository.
type HeartbeatRepo struct{ db *bun.DB }

// See the matching assertion in the SQLite adapter: the hub falls back to the
// N+1 path when HeartbeatBatchReader is absent, so losing this method would be a
// silent performance regression rather than a build error without this.
var (
	_ ports.HeartbeatRepository  = (*HeartbeatRepo)(nil)
	_ ports.HeartbeatBatchReader = (*HeartbeatRepo)(nil)
)

// NewHeartbeatRepo creates a MariaDB-backed heartbeat repository.
func NewHeartbeatRepo(db *bun.DB) *HeartbeatRepo { return &HeartbeatRepo{db: db} }

func (r *HeartbeatRepo) Save(ctx context.Context, h *domain.Heartbeat) error {
	m := repository.HeartbeatModelFromDomain(h)
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	h.ID = m.ID
	return nil
}

// GetLatest returns the monitor's most recent heartbeat.
//
// The `id DESC` tie-break is LOAD-BEARING on MariaDB, and its absence was a real
// bug. heartbeats.time is a TIMESTAMP with SECOND precision here (001_init), so a
// retry PENDING and the confirmed DOWN that follows it milliseconds later carry
// the SAME time value. Ordering by time alone leaves that tie to the engine, and
// MariaDB happily returns the older PENDING row — so a monitor that had just gone
// DOWN read back as PENDING, and every consumer of "latest status" (the group
// rollup, folder alerting, the dashboard) silently saw the wrong thing.
//
// SQLite stores timestamps as higher-precision text, so it never produces the tie
// and the repo's SQLite-only tests cannot catch this. The tie-break is applied to
// BOTH engines so they stay observably identical. id is monotonic per insert, so
// within one second the newest row wins.
func (r *HeartbeatRepo) GetLatest(ctx context.Context, monitorID int64) (*domain.Heartbeat, error) {
	m := new(repository.HeartbeatModel)
	err := r.db.NewSelect().Model(m).
		Where("monitor_id = ?", monitorID).
		OrderExpr("time DESC, id DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// GetLatestForMonitors returns the newest heartbeat per monitor in one query
// per 500 ids, instead of one query per monitor.
//
// The ordering is delegated to repository.LatestHeartbeatsForMonitors, which
// reproduces GetLatest's `time DESC, id DESC` exactly. That tie-break is not
// cosmetic here: heartbeats.time is second-precision on MariaDB, so dropping it
// would resurrect the bug where a monitor that had just gone DOWN read back as
// PENDING — only now for every monitor on the dashboard at once.
func (r *HeartbeatRepo) GetLatestForMonitors(ctx context.Context, monitorIDs []int64) (map[int64]*domain.Heartbeat, error) {
	out, err := repository.LatestHeartbeatsForMonitors(ctx, r.db, monitorIDs)
	if err != nil {
		return nil, translateError(err)
	}
	return out, nil
}

// ListByMonitor returns heartbeats in [from, to].
//
// The bounds are forced to UTC at the boundary. The MySQL driver does convert to
// the DSN's loc=UTC, so this is belt-and-braces here — but the SQLite repo shares
// these callers and does NOT convert, and a query that behaves differently on the
// two engines is worse than one that is simply wrong. Keep both identical. See
// AGENTS.md rule 6.
func (r *HeartbeatRepo) ListByMonitor(ctx context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
	var models []*repository.HeartbeatModel
	err := r.db.NewSelect().Model(&models).
		Where("monitor_id = ?", monitorID).
		Where("time >= ?", from.UTC()).
		Where("time <= ?", to.UTC()).
		// `id ASC` for the same reason GetLatest needs `id DESC`: heartbeats.time is
		// second-precision here, so beats written within one second tie and the engine
		// is free to return them in any order — which renders the recent-checks strip
		// and the chart out of sequence. id is monotonic per insert; it is the real
		// chronological order.
		OrderExpr("time ASC, id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Heartbeat, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *HeartbeatRepo) DeleteByMonitor(ctx context.Context, monitorID int64) error {
	_, err := r.db.NewDelete().Model((*repository.HeartbeatModel)(nil)).
		Where("monitor_id = ?", monitorID).
		Exec(ctx)
	return translateError(err)
}

// DeleteOlderThan removes heartbeats older than before. before is forced to
// UTC so a local-zoned cutoff cannot delete rows newer than intended (AGENTS.md rule 6).
func (r *HeartbeatRepo) DeleteOlderThan(ctx context.Context, before time.Time) error {
	_, err := r.db.NewDelete().Model((*repository.HeartbeatModel)(nil)).
		Where("time < ?", before.UTC()).
		Exec(ctx)
	return translateError(err)
}

func (r *HeartbeatRepo) SaveAggregate1m(ctx context.Context, agg *ports.Aggregate1m) error {
	m := repository.Aggregate1mFromDomain(agg)
	_, err := r.db.NewInsert().Model(m).
		ModelTableExpr("heartbeat_1m").
		On("DUPLICATE KEY UPDATE").
		Set("up_count = VALUES(up_count)").
		Set("down_count = VALUES(down_count)").
		Set("pending_count = VALUES(pending_count)").
		Set("maint_count = VALUES(maint_count)").
		Set("avg_ping = VALUES(avg_ping)").
		Set("min_ping = VALUES(min_ping)").
		Set("max_ping = VALUES(max_ping)").
		Set("total_checks = VALUES(total_checks)").
		Exec(ctx)
	return translateError(err)
}

func (r *HeartbeatRepo) SaveAggregate1h(ctx context.Context, agg *ports.Aggregate1h) error {
	m := repository.Aggregate1hFromDomain(agg)
	_, err := r.db.NewInsert().Model(m).
		ModelTableExpr("heartbeat_1h").
		On("DUPLICATE KEY UPDATE").
		Set("up_count = VALUES(up_count)").
		Set("down_count = VALUES(down_count)").
		Set("pending_count = VALUES(pending_count)").
		Set("maint_count = VALUES(maint_count)").
		Set("avg_ping = VALUES(avg_ping)").
		Set("min_ping = VALUES(min_ping)").
		Set("max_ping = VALUES(max_ping)").
		Set("total_checks = VALUES(total_checks)").
		Exec(ctx)
	return translateError(err)
}

func (r *HeartbeatRepo) SaveAggregate1d(ctx context.Context, agg *ports.Aggregate1d) error {
	m := repository.Aggregate1dFromDomain(agg)
	_, err := r.db.NewInsert().Model(m).
		ModelTableExpr("heartbeat_1d").
		On("DUPLICATE KEY UPDATE").
		Set("up_count = VALUES(up_count)").
		Set("down_count = VALUES(down_count)").
		Set("pending_count = VALUES(pending_count)").
		Set("maint_count = VALUES(maint_count)").
		Set("avg_ping = VALUES(avg_ping)").
		Set("min_ping = VALUES(min_ping)").
		Set("max_ping = VALUES(max_ping)").
		Set("total_checks = VALUES(total_checks)").
		Exec(ctx)
	return translateError(err)
}

func (r *HeartbeatRepo) GetAggregate1m(ctx context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1m, error) {
	var models []*repository.AggregateModel
	err := r.db.NewSelect().Model(&models).
		ModelTableExpr("heartbeat_1m AS aggregate_model").
		Where("monitor_id = ?", monitorID).
		Where("bucket >= ?", from.UTC()).
		OrderExpr("bucket ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*ports.Aggregate1m, len(models))
	for i, m := range models {
		out[i] = m.ToAggregate1m()
	}
	return out, nil
}

func (r *HeartbeatRepo) GetAggregate1h(ctx context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1h, error) {
	var models []*repository.AggregateModel
	err := r.db.NewSelect().Model(&models).
		ModelTableExpr("heartbeat_1h AS aggregate_model").
		Where("monitor_id = ?", monitorID).
		Where("bucket >= ?", from.UTC()).
		OrderExpr("bucket ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*ports.Aggregate1h, len(models))
	for i, m := range models {
		out[i] = m.ToAggregate1h()
	}
	return out, nil
}

func (r *HeartbeatRepo) GetAggregate1d(ctx context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1d, error) {
	var models []*repository.AggregateModel
	err := r.db.NewSelect().Model(&models).
		ModelTableExpr("heartbeat_1d AS aggregate_model").
		Where("monitor_id = ?", monitorID).
		Where("bucket >= ?", from.UTC()).
		OrderExpr("bucket ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*ports.Aggregate1d, len(models))
	for i, m := range models {
		out[i] = m.ToAggregate1d()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// NotificationRepo implements ports.NotificationRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// NotificationRepo implements ports.NotificationRepository.
type NotificationRepo struct{ db *bun.DB }

// NewNotificationRepo creates a MariaDB-backed notification repository.
func NewNotificationRepo(db *bun.DB) *NotificationRepo { return &NotificationRepo{db: db} }

func (r *NotificationRepo) Create(ctx context.Context, n *domain.Notification) error {
	m := repository.NotificationModelFromDomain(n)
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	if m.Config == nil {
		m.Config = repository.JSONField{}
	}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n.ID = m.ID
	n.CreatedAt = m.CreatedAt
	n.UpdatedAt = m.UpdatedAt
	return nil
}

func (r *NotificationRepo) GetByID(ctx context.Context, id int64) (*domain.Notification, error) {
	m := new(repository.NotificationModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *NotificationRepo) List(ctx context.Context, userID int64) ([]*domain.Notification, error) {
	var models []*repository.NotificationModel
	q := r.db.NewSelect().Model(&models)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	q = q.Order("id ASC")
	if err := q.Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Notification, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ListAll returns every notification in the install, ordered by id ascending.
func (r *NotificationRepo) ListAll(ctx context.Context) ([]*domain.Notification, error) {
	return r.List(ctx, 0)
}

func (r *NotificationRepo) Update(ctx context.Context, n *domain.Notification) error {
	m := repository.NotificationModelFromDomain(n)
	m.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *NotificationRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.NotificationModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// GetByMonitorID returns notifications linked to the given monitor via monitor_notification.
func (r *NotificationRepo) GetByMonitorID(ctx context.Context, monitorID int64) ([]*domain.Notification, error) {
	var models []*repository.NotificationModel
	err := r.db.NewSelect().
		Model(&models).
		Join("JOIN monitor_notification AS mn ON mn.notification_id = notification_model.id").
		Where("mn.monitor_id = ?", monitorID).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Notification, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// StatusPageRepo implements ports.StatusPageRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// StatusPageRepo implements ports.StatusPageRepository.
type StatusPageRepo struct{ db *bun.DB }

// NewStatusPageRepo creates a MariaDB-backed status page repository.
func NewStatusPageRepo(db *bun.DB) *StatusPageRepo { return &StatusPageRepo{db: db} }

func (r *StatusPageRepo) Create(ctx context.Context, sp *domain.StatusPage) error {
	m := repository.StatusPageModelFromDomain(sp)
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = now
	}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	sp.ID = m.ID
	sp.CreatedAt = m.CreatedAt
	sp.UpdatedAt = m.UpdatedAt
	return nil
}

func (r *StatusPageRepo) GetByID(ctx context.Context, id int64) (*domain.StatusPage, error) {
	m := new(repository.StatusPageModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *StatusPageRepo) GetBySlug(ctx context.Context, slug string) (*domain.StatusPage, error) {
	m := new(repository.StatusPageModel)
	if err := r.db.NewSelect().Model(m).Where("slug = ?", slug).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *StatusPageRepo) List(ctx context.Context) ([]*domain.StatusPage, error) {
	var models []*repository.StatusPageModel
	if err := r.db.NewSelect().Model(&models).Order("id ASC").Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.StatusPage, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *StatusPageRepo) Update(ctx context.Context, sp *domain.StatusPage) error {
	m := repository.StatusPageModelFromDomain(sp)
	m.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *StatusPageRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.StatusPageModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// TagRepo implements ports.TagRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// TagRepo implements ports.TagRepository.
type TagRepo struct{ db *bun.DB }

// NewTagRepo creates a MariaDB-backed tag repository.
func NewTagRepo(db *bun.DB) *TagRepo { return &TagRepo{db: db} }

func (r *TagRepo) Create(ctx context.Context, t *domain.Tag) error {
	m := repository.TagModelFromDomain(t)
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	t.ID = m.ID
	return nil
}

func (r *TagRepo) GetByID(ctx context.Context, id int64) (*domain.Tag, error) {
	m := new(repository.TagModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *TagRepo) List(ctx context.Context) ([]*domain.Tag, error) {
	var models []*repository.TagModel
	if err := r.db.NewSelect().Model(&models).Order("id ASC").Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Tag, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *TagRepo) Update(ctx context.Context, t *domain.Tag) error {
	m := repository.TagModelFromDomain(t)
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *TagRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.TagModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// MaintenanceRepo implements ports.MaintenanceRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// MaintenanceRepo implements ports.MaintenanceRepository.
type MaintenanceRepo struct{ db *bun.DB }

// NewMaintenanceRepo creates a MariaDB-backed maintenance repository.
func NewMaintenanceRepo(db *bun.DB) *MaintenanceRepo { return &MaintenanceRepo{db: db} }

func (r *MaintenanceRepo) Create(ctx context.Context, mw *domain.MaintenanceWindow) error {
	m := repository.MaintenanceWindowModelFromDomain(mw)
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	mw.ID = m.ID
	return nil
}

func (r *MaintenanceRepo) GetByID(ctx context.Context, id int64) (*domain.MaintenanceWindow, error) {
	m := new(repository.MaintenanceWindowModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *MaintenanceRepo) List(ctx context.Context, userID int64) ([]*domain.MaintenanceWindow, error) {
	var models []*repository.MaintenanceWindowModel
	q := r.db.NewSelect().Model(&models)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	q = q.Order("id ASC")
	if err := q.Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.MaintenanceWindow, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ListAll returns every maintenance window in the install, ordered by id ascending.
func (r *MaintenanceRepo) ListAll(ctx context.Context) ([]*domain.MaintenanceWindow, error) {
	return r.List(ctx, 0)
}

func (r *MaintenanceRepo) Update(ctx context.Context, mw *domain.MaintenanceWindow) error {
	m := repository.MaintenanceWindowModelFromDomain(mw)
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *MaintenanceRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.MaintenanceWindowModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// ProxyRepo implements ports.ProxyRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// ProxyRepo implements ports.ProxyRepository.
type ProxyRepo struct{ db *bun.DB }

// NewProxyRepo creates a MariaDB-backed proxy repository.
func NewProxyRepo(db *bun.DB) *ProxyRepo { return &ProxyRepo{db: db} }

func (r *ProxyRepo) Create(ctx context.Context, p *domain.Proxy) error {
	m := repository.ProxyModelFromDomain(p)
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	p.ID = m.ID
	return nil
}

func (r *ProxyRepo) GetByID(ctx context.Context, id int64) (*domain.Proxy, error) {
	m := new(repository.ProxyModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *ProxyRepo) List(ctx context.Context, userID int64) ([]*domain.Proxy, error) {
	var models []*repository.ProxyModel
	q := r.db.NewSelect().Model(&models)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	q = q.Order("id ASC")
	if err := q.Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Proxy, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *ProxyRepo) Update(ctx context.Context, p *domain.Proxy) error {
	m := repository.ProxyModelFromDomain(p)
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *ProxyRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.ProxyModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// APIKeyRepo implements ports.APIKeyRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// APIKeyRepo implements ports.APIKeyRepository.
type APIKeyRepo struct{ db *bun.DB }

// NewAPIKeyRepo creates a MariaDB-backed API key repository.
func NewAPIKeyRepo(db *bun.DB) *APIKeyRepo { return &APIKeyRepo{db: db} }

func (r *APIKeyRepo) Create(ctx context.Context, ak *domain.APIKey) error {
	m := repository.APIKeyModelFromDomain(ak)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	ak.ID = m.ID
	ak.CreatedAt = m.CreatedAt
	return nil
}

func (r *APIKeyRepo) GetByHash(ctx context.Context, hash string) (*domain.APIKey, error) {
	m := new(repository.APIKeyModel)
	if err := r.db.NewSelect().Model(m).Where("key_hash = ?", hash).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *APIKeyRepo) List(ctx context.Context, userID int64) ([]*domain.APIKey, error) {
	var models []*repository.APIKeyModel
	q := r.db.NewSelect().Model(&models)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	q = q.Order("id ASC")
	if err := q.Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.APIKey, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *APIKeyRepo) Update(ctx context.Context, ak *domain.APIKey) error {
	m := repository.APIKeyModelFromDomain(ak)
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *APIKeyRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.APIKeyModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// SettingRepo implements ports.SettingRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// SettingRepo implements ports.SettingRepository.
type SettingRepo struct{ db *bun.DB }

// NewSettingRepo creates a MariaDB-backed setting repository.
func NewSettingRepo(db *bun.DB) *SettingRepo { return &SettingRepo{db: db} }

func (r *SettingRepo) Get(ctx context.Context, key string) (string, error) {
	m := new(repository.SettingModel)
	err := r.db.NewSelect().Model(m).Where("setting_key = ?", key).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ports.ErrNotFound
		}
		return "", translateError(err)
	}
	return m.Value, nil
}

func (r *SettingRepo) Set(ctx context.Context, key, value string) error {
	m := &repository.SettingModel{Key: key, Value: value}
	_, err := r.db.NewInsert().Model(m).
		On("DUPLICATE KEY UPDATE").
		Set("value = EXCLUDED.value").
		Exec(ctx)
	return translateError(err)
}

func (r *SettingRepo) Delete(ctx context.Context, key string) error {
	_, err := r.db.NewDelete().Model((*repository.SettingModel)(nil)).Where("setting_key = ?", key).Exec(ctx)
	return translateError(err)
}

// ---------------------------------------------------------------------------
// MonitorNotificationRepo implements ports.MonitorNotificationRepository.
// ---------------------------------------------------------------------------

// MonitorNotificationRepo implements ports.MonitorNotificationRepository.
type MonitorNotificationRepo struct{ db *bun.DB }

// NewMonitorNotificationRepo creates a MariaDB-backed monitor-notification repo.
func NewMonitorNotificationRepo(db *bun.DB) *MonitorNotificationRepo {
	return &MonitorNotificationRepo{db: db}
}

func (r *MonitorNotificationRepo) Attach(ctx context.Context, monitorID, notificationID int64) error {
	m := &repository.MonitorNotificationModel{MonitorID: monitorID, NotificationID: notificationID}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	return translateError(err)
}

func (r *MonitorNotificationRepo) Detach(ctx context.Context, monitorID, notificationID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.MonitorNotificationModel)(nil)).
		Where("monitor_id = ?", monitorID).
		Where("notification_id = ?", notificationID).
		Exec(ctx)
	return translateError(err)
}

func (r *MonitorNotificationRepo) ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.MonitorNotification, error) {
	var models []*repository.MonitorNotificationModel
	err := r.db.NewSelect().Model(&models).
		Where("monitor_id = ?", monitorID).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.MonitorNotification, len(models))
	for i, m := range models {
		out[i] = m.ToDomainMonitorNotification()
	}
	return out, nil
}

func (r *MonitorNotificationRepo) ListByNotification(ctx context.Context, notificationID int64) ([]*domain.MonitorNotification, error) {
	var models []*repository.MonitorNotificationModel
	err := r.db.NewSelect().Model(&models).
		Where("notification_id = ?", notificationID).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.MonitorNotification, len(models))
	for i, m := range models {
		out[i] = m.ToDomainMonitorNotification()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// GroupNotificationRepo implements ports.GroupNotificationRepository.
// ---------------------------------------------------------------------------

// GroupNotificationRepo implements ports.GroupNotificationRepository.
type GroupNotificationRepo struct{ db *bun.DB }

// NewGroupNotificationRepo creates a MariaDB-backed group-notification repo.
func NewGroupNotificationRepo(db *bun.DB) *GroupNotificationRepo {
	return &GroupNotificationRepo{db: db}
}

// Attach links a notification to a group, idempotently.
//
// ON DUPLICATE KEY UPDATE id = id — a deliberate no-op assignment — makes the
// re-attach silent while leaving every other error loud. Plain INSERT IGNORE
// would ALSO downgrade a foreign-key violation to a warning, so attaching to a
// group that does not exist would return 204 and write nothing: exactly the
// stub-that-returns-success failure this codebase has already shipped once.
// The SQLite adapter's ON CONFLICT DO NOTHING has the same narrow scope.
func (r *GroupNotificationRepo) Attach(ctx context.Context, groupID, notificationID int64) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO group_notifications (group_id, notification_id) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE id = id`,
		groupID, notificationID,
	)
	return translateError(err)
}

func (r *GroupNotificationRepo) Detach(ctx context.Context, groupID, notificationID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.GroupNotificationModel)(nil)).
		Where("group_id = ?", groupID).
		Where("notification_id = ?", notificationID).
		Exec(ctx)
	return translateError(err)
}

func (r *GroupNotificationRepo) ListByGroup(ctx context.Context, groupID int64) ([]*domain.GroupNotification, error) {
	var models []*repository.GroupNotificationModel
	err := r.db.NewSelect().Model(&models).
		Where("group_id = ?", groupID).
		Order("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.GroupNotification, len(models))
	for i, m := range models {
		out[i] = m.ToDomainGroupNotification()
	}
	return out, nil
}

func (r *GroupNotificationRepo) ListByNotification(ctx context.Context, notificationID int64) ([]*domain.GroupNotification, error) {
	var models []*repository.GroupNotificationModel
	err := r.db.NewSelect().Model(&models).
		Where("notification_id = ?", notificationID).
		Order("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.GroupNotification, len(models))
	for i, m := range models {
		out[i] = m.ToDomainGroupNotification()
	}
	return out, nil
}

// ListNotificationsByGroup resolves the join in SQL — one query, not one per link.
func (r *GroupNotificationRepo) ListNotificationsByGroup(ctx context.Context, groupID int64) ([]*domain.Notification, error) {
	var models []*repository.NotificationModel
	err := r.db.NewSelect().
		Model(&models).
		Join("JOIN group_notifications AS gn ON gn.notification_id = notification_model.id").
		Where("gn.group_id = ?", groupID).
		Order("notification_model.id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Notification, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// IncidentRepo implements ports.IncidentRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// IncidentRepo implements ports.IncidentRepository.
type IncidentRepo struct{ db *bun.DB }

// NewIncidentRepo creates a MariaDB-backed incident repository.
func NewIncidentRepo(db *bun.DB) *IncidentRepo { return &IncidentRepo{db: db} }

func (r *IncidentRepo) Create(ctx context.Context, inc *domain.Incident) error {
	m := repository.IncidentModelFromDomain(inc)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	inc.ID = m.ID
	inc.CreatedAt = m.CreatedAt
	return nil
}

func (r *IncidentRepo) GetByID(ctx context.Context, id int64) (*domain.Incident, error) {
	m := new(repository.IncidentModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *IncidentRepo) ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.Incident, error) {
	var models []*repository.IncidentModel
	err := r.db.NewSelect().Model(&models).
		Where("status_page_id = ?", statusPageID).
		Order("created_at DESC", "id DESC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Incident, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *IncidentRepo) ListAll(ctx context.Context) ([]*domain.Incident, error) {
	var models []*repository.IncidentModel
	err := r.db.NewSelect().Model(&models).Order("created_at DESC", "id DESC").Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.Incident, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *IncidentRepo) Update(ctx context.Context, inc *domain.Incident) error {
	m := repository.IncidentModelFromDomain(inc)
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return translateError(err)
}

func (r *IncidentRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.IncidentModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// IncidentUpdateRepo implements ports.IncidentUpdateRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// IncidentUpdateRepo implements ports.IncidentUpdateRepository.
type IncidentUpdateRepo struct{ db *bun.DB }

// NewIncidentUpdateRepo creates a MariaDB-backed incident update repository.
func NewIncidentUpdateRepo(db *bun.DB) *IncidentUpdateRepo { return &IncidentUpdateRepo{db: db} }

func (r *IncidentUpdateRepo) Create(ctx context.Context, update *domain.IncidentUpdate) error {
	m := repository.IncidentUpdateModelFromDomain(update)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	update.ID = m.ID
	update.CreatedAt = m.CreatedAt
	return nil
}

func (r *IncidentUpdateRepo) ListByIncident(ctx context.Context, incidentID int64) ([]*domain.IncidentUpdate, error) {
	var models []*repository.IncidentUpdateModel
	err := r.db.NewSelect().Model(&models).
		Where("incident_id = ?", incidentID).
		Order("created_at ASC", "id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.IncidentUpdate, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *IncidentUpdateRepo) ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.IncidentUpdate, error) {
	var models []*repository.IncidentUpdateModel
	err := r.db.NewSelect().Model(&models).
		Where("status_page_id = ?", statusPageID).
		Order("incident_id ASC", "created_at ASC", "id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.IncidentUpdate, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// StatusPageCnameRepo implements ports.StatusPageCNAMERepository.
// ---------------------------------------------------------------------------

// StatusPageCnameRepo implements ports.StatusPageCNAMERepository.
type StatusPageCnameRepo struct{ db *bun.DB }

// NewStatusPageCnameRepo creates a MariaDB-backed status page CNAME repo.
func NewStatusPageCnameRepo(db *bun.DB) *StatusPageCnameRepo {
	return &StatusPageCnameRepo{db: db}
}

func (r *StatusPageCnameRepo) Create(ctx context.Context, cname *domain.StatusPageCNAME) error {
	m := repository.StatusPageCnameModelFromDomain(cname)
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	cname.ID = m.ID
	return nil
}

func (r *StatusPageCnameRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.StatusPageCnameModel)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

func (r *StatusPageCnameRepo) ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.StatusPageCNAME, error) {
	var models []*repository.StatusPageCnameModel
	err := r.db.NewSelect().Model(&models).
		Where("status_page_id = ?", statusPageID).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.StatusPageCNAME, len(models))
	for i, m := range models {
		out[i] = m.ToDomainCNAME()
	}
	return out, nil
}

func (r *StatusPageCnameRepo) GetByDomain(ctx context.Context, domain string) (*domain.StatusPageCNAME, error) {
	m := new(repository.StatusPageCnameModel)
	if err := r.db.NewSelect().Model(m).Where("domain = ?", domain).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomainCNAME(), nil
}

// ---------------------------------------------------------------------------
// StatusPageMonitorRepo implements ports.StatusPageMonitorRepository.
// ---------------------------------------------------------------------------

// StatusPageMonitorRepo implements ports.StatusPageMonitorRepository.
type StatusPageMonitorRepo struct{ db *bun.DB }

// NewStatusPageMonitorRepo creates a MariaDB-backed status page monitor repo.
func NewStatusPageMonitorRepo(db *bun.DB) *StatusPageMonitorRepo {
	return &StatusPageMonitorRepo{db: db}
}

func (r *StatusPageMonitorRepo) AddMonitor(ctx context.Context, spID, monitorID int64, displayOrder int) error {
	m := &repository.StatusPageMonitorModel{
		StatusPageID: spID,
		MonitorID:    monitorID,
		DisplayOrder: displayOrder,
	}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	return translateError(err)
}

func (r *StatusPageMonitorRepo) RemoveMonitor(ctx context.Context, spID, monitorID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.StatusPageMonitorModel)(nil)).
		Where("status_page_id = ?", spID).
		Where("monitor_id = ?", monitorID).
		Exec(ctx)
	return translateError(err)
}

// ReorderMonitors replaces the display_order of every monitor on a status page
// in one transaction. monitorIDs is ordered: index 0 → display_order 10,
// index 1 → display_order 20, etc. Any monitor currently assigned but absent
// from the list is removed. This is the only safe way to reorder — the
// alternative (remove + re-add) drops the row between the two calls, which
// can lose the assignment on a network error.
func (r *StatusPageMonitorRepo) ReorderMonitors(ctx context.Context, spID int64, monitorIDs []int64) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Remove existing assignments not in the new list.
		if len(monitorIDs) > 0 {
			if _, err := tx.NewDelete().
				Model((*repository.StatusPageMonitorModel)(nil)).
				Where("status_page_id = ?", spID).
				Where("monitor_id NOT IN (?)", bun.List(monitorIDs)).
				Exec(ctx); err != nil {
				return err
			}
		} else {
			if _, err := tx.NewDelete().
				Model((*repository.StatusPageMonitorModel)(nil)).
				Where("status_page_id = ?", spID).
				Exec(ctx); err != nil {
				return err
			}
		}

		// Upsert each assignment with its new display_order.
		for i, mid := range monitorIDs {
			order := (i + 1) * 10
			// MariaDB UPSERT: INSERT … ON DUPLICATE KEY UPDATE display_order.
			_, err := tx.NewRaw(
				`INSERT INTO status_page_monitors (status_page_id, monitor_id, display_order)
				 VALUES (?, ?, ?)
				 ON DUPLICATE KEY UPDATE display_order = ?`,
				spID, mid, order, order,
			).Exec(ctx)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *StatusPageMonitorRepo) ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.StatusPageMonitor, error) {
	var models []*repository.StatusPageMonitorModel
	err := r.db.NewSelect().Model(&models).
		Where("status_page_id = ?", statusPageID).
		Order("display_order ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.StatusPageMonitor, len(models))
	for i, m := range models {
		out[i] = m.ToDomainSPMonitor()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// StatusPageSubscriberRepo implements ports.StatusPageSubscriberRepository.
// ---------------------------------------------------------------------------

type StatusPageSubscriberRepo struct{ db *bun.DB }

func NewStatusPageSubscriberRepo(db *bun.DB) *StatusPageSubscriberRepo {
	return &StatusPageSubscriberRepo{db: db}
}

func (r *StatusPageSubscriberRepo) Create(ctx context.Context, sub *domain.StatusPageSubscriber) error {
	m := repository.StatusPageEmailSubscriberFromDomain(sub)
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	sub.ID = m.ID
	sub.CreatedAt = m.CreatedAt
	sub.UpdatedAt = m.UpdatedAt
	return nil
}

func (r *StatusPageSubscriberRepo) GetByID(ctx context.Context, id int64) (*domain.StatusPageSubscriber, error) {
	m := new(repository.StatusPageEmailSubscriberModel)
	err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *StatusPageSubscriberRepo) GetByPageAndEmail(ctx context.Context, statusPageID int64, email string) (*domain.StatusPageSubscriber, error) {
	m := new(repository.StatusPageEmailSubscriberModel)
	err := r.db.NewSelect().Model(m).
		Where("status_page_id = ? AND email = ?", statusPageID, email).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *StatusPageSubscriberRepo) Update(ctx context.Context, sub *domain.StatusPageSubscriber) error {
	m := repository.StatusPageEmailSubscriberFromDomain(sub)
	m.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(m).
		Column("email", "active", "confirmed_at", "updated_at").
		WherePK().
		Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	sub.UpdatedAt = m.UpdatedAt
	return nil
}

func (r *StatusPageSubscriberRepo) Delete(ctx context.Context, id int64) error {
	_, err := r.db.NewDelete().Model((*repository.StatusPageEmailSubscriberModel)(nil)).Where("id = ?", id).Exec(ctx)
	return translateError(err)
}

func (r *StatusPageSubscriberRepo) ListByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.StatusPageSubscriber, error) {
	var models []*repository.StatusPageEmailSubscriberModel
	err := r.db.NewSelect().Model(&models).
		Where("status_page_id = ?", statusPageID).
		Order("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.StatusPageSubscriber, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *StatusPageSubscriberRepo) ListConfirmedByStatusPage(ctx context.Context, statusPageID int64) ([]*domain.StatusPageSubscriber, error) {
	var models []*repository.StatusPageEmailSubscriberModel
	err := r.db.NewSelect().Model(&models).
		Where("status_page_id = ? AND active = TRUE", statusPageID).
		Order("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.StatusPageSubscriber, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *StatusPageSubscriberRepo) GetChannel(ctx context.Context, statusPageID int64) (*domain.StatusPageSubscriptionChannel, error) {
	m := new(repository.StatusPageSubscriptionChannelModel)
	err := r.db.NewSelect().Model(m).Where("status_page_id = ?", statusPageID).Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *StatusPageSubscriberRepo) SetChannel(ctx context.Context, channel *domain.StatusPageSubscriptionChannel) error {
	m := repository.StatusPageSubscriptionChannelFromDomain(channel)
	_, err := r.db.NewInsert().Model(m).
		On("DUPLICATE KEY UPDATE").
		Set("notification_id = VALUES(notification_id)").
		Exec(ctx)
	return translateError(err)
}

func (r *StatusPageSubscriberRepo) DeleteChannel(ctx context.Context, statusPageID int64) error {
	_, err := r.db.NewDelete().Model((*repository.StatusPageSubscriptionChannelModel)(nil)).
		Where("status_page_id = ?", statusPageID).
		Exec(ctx)
	return translateError(err)
}

func (r *StatusPageSubscriberRepo) ListStatusPageIDsForMonitors(ctx context.Context, monitorIDs []int64) ([]int64, error) {
	if len(monitorIDs) == 0 {
		return nil, nil
	}
	var ids []int64
	err := r.db.NewSelect().
		Model((*repository.StatusPageMonitorModel)(nil)).
		ColumnExpr("DISTINCT status_page_id").
		Where("monitor_id IN (?)", bun.List(monitorIDs)).
		OrderExpr("status_page_id ASC").
		Scan(ctx, &ids)
	if err != nil {
		return nil, translateError(err)
	}
	return ids, nil
}

// ---------------------------------------------------------------------------
// MonitorTagRepo implements ports.MonitorTagRepository.
// ---------------------------------------------------------------------------

type MonitorTagRepo struct{ db *bun.DB }

func NewMonitorTagRepo(db *bun.DB) *MonitorTagRepo { return &MonitorTagRepo{db: db} }

func (r *MonitorTagRepo) Assign(ctx context.Context, monitorID, tagID int64, value string) error {
	m := &repository.MonitorTagModel{MonitorID: monitorID, TagID: tagID, Value: value}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	return translateError(err)
}

func (r *MonitorTagRepo) Remove(ctx context.Context, monitorID, tagID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.MonitorTagModel)(nil)).
		Where("monitor_id = ?", monitorID).
		Where("tag_id = ?", tagID).
		Exec(ctx)
	return translateError(err)
}

func (r *MonitorTagRepo) ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.MonitorTag, error) {
	var models []*repository.MonitorTagModel
	err := r.db.NewSelect().Model(&models).
		Where("monitor_id = ?", monitorID).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.MonitorTag, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ListByMonitors fetches the tag assignments for many monitors in one query.
// Monitors with no tags are absent from the map (callers must render them as an
// empty array, not null). An empty id list issues no query at all.
func (r *MonitorTagRepo) ListByMonitors(ctx context.Context, monitorIDs []int64) (map[int64][]*domain.MonitorTag, error) {
	out := make(map[int64][]*domain.MonitorTag, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	var models []*repository.MonitorTagModel
	err := r.db.NewSelect().Model(&models).
		Where("monitor_id IN (?)", bun.List(monitorIDs)).
		Order("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	for _, m := range models {
		out[m.MonitorID] = append(out[m.MonitorID], m.ToDomain())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// UserPermissionRepo implements ports.UserPermissionRepository.
// ---------------------------------------------------------------------------

type UserPermissionRepo struct{ db *bun.DB }

// NewUserPermissionRepo creates a MariaDB-backed RBAC grant repository.
func NewUserPermissionRepo(db *bun.DB) *UserPermissionRepo { return &UserPermissionRepo{db: db} }

// Grant persists a view grant, idempotently: the uq_user_permissions_monitor /
// uq_user_permissions_group UNIQUE keys turn a repeat grant into a duplicate key,
// which becomes an UPDATE of include_descendants rather than an error —
// re-granting what a user already has is a no-op, and re-granting it with a
// different reach changes the reach. created_at is forced to UTC at the DB
// boundary.
//
// This is an upsert and not a swallowed duplicate because a grant now carries a
// payload beyond its identity; see the SQLite twin for the full reasoning. Unlike
// SQLite's ON CONFLICT, ON DUPLICATE KEY UPDATE needs no conflict target — it
// fires for whichever UNIQUE key was violated, so the monitor/group branch the
// SQLite twin has to make explicit is implicit here.
func (r *UserPermissionRepo) Grant(ctx context.Context, p *domain.UserPermission) error {
	m := repository.UserPermissionModelFromDomain(p)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	} else {
		m.CreatedAt = m.CreatedAt.UTC()
	}
	if _, err := r.db.NewInsert().Model(m).
		On("DUPLICATE KEY UPDATE").
		Set("include_descendants = VALUES(include_descendants)").
		Exec(ctx); err != nil {
		if errors.Is(translateError(err), ports.ErrConflict) {
			return nil
		}
		return translateError(err)
	}
	p.ID = m.ID
	p.CreatedAt = m.CreatedAt
	return nil
}

// RevokeMonitor drops a user's direct grant on one monitor. Revoking something
// that was never granted is not an error — the post-condition ("this user has no
// grant on this monitor") already holds.
func (r *UserPermissionRepo) RevokeMonitor(ctx context.Context, userID, monitorID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.UserPermissionModel)(nil)).
		Where("user_id = ?", userID).
		Where("monitor_id = ?", monitorID).
		Exec(ctx)
	return translateError(err)
}

// RevokeGroup drops a user's grant on one group.
func (r *UserPermissionRepo) RevokeGroup(ctx context.Context, userID, groupID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.UserPermissionModel)(nil)).
		Where("user_id = ?", userID).
		Where("group_id = ?", groupID).
		Exec(ctx)
	return translateError(err)
}

// RevokeAll drops every grant held by one user.
func (r *UserPermissionRepo) RevokeAll(ctx context.Context, userID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.UserPermissionModel)(nil)).
		Where("user_id = ?", userID).
		Exec(ctx)
	return translateError(err)
}

func (r *UserPermissionRepo) listWhere(ctx context.Context, col string, val int64) ([]*domain.UserPermission, error) {
	var models []*repository.UserPermissionModel
	err := r.db.NewSelect().Model(&models).
		Where(col+" = ?", val).
		Order("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.UserPermission, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ListByUser returns every grant held by one user.
func (r *UserPermissionRepo) ListByUser(ctx context.Context, userID int64) ([]*domain.UserPermission, error) {
	return r.listWhere(ctx, "user_id", userID)
}

// ListByMonitor returns every grant pointing at one monitor, across all users.
func (r *UserPermissionRepo) ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.UserPermission, error) {
	return r.listWhere(ctx, "monitor_id", monitorID)
}

// ListByGroup returns every grant pointing at one group, across all users.
func (r *UserPermissionRepo) ListByGroup(ctx context.Context, groupID int64) ([]*domain.UserPermission, error) {
	return r.listWhere(ctx, "group_id", groupID)
}

// ---------------------------------------------------------------------------
// MaintenanceWindowMonitorRepo implements ports.MaintenanceWindowMonitorRepository.
// ---------------------------------------------------------------------------

type MaintenanceWindowMonitorRepo struct{ db *bun.DB }

func NewMaintenanceWindowMonitorRepo(db *bun.DB) *MaintenanceWindowMonitorRepo {
	return &MaintenanceWindowMonitorRepo{db: db}
}

func (r *MaintenanceWindowMonitorRepo) Assign(ctx context.Context, maintenanceID, monitorID int64) error {
	m := &repository.MaintenanceWindowMonitorModel{MaintenanceWindowID: maintenanceID, MonitorID: monitorID}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	return translateError(err)
}

func (r *MaintenanceWindowMonitorRepo) Remove(ctx context.Context, maintenanceID, monitorID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.MaintenanceWindowMonitorModel)(nil)).
		Where("maintenance_window_id = ?", maintenanceID).
		Where("monitor_id = ?", monitorID).
		Exec(ctx)
	return translateError(err)
}

func (r *MaintenanceWindowMonitorRepo) ListByMaintenance(ctx context.Context, maintenanceID int64) ([]int64, error) {
	var models []*repository.MaintenanceWindowMonitorModel
	err := r.db.NewSelect().Model(&models).
		Where("maintenance_window_id = ?", maintenanceID).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	ids := make([]int64, len(models))
	for i, m := range models {
		ids[i] = m.MonitorID
	}
	return ids, nil
}

func (r *MaintenanceWindowMonitorRepo) ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.MaintenanceWindow, error) {
	// First get the maintenance window IDs linked to this monitor.
	var windowIDs []int64
	err := r.db.NewSelect().
		Model((*repository.MaintenanceWindowMonitorModel)(nil)).
		Column("maintenance_window_id").
		Where("monitor_id = ?", monitorID).
		Scan(ctx, &windowIDs)
	if err != nil {
		return nil, translateError(err)
	}
	if len(windowIDs) == 0 {
		return nil, nil
	}

	// Then fetch the maintenance windows.
	var models []*repository.MaintenanceWindowModel
	err = r.db.NewSelect().
		Model(&models).
		Where("id IN (?)", bun.List(windowIDs)).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.MaintenanceWindow, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// TLSInfoRepo implements ports.TLSInfoRepository backed by MariaDB.
// ---------------------------------------------------------------------------

// TLSInfoRepo implements ports.TLSInfoRepository.
type TLSInfoRepo struct{ db *bun.DB }

// NewTLSInfoRepo creates a MariaDB-backed TLS info repository.
func NewTLSInfoRepo(db *bun.DB) *TLSInfoRepo { return &TLSInfoRepo{db: db} }

func (r *TLSInfoRepo) Upsert(ctx context.Context, info *ports.TLSInfo) error {
	m := repository.TLSInfoModelFromPort(info)
	_, err := r.db.NewInsert().Model(m).
		On("DUPLICATE KEY UPDATE").
		Set("info_json = VALUES(info_json)").
		Set("checked_at = VALUES(checked_at)").
		Exec(ctx)
	return translateError(err)
}

func (r *TLSInfoRepo) GetByMonitorID(ctx context.Context, monitorID int64) (*ports.TLSInfo, error) {
	m := new(repository.TLSInfoModel)
	if err := r.db.NewSelect().Model(m).Where("monitor_id = ?", monitorID).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToPort()
}

// ---------------------------------------------------------------------------
// Repository — unified facade embedding all individual repos.
// ---------------------------------------------------------------------------

// Repository embeds all individual repository implementations backed by the
// same Bun DB handle. It satisfies every port interface through method
// promotion and can be passed wherever a single port is expected.
type Repository struct {
	*UserRepo
	*MonitorRepo
	*MonitorGroupRepo
	*HeartbeatRepo
	*TLSInfoRepo
	*NotificationRepo
	*StatusPageRepo
	*TagRepo
	*MaintenanceRepo
	*APIKeyRepo
	*ProxyRepo
	*SettingRepo
	*MonitorNotificationRepo
	*GroupNotificationRepo
	*IncidentRepo
	*IncidentUpdateRepo
	*StatusPageCnameRepo
	*StatusPageMonitorRepo
	*MonitorTagRepo
	*MaintenanceWindowMonitorRepo
	*StatusPageSubscriberRepo
	*WebAuthnCredentialRepo
	*UserPermissionRepo
	*OIDCIdentityRepo
	*ConfigKeyRepo
	*AlertRepo
	*EscalationPolicyRepo
	*EscalationAssignmentRepo
	*AlertEscalationRepo
}

// NewRepository creates a Repository backed by the given Bun DB.
func NewRepository(db *bun.DB) *Repository {
	return &Repository{
		UserRepo:                     NewUserRepo(db),
		MonitorRepo:                  NewMonitorRepo(db),
		MonitorGroupRepo:             NewMonitorGroupRepo(db),
		HeartbeatRepo:                NewHeartbeatRepo(db),
		TLSInfoRepo:                  NewTLSInfoRepo(db),
		NotificationRepo:             NewNotificationRepo(db),
		StatusPageRepo:               NewStatusPageRepo(db),
		TagRepo:                      NewTagRepo(db),
		MaintenanceRepo:              NewMaintenanceRepo(db),
		APIKeyRepo:                   NewAPIKeyRepo(db),
		ProxyRepo:                    NewProxyRepo(db),
		SettingRepo:                  NewSettingRepo(db),
		MonitorNotificationRepo:      NewMonitorNotificationRepo(db),
		GroupNotificationRepo:        NewGroupNotificationRepo(db),
		IncidentRepo:                 NewIncidentRepo(db),
		IncidentUpdateRepo:           NewIncidentUpdateRepo(db),
		MonitorTagRepo:               NewMonitorTagRepo(db),
		MaintenanceWindowMonitorRepo: NewMaintenanceWindowMonitorRepo(db),
		StatusPageCnameRepo:          NewStatusPageCnameRepo(db),
		StatusPageMonitorRepo:        NewStatusPageMonitorRepo(db),
		StatusPageSubscriberRepo:     NewStatusPageSubscriberRepo(db),
		WebAuthnCredentialRepo:       NewWebAuthnCredentialRepo(db),
		UserPermissionRepo:           NewUserPermissionRepo(db),
		OIDCIdentityRepo:             NewOIDCIdentityRepo(db),
		ConfigKeyRepo:                NewConfigKeyRepo(db),
		AlertRepo:                    NewAlertRepo(db),
		EscalationPolicyRepo:         NewEscalationPolicyRepo(db),
		EscalationAssignmentRepo:     NewEscalationAssignmentRepo(db),
		AlertEscalationRepo:          NewAlertEscalationRepo(db),
	}
}

// ---------------------------------------------------------------------------
// WebAuthnCredentialRepo implements ports.WebAuthnCredentialRepository.
// ---------------------------------------------------------------------------

type WebAuthnCredentialRepo struct{ db *bun.DB }

// NewWebAuthnCredentialRepo creates a MariaDB-backed passkey credential repo.
func NewWebAuthnCredentialRepo(db *bun.DB) *WebAuthnCredentialRepo {
	return &WebAuthnCredentialRepo{db: db}
}

func (r *WebAuthnCredentialRepo) Create(ctx context.Context, c *domain.WebAuthnCredential) error {
	m := repository.WebAuthnCredentialModelFromDomain(c)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	c.ID = m.ID
	c.CreatedAt = m.CreatedAt
	return nil
}

func (r *WebAuthnCredentialRepo) ListByUser(ctx context.Context, userID int64) ([]*domain.WebAuthnCredential, error) {
	var models []*repository.WebAuthnCredentialModel
	if err := r.db.NewSelect().Model(&models).Where("user_id = ?", userID).Order("id ASC").Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.WebAuthnCredential, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

func (r *WebAuthnCredentialRepo) GetByCredentialID(ctx context.Context, credentialID []byte) (*domain.WebAuthnCredential, error) {
	encoded := base64.RawURLEncoding.EncodeToString(credentialID)
	m := new(repository.WebAuthnCredentialModel)
	if err := r.db.NewSelect().Model(m).Where("credential_id = ?", encoded).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

func (r *WebAuthnCredentialRepo) UpdateUsage(ctx context.Context, id int64, signCount uint32, flags byte, cloneWarning bool, attachment string, lastUsedAt time.Time) error {
	res, err := r.db.NewUpdate().Model((*repository.WebAuthnCredentialModel)(nil)).
		Set("sign_count = ?", signCount).
		Set("flags = ?", flags).
		Set("clone_warning = ?", cloneWarning).
		Set("attachment = ?", attachment).
		Set("last_used_at = ?", lastUsedAt).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

func (r *WebAuthnCredentialRepo) Delete(ctx context.Context, id, userID int64) error {
	res, err := r.db.NewDelete().Model((*repository.WebAuthnCredentialModel)(nil)).
		Where("id = ? AND user_id = ?", id, userID).
		Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Compile-time interface checks
// ---------------------------------------------------------------------------

var (
	_ ports.UserRepository                     = (*UserRepo)(nil)
	_ ports.MonitorRepository                  = (*MonitorRepo)(nil)
	_ ports.MonitorGroupRepository             = (*MonitorGroupRepo)(nil)
	_ ports.HeartbeatRepository                = (*HeartbeatRepo)(nil)
	_ ports.NotificationRepository             = (*NotificationRepo)(nil)
	_ ports.StatusPageRepository               = (*StatusPageRepo)(nil)
	_ ports.TagRepository                      = (*TagRepo)(nil)
	_ ports.MaintenanceRepository              = (*MaintenanceRepo)(nil)
	_ ports.APIKeyRepository                   = (*APIKeyRepo)(nil)
	_ ports.SettingRepository                  = (*SettingRepo)(nil)
	_ ports.MonitorNotificationRepository      = (*MonitorNotificationRepo)(nil)
	_ ports.IncidentRepository                 = (*IncidentRepo)(nil)
	_ ports.StatusPageCNAMERepository          = (*StatusPageCnameRepo)(nil)
	_ ports.StatusPageMonitorRepository        = (*StatusPageMonitorRepo)(nil)
	_ ports.MonitorTagRepository               = (*MonitorTagRepo)(nil)
	_ ports.MaintenanceWindowMonitorRepository = (*MaintenanceWindowMonitorRepo)(nil)
	_ ports.WebAuthnCredentialRepository       = (*WebAuthnCredentialRepo)(nil)
	_ ports.TLSInfoRepository                  = (*TLSInfoRepo)(nil)
	_ ports.UserPermissionRepository           = (*UserPermissionRepo)(nil)
	_ ports.OIDCIdentityRepository             = (*OIDCIdentityRepo)(nil)
	_ ports.ConfigKeyRepository                = (*ConfigKeyRepo)(nil)
)

// ---------------------------------------------------------------------------
// ConfigKeyRepo implements ports.ConfigKeyRepository.
// ---------------------------------------------------------------------------

// ConfigKeyRepo is the MariaDB-backed config key store.
type ConfigKeyRepo struct{ db *bun.DB }

// NewConfigKeyRepo creates a MariaDB-backed config key repository.
func NewConfigKeyRepo(db *bun.DB) *ConfigKeyRepo { return &ConfigKeyRepo{db: db} }

// Upsert inserts or updates a config key mapping.
func (r *ConfigKeyRepo) Upsert(ctx context.Context, k *domain.ConfigKey) error {
	now := time.Now().UTC()
	_, _ = r.db.NewDelete().Model((*repository.ConfigKeyModel)(nil)).
		Where("resource_type = ? AND key_name = ?", k.ResourceType, k.KeyName).
		Exec(ctx)
	_, _ = r.db.NewDelete().Model((*repository.ConfigKeyModel)(nil)).
		Where("resource_type = ? AND resource_id = ?", k.ResourceType, k.ResourceID).
		Exec(ctx)
	m := repository.ConfigKeyModelFromDomain(k)
	m.CreatedAt = now
	m.UpdatedAt = now
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	k.ID = m.ID
	k.CreatedAt = m.CreatedAt
	k.UpdatedAt = m.UpdatedAt
	return nil
}

// GetByKey looks up a mapping by type + key name.
func (r *ConfigKeyRepo) GetByKey(ctx context.Context, resourceType, keyName string) (*domain.ConfigKey, error) {
	m := new(repository.ConfigKeyModel)
	if err := r.db.NewSelect().Model(m).
		Where("resource_type = ? AND key_name = ?", resourceType, keyName).
		Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// GetByResource looks up a mapping by type + resource id.
func (r *ConfigKeyRepo) GetByResource(ctx context.Context, resourceType string, resourceID int64) (*domain.ConfigKey, error) {
	m := new(repository.ConfigKeyModel)
	if err := r.db.NewSelect().Model(m).
		Where("resource_type = ? AND resource_id = ?", resourceType, resourceID).
		Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// ListByType returns every key for one resource type.
func (r *ConfigKeyRepo) ListByType(ctx context.Context, resourceType string) ([]*domain.ConfigKey, error) {
	var models []*repository.ConfigKeyModel
	if err := r.db.NewSelect().Model(&models).
		Where("resource_type = ?", resourceType).
		Order("key_name ASC").
		Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.ConfigKey, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// DeleteByKey removes one mapping.
func (r *ConfigKeyRepo) DeleteByKey(ctx context.Context, resourceType, keyName string) error {
	_, err := r.db.NewDelete().Model((*repository.ConfigKeyModel)(nil)).
		Where("resource_type = ? AND key_name = ?", resourceType, keyName).
		Exec(ctx)
	return translateError(err)
}

// DeleteByResource removes the mapping for a resource id.
func (r *ConfigKeyRepo) DeleteByResource(ctx context.Context, resourceType string, resourceID int64) error {
	_, err := r.db.NewDelete().Model((*repository.ConfigKeyModel)(nil)).
		Where("resource_type = ? AND resource_id = ?", resourceType, resourceID).
		Exec(ctx)
	return translateError(err)
}

// ---------------------------------------------------------------------------
// OIDCIdentityRepo implements ports.OIDCIdentityRepository.
// ---------------------------------------------------------------------------

// OIDCIdentityRepo is the MariaDB-backed OIDC identity store.
type OIDCIdentityRepo struct{ db *bun.DB }

// NewOIDCIdentityRepo creates a MariaDB-backed OIDC identity repository.
func NewOIDCIdentityRepo(db *bun.DB) *OIDCIdentityRepo {
	return &OIDCIdentityRepo{db: db}
}

// Create inserts a new OIDC identity link.
func (r *OIDCIdentityRepo) Create(ctx context.Context, id *domain.OIDCIdentity) error {
	m := repository.OIDCIdentityModelFromDomain(id)
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	id.ID = m.ID
	id.CreatedAt = m.CreatedAt
	id.UpdatedAt = m.UpdatedAt
	return nil
}

// GetByIssuerSubject looks up a link by the immutable OIDC pair.
func (r *OIDCIdentityRepo) GetByIssuerSubject(ctx context.Context, issuer, subject string) (*domain.OIDCIdentity, error) {
	m := new(repository.OIDCIdentityModel)
	if err := r.db.NewSelect().Model(m).
		Where("issuer = ? AND subject = ?", issuer, subject).
		Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// ListByUser returns every OIDC identity linked to a Phoenix user.
func (r *OIDCIdentityRepo) ListByUser(ctx context.Context, userID int64) ([]*domain.OIDCIdentity, error) {
	var models []*repository.OIDCIdentityModel
	if err := r.db.NewSelect().Model(&models).
		Where("user_id = ?", userID).
		Order("id ASC").
		Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.OIDCIdentity, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// TouchLogin updates email and last_login_at after a successful login.
func (r *OIDCIdentityRepo) TouchLogin(ctx context.Context, id int64, email string, lastLoginAt time.Time) error {
	res, err := r.db.NewUpdate().Model((*repository.OIDCIdentityModel)(nil)).
		Set("email = ?", email).
		Set("last_login_at = ?", lastLoginAt.UTC()).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// Delete removes one identity row by primary key.
func (r *OIDCIdentityRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.NewDelete().Model((*repository.OIDCIdentityModel)(nil)).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}
