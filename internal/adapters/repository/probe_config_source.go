package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	configSourceAssignments  = 10000
	configSourceDependencies = 1000
	// Bound intermediate relation rows as well as final encoded document bytes.
	configSourceEdges = domain.MaxProbeConfigBytes / 64
)

// LocalProbeConfigSourceStore reads the saved local graph in one database snapshot.
// It neither accepts arbitrary probe identities nor changes/activates configuration.
type LocalProbeConfigSourceStore struct{ db *bun.DB }

// NewLocalProbeConfigSourceStore creates a source reader for MariaDB or SQLite.
func NewLocalProbeConfigSourceStore(db *bun.DB) *LocalProbeConfigSourceStore {
	return &LocalProbeConfigSourceStore{db: db}
}

var _ ports.LocalProbeConfigSourceRepository = (*LocalProbeConfigSourceStore)(nil)

// ReadLocal includes current local assignments (including paused monitors), their
// ancestor groups and referenced dependencies. Tombstones and remote-only work
// are excluded. A legacy monitor without assignment state must be initialized
// explicitly before preparation; reads never invent a generation or write rows.
func (r *LocalProbeConfigSourceStore) ReadLocal(ctx context.Context) (*domain.LocalProbeConfigSource, error) {
	if r == nil || r.db == nil {
		return nil, domain.ErrValidation
	}
	var opts *sql.TxOptions
	if r.db.Dialect().Name() == dialect.MySQL {
		// Do not depend on a deployment's session/server isolation default.
		opts = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	var out *domain.LocalProbeConfigSource
	err := r.db.RunInTx(ctx, opts, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = readLocalConfigSource(ctx, tx)
		return err
	})
	if err != nil {
		// Driver JSON conversion errors can contain source values. Keep category
		// sentinels, never return a secret-bearing SQL/decoder error string.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		switch {
		case errors.Is(err, domain.ErrValidation):
			return nil, domain.ErrValidation
		case errors.Is(err, sql.ErrNoRows):
			return nil, ports.ErrNotFound
		default:
			return nil, fmt.Errorf("read local configuration source: %w", domain.ErrInternal)
		}
	}
	return out, nil
}

func readLocalConfigSource(ctx context.Context, tx bun.Tx) (*domain.LocalProbeConfigSource, error) {
	registration := new(probeRegistrationModel)
	if err := tx.NewSelect().Model(registration).Where("id = ?", domain.LocalProbeID).Scan(ctx); err != nil {
		return nil, err
	}
	if !registration.Enabled || registration.Kind != domain.ProbeKindLocal {
		return nil, domain.ErrValidation
	}
	legacy, err := tx.NewSelect().TableExpr("monitors AS m").
		Where("NOT EXISTS (SELECT 1 FROM monitor_probe_assignment_sets s WHERE s.monitor_id = m.id)").Exists(ctx)
	if err != nil {
		return nil, err
	}
	if legacy {
		return nil, domain.ErrValidation
	}
	rows, err := configSourceRows[probeAssignmentModel](ctx, tx.NewSelect().Where("probe_id = ? AND active = ?", domain.LocalProbeID, true).Order("monitor_id ASC"), configSourceAssignments)
	if err != nil {
		return nil, err
	}
	out := &domain.LocalProbeConfigSource{
		Probe: registration.domain(), Groups: map[int64]*domain.MonitorGroup{},
		MonitorPolicies: map[int64]int64{}, GroupPolicies: map[int64]int64{},
		Policies: map[int64]*domain.EscalationPolicy{}, Notifications: map[int64]*domain.Notification{},
		Templates: map[int64]*domain.NotificationTemplate{}, Proxies: map[int64]*domain.Proxy{},
		Assignments: make([]domain.ProbeConfigAssignment, 0, len(rows)),
	}
	ids := make([]int64, 0, len(rows))
	generation := make(map[int64]int64, len(rows))
	for _, row := range rows {
		ids = append(ids, row.MonitorID)
		generation[row.MonitorID] = row.Generation
	}
	monitors, err := configSourceRows[MonitorModel](ctx, tx.NewSelect().Where("id IN (?)", bun.List(ids)).Order("id ASC"), configSourceAssignments)
	if err != nil {
		return nil, err
	}
	if len(monitors) != len(rows) {
		return nil, domain.ErrValidation
	}
	for i := range monitors {
		out.Assignments = append(out.Assignments, domain.ProbeConfigAssignment{Monitor: monitors[i].ToDomain(), Generation: generation[monitors[i].ID]})
	}
	if err := readConfigGroups(ctx, tx, out); err != nil {
		return nil, err
	}
	if err := readConfigPolicies(ctx, tx, ids, out); err != nil {
		return nil, err
	}
	if err := readConfigDependencies(ctx, tx, ids, out); err != nil {
		return nil, err
	}
	if err := readConfigMaintenance(ctx, tx, ids, out); err != nil {
		return nil, err
	}
	return out, nil
}

func configSourceRows[T any](ctx context.Context, query *bun.SelectQuery, limit int) ([]T, error) {
	var rows []T
	if err := query.Model(&rows).Limit(limit + 1).Scan(ctx); err != nil {
		return nil, err
	}
	if len(rows) > limit {
		return nil, domain.ErrValidation
	}
	return rows, nil
}

func readConfigGroups(ctx context.Context, tx bun.Tx, out *domain.LocalProbeConfigSource) error {
	pending := map[int64]bool{}
	for _, assignment := range out.Assignments {
		if id := assignment.Monitor.GroupID; id != nil {
			pending[*id] = true
		}
	}
	for depth := 0; len(pending) > 0; depth++ {
		if depth >= 64 {
			return domain.ErrValidation
		}
		rows, err := configSourceRows[MonitorGroupModel](ctx, tx.NewSelect().Where("id IN (?)", bun.List(configSourceIDs(pending))).Order("id ASC"), configSourceAssignments-len(out.Groups))
		if err != nil {
			return err
		}
		if len(rows) != len(pending) {
			return domain.ErrValidation
		}
		for i := range rows {
			out.Groups[rows[i].ID] = rows[i].ToDomain()
		}
		pending = map[int64]bool{}
		for _, row := range rows {
			if row.ParentID != nil && out.Groups[*row.ParentID] == nil {
				pending[*row.ParentID] = true
			}
		}
	}
	return nil
}

func readConfigPolicies(ctx context.Context, tx bun.Tx, monitorIDs []int64, out *domain.LocalProbeConfigSource) error {
	monitorLinks, err := configSourceRows[EscalationPolicyMonitorModel](ctx, tx.NewSelect().Where("monitor_id IN (?)", bun.List(monitorIDs)), configSourceAssignments)
	if err != nil {
		return err
	}
	groupIDs := make([]int64, 0, len(out.Groups))
	for id := range out.Groups {
		groupIDs = append(groupIDs, id)
	}
	slices.Sort(groupIDs)
	groupLinks, err := configSourceRows[EscalationPolicyGroupModel](ctx, tx.NewSelect().Where("group_id IN (?)", bun.List(groupIDs)), configSourceAssignments)
	if err != nil {
		return err
	}
	ids := map[int64]bool{}
	for _, link := range monitorLinks {
		out.MonitorPolicies[link.MonitorID] = link.PolicyID
		ids[link.PolicyID] = true
	}
	for _, link := range groupLinks {
		out.GroupPolicies[link.GroupID] = link.PolicyID
		ids[link.PolicyID] = true
	}
	policies, err := configSourceRows[EscalationPolicyModel](ctx, tx.NewSelect().Where("id IN (?)", bun.List(configSourceIDs(ids))).Order("id ASC"), configSourceDependencies)
	if err != nil {
		return err
	}
	for i := range policies {
		out.Policies[policies[i].ID] = policies[i].ToDomain()
	}
	steps, err := configSourceRows[EscalationStepModel](ctx, tx.NewSelect().Where("policy_id IN (?)", bun.List(configSourceIDs(ids))).Order("policy_id ASC", "step_order ASC", "id ASC"), configSourceDependencies*20)
	if err != nil {
		return err
	}
	stepIDs := make([]int64, 0, len(steps))
	for _, step := range steps {
		stepIDs = append(stepIDs, step.ID)
	}
	links, err := configSourceRows[EscalationStepNotificationModel](ctx, tx.NewSelect().Where("step_id IN (?)", bun.List(stepIDs)).Order("step_id ASC", "notification_id ASC", "id ASC"), configSourceEdges)
	if err != nil {
		return err
	}
	channels := map[int64][]int64{}
	for _, link := range links {
		channels[link.StepID] = append(channels[link.StepID], link.NotificationID)
	}
	for _, step := range steps {
		policy := out.Policies[step.PolicyID]
		if policy == nil {
			return domain.ErrValidation
		}
		policy.Steps = append(policy.Steps, domain.EscalationStep{ID: step.ID, PolicyID: step.PolicyID, StepOrder: step.StepOrder, WaitMinutes: step.WaitMinutes, NotificationIDs: channels[step.ID]})
	}
	return nil
}

func readConfigDependencies(ctx context.Context, tx bun.Tx, monitorIDs []int64, out *domain.LocalProbeConfigSource) error {
	links, err := configSourceRows[MonitorNotificationModel](ctx, tx.NewSelect().Where("monitor_id IN (?)", bun.List(monitorIDs)).Order("monitor_id ASC", "notification_id ASC", "id ASC"), configSourceEdges)
	if err != nil {
		return err
	}
	channelIDs := map[int64]bool{}
	byMonitor := map[int64][]domain.MonitorNotification{}
	for _, link := range links {
		channelIDs[link.NotificationID] = true
		byMonitor[link.MonitorID] = append(byMonitor[link.MonitorID], *link.ToDomainMonitorNotification())
	}
	for _, policy := range out.Policies {
		for _, step := range policy.Steps {
			for _, id := range step.NotificationIDs {
				channelIDs[id] = true
			}
		}
	}
	channels, err := configSourceRows[NotificationModel](ctx, tx.NewSelect().Where("id IN (?)", bun.List(configSourceIDs(channelIDs))).Order("id ASC"), configSourceDependencies)
	if err != nil {
		return err
	}
	templateIDs, proxyIDs := map[int64]bool{}, map[int64]bool{}
	for i := range channels {
		out.Notifications[channels[i].ID] = channels[i].ToDomain()
		if id := channels[i].TemplateID; id != nil {
			templateIDs[*id] = true
		}
	}
	for i := range out.Assignments {
		assignment := &out.Assignments[i]
		assignment.NotificationLinks = byMonitor[assignment.Monitor.ID]
		if id := assignment.Monitor.ProxyID; id != nil {
			proxyIDs[*id] = true
		}
	}
	templates, err := configSourceRows[NotificationTemplateModel](ctx, tx.NewSelect().Where("id IN (?)", bun.List(configSourceIDs(templateIDs))).Order("id ASC"), configSourceDependencies)
	if err != nil {
		return err
	}
	for i := range templates {
		out.Templates[templates[i].ID] = templates[i].ToDomain()
	}
	proxies, err := configSourceRows[ProxyModel](ctx, tx.NewSelect().Where("id IN (?)", bun.List(configSourceIDs(proxyIDs))).Order("id ASC"), configSourceDependencies)
	if err != nil {
		return err
	}
	for i := range proxies {
		out.Proxies[proxies[i].ID] = proxies[i].ToDomain()
	}
	return readConfigTags(ctx, tx, monitorIDs, out)
}

type configTagRow struct {
	MonitorID int64
	Name      string
	Value     string
}

func readConfigTags(ctx context.Context, tx bun.Tx, monitorIDs []int64, out *domain.LocalProbeConfigSource) error {
	var rows []configTagRow
	err := tx.NewSelect().TableExpr("monitor_tags AS mt").ColumnExpr("mt.monitor_id, t.name, mt.value").
		Join("JOIN tags AS t ON t.id = mt.tag_id").Where("mt.monitor_id IN (?)", bun.List(monitorIDs)).
		OrderExpr("mt.monitor_id ASC, t.name ASC, mt.id ASC").Limit(configSourceEdges+1).Scan(ctx, &rows)
	if err != nil {
		return err
	}
	if len(rows) > configSourceEdges {
		return domain.ErrValidation
	}
	tags := map[int64][]domain.ProbeConfigTag{}
	for _, row := range rows {
		tags[row.MonitorID] = append(tags[row.MonitorID], domain.ProbeConfigTag{Name: row.Name, Value: row.Value})
	}
	for i := range out.Assignments {
		out.Assignments[i].Tags = tags[out.Assignments[i].Monitor.ID]
	}
	return nil
}

func readConfigMaintenance(ctx context.Context, tx bun.Tx, monitorIDs []int64, out *domain.LocalProbeConfigSource) error {
	// Empty link sets suppress nothing in MaintenanceService.IsActive. Do not
	// reinterpret an unlinked or remote-only window as global maintenance.
	rows, err := configSourceRows[MaintenanceWindowModel](ctx, tx.NewSelect().Where(
		"EXISTS (SELECT 1 FROM maintenance_window_monitors l WHERE l.maintenance_window_id = maintenance_window_model.id AND l.monitor_id IN (?))", bun.List(monitorIDs)).Order("id ASC"), configSourceDependencies)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	links, err := configSourceRows[MaintenanceWindowMonitorModel](ctx, tx.NewSelect().Where("maintenance_window_id IN (?) AND monitor_id IN (?)", bun.List(ids), bun.List(monitorIDs)).Order("maintenance_window_id ASC", "monitor_id ASC", "id ASC"), configSourceEdges)
	if err != nil {
		return err
	}
	byWindow := map[int64][]int64{}
	for _, link := range links {
		byWindow[link.MaintenanceWindowID] = append(byWindow[link.MaintenanceWindowID], link.MonitorID)
	}
	for i := range rows {
		assigned := byWindow[rows[i].ID]
		out.Maintenance = append(out.Maintenance, domain.ProbeConfigMaintenance{Window: rows[i].ToDomain(), MonitorIDs: assigned})
	}
	return nil
}

func configSourceIDs(ids map[int64]bool) []int64 {
	result := make([]int64, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}
