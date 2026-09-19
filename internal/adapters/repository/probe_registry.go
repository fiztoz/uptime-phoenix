package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var (
	probeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	probeKey  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type probeRegistrationModel struct {
	bun.BaseModel `bun:"table:probes,alias:probe"`
	ID            string           `bun:"id,pk"`
	Key           string           `bun:"probe_key"`
	Name          string           `bun:"name"`
	Location      string           `bun:"location"`
	Kind          domain.ProbeKind `bun:"kind"`
	Enabled       bool             `bun:"enabled"`
	Revision      int64            `bun:"revision"`
	CreatedAt     time.Time        `bun:"created_at"`
	UpdatedAt     time.Time        `bun:"updated_at"`
}

func (m probeRegistrationModel) domain() domain.Probe {
	return domain.Probe{
		ID: m.ID, Key: m.Key, Name: m.Name, Location: m.Location, Kind: m.Kind,
		Enabled: m.Enabled, Revision: m.Revision,
		CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC(),
	}
}

// ProbeRegistryStore implements the dialect-neutral registration foundation.
// Concrete database adapters expose it through their own constructors. No
// scheduler, handler, credentials, or remote-runtime activation is wired here.
type ProbeRegistryStore struct{ db *bun.DB }

// NewProbeRegistryStore creates a Bun-backed registration store.
func NewProbeRegistryStore(db *bun.DB) *ProbeRegistryStore { return &ProbeRegistryStore{db: db} }

var _ ports.ProbeRegistryRepository = (*ProbeRegistryStore)(nil)

// Create stores one caller-allocated remote UUID and assigns revision one.
func (r *ProbeRegistryStore) Create(ctx context.Context, probe *domain.Probe) error {
	if err := validateProbeRegistration(probe); err != nil {
		return err
	}
	now := time.Now().UTC()
	m := &probeRegistrationModel{
		ID: probe.ID, Key: probe.Key, Name: probe.Name, Location: probe.Location,
		Kind: probe.Kind, Enabled: probe.Enabled, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := r.db.NewInsert().Model(m).Exec(ctx); err != nil {
		return fmt.Errorf("create probe: %w", probeRegistryError(err))
	}
	*probe = m.domain()
	return nil
}

// GetByID returns one registration, including the reserved local row.
func (r *ProbeRegistryStore) GetByID(ctx context.Context, id string) (*domain.Probe, error) {
	m := new(probeRegistrationModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, fmt.Errorf("get probe: %w", probeRegistryError(err))
	}
	p := m.domain()
	return &p, nil
}

// List returns all registrations in deterministic key/ID order.
func (r *ProbeRegistryStore) List(ctx context.Context) ([]domain.Probe, error) {
	var rows []probeRegistrationModel
	if err := r.db.NewSelect().Model(&rows).Order("probe_key ASC", "id ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("list probes: %w", err)
	}
	out := make([]domain.Probe, len(rows))
	for i := range rows {
		out[i] = rows[i].domain()
	}
	return out, nil
}

// Update changes display metadata/enabled state using a revision comparison.
// The local row and each remote registration's ID/key/kind remain immutable.
func (r *ProbeRegistryStore) Update(ctx context.Context, probe *domain.Probe, expectedRevision int64) error {
	if err := validateProbeRegistration(probe); err != nil {
		return err
	}
	if expectedRevision < 1 {
		return fmt.Errorf("probe revision must be positive: %w", domain.ErrValidation)
	}
	if expectedRevision == math.MaxInt64 {
		return fmt.Errorf("probe revision exhausted: %w", ports.ErrConflict)
	}
	previous, err := r.GetByID(ctx, probe.ID)
	if err != nil {
		return err
	}
	if previous.Key != probe.Key || previous.Kind != probe.Kind {
		return fmt.Errorf("probe identity is immutable: %w", domain.ErrValidation)
	}
	now := time.Now().UTC()
	result, err := r.db.NewUpdate().Table("probes").
		Set("name = ?", probe.Name).Set("location = ?", probe.Location).
		Set("enabled = ?", probe.Enabled).Set("revision = revision + 1").Set("updated_at = ?", now).
		Where("id = ? AND revision = ?", probe.ID, expectedRevision).Exec(ctx)
	if err != nil {
		return fmt.Errorf("update probe: %w", probeRegistryError(err))
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count probe update: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("probe revision changed: %w", ports.ErrConflict)
	}
	probe.Revision = expectedRevision + 1
	probe.CreatedAt = previous.CreatedAt
	probe.UpdatedAt = now
	return nil
}

func validateProbeRegistration(p *domain.Probe) error {
	if p == nil || p.Kind != domain.ProbeKindRemote || !validRemoteProbeID(p.ID) ||
		p.Key == domain.LocalProbeID || !probeKey.MatchString(p.Key) {
		return fmt.Errorf("remote probe requires a canonical UUID and nonreserved slug: %w", domain.ErrValidation)
	}
	if !utf8.ValidString(p.Name) || strings.TrimSpace(p.Name) == "" || utf8.RuneCountInString(p.Name) > 200 ||
		!utf8.ValidString(p.Location) || utf8.RuneCountInString(p.Location) > 255 {
		return fmt.Errorf("invalid probe name or location: %w", domain.ErrValidation)
	}
	return nil
}

func validRemoteProbeID(id string) bool {
	return probeUUID.MatchString(id) && id != "00000000-0000-0000-0000-000000000000"
}

type probeAssignmentSetModel struct {
	bun.BaseModel `bun:"table:monitor_probe_assignment_sets"`
	MonitorID     int64               `bun:"monitor_id,pk"`
	Revision      int64               `bun:"revision"`
	HealthPolicy  domain.HealthPolicy `bun:"health_policy"`
	CreatedAt     time.Time           `bun:"created_at"`
	UpdatedAt     time.Time           `bun:"updated_at"`
}

type probeAssignmentModel struct {
	bun.BaseModel `bun:"table:monitor_probe_assignments"`
	MonitorID     int64     `bun:"monitor_id,pk"`
	ProbeID       string    `bun:"probe_id,pk"`
	Generation    int64     `bun:"generation"`
	Active        bool      `bun:"active"`
	CreatedAt     time.Time `bun:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at"`
}

// ProbeAssignmentStore provides atomic desired-set replacement. Its transaction
// protects both the monitor revision and all assignment generations/tombstones.
type ProbeAssignmentStore struct{ db *bun.DB }

// NewProbeAssignmentStore creates a Bun-backed desired-assignment store.
func NewProbeAssignmentStore(db *bun.DB) *ProbeAssignmentStore { return &ProbeAssignmentStore{db: db} }

var _ ports.MonitorProbeAssignmentRepository = (*ProbeAssignmentStore)(nil)

// InitializeLocal idempotently initializes an existing monitor to local. It does
// not repair/rewrite existing desired state or cause remote execution.
func (r *ProbeAssignmentStore) InitializeLocal(ctx context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error) {
	if monitorID < 1 {
		return nil, fmt.Errorf("invalid monitor ID: %w", domain.ErrValidation)
	}
	var out *domain.MonitorProbeAssignments
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Acquire a write lock before reading on both engines. SQLite otherwise
		// cannot reliably upgrade a stale read snapshot when another writer wins.
		if _, err := tx.NewUpdate().Table("monitors").Set("id = id").Where("id = ?", monitorID).Exec(ctx); err != nil {
			return err
		}
		exists, err := tx.NewSelect().Table("monitors").Where("id = ?", monitorID).Exists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			return ports.ErrNotFound
		}
		if err := InitializeLocalAssignment(ctx, tx, monitorID); err != nil {
			return err
		}
		out, err = readProbeAssignments(ctx, tx, monitorID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("initialize local probe assignment: %w", probeRegistryError(err))
	}
	return out, nil
}

// InitializeLocalAssignment writes revision/generation-one local membership
// inside an open transaction. A monitor insert must use the same tx.
func InitializeLocalAssignment(ctx context.Context, tx bun.Tx, monitorID int64) error {
	set := new(probeAssignmentSetModel)
	err := tx.NewSelect().Model(set).Where("monitor_id = ?", monitorID).Scan(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := requireEnabledProbe(ctx, tx, domain.LocalProbeID); err != nil {
		return err
	}
	now := assignmentChangeTime(time.Time{})
	set = &probeAssignmentSetModel{MonitorID: monitorID, Revision: 1,
		HealthPolicy: domain.HealthPolicyAnyDown, CreatedAt: now, UpdatedAt: now}
	if _, err := tx.NewInsert().Model(set).Exec(ctx); err != nil {
		return err
	}
	assignment := &probeAssignmentModel{MonitorID: monitorID, ProbeID: domain.LocalProbeID,
		Generation: 1, Active: true, CreatedAt: now, UpdatedAt: now}
	if _, err := tx.NewInsert().Model(assignment).Exec(ctx); err != nil {
		return err
	}
	return writeAssignmentHistory(ctx, tx, monitorID, 1, set.HealthPolicy, now)
}

// CreateMonitorWithLocalAssignment inserts a monitor and its local assignment
// in one transaction so create/clone/import/restore cannot leave an unassigned row.
func CreateMonitorWithLocalAssignment(ctx context.Context, db *bun.DB, model *MonitorModel) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(model).Exec(ctx); err != nil {
			return err
		}
		return InitializeLocalAssignment(ctx, tx, model.ID)
	})
}

// ExecutableByLocal reports which monitors the hub worker may run and their local generation.
func (r *ProbeAssignmentStore) ExecutableByLocal(ctx context.Context, monitorIDs []int64) (map[int64]int64, error) {
	allowed := make(map[int64]int64, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return allowed, nil
	}
	var withSet []int64
	if err := r.db.NewSelect().Table("monitor_probe_assignment_sets").Column("monitor_id").
		Where("monitor_id IN (?)", bun.List(monitorIDs)).Scan(ctx, &withSet); err != nil {
		return nil, fmt.Errorf("list assignment sets: %w", err)
	}
	type localRow struct {
		MonitorID  int64 `bun:"monitor_id"`
		Generation int64 `bun:"generation"`
	}
	var withLocal []localRow
	if err := r.db.NewSelect().Table("monitor_probe_assignments").
		Column("monitor_id", "generation").
		Where("monitor_id IN (?) AND probe_id = ? AND active = ?", bun.List(monitorIDs), domain.LocalProbeID, true).
		Scan(ctx, &withLocal); err != nil {
		return nil, fmt.Errorf("list local assignments: %w", err)
	}
	sets := make(map[int64]struct{}, len(withSet))
	for _, id := range withSet {
		sets[id] = struct{}{}
	}
	local := make(map[int64]int64, len(withLocal))
	for _, row := range withLocal {
		local[row.MonitorID] = row.Generation
	}
	for _, id := range monitorIDs {
		if _, ok := sets[id]; !ok {
			allowed[id] = 1
			continue
		}
		if gen, ok := local[id]; ok {
			allowed[id] = gen
		}
	}
	return allowed, nil
}

// LocalHubExecutionSQL is the claim/list predicate for hub-side execution.
func LocalHubExecutionSQL(monitorIDExpr string) string {
	return "(NOT EXISTS (SELECT 1 FROM monitor_probe_assignment_sets s WHERE s.monitor_id = " + monitorIDExpr +
		") OR EXISTS (SELECT 1 FROM monitor_probe_assignments a WHERE a.monitor_id = " + monitorIDExpr +
		" AND a.probe_id = '" + domain.LocalProbeID + "' AND a.active))"
}

// GetByMonitorID reads a consistent active set in one statement, never creating
// an assignment as a side effect of a read.
func (r *ProbeAssignmentStore) GetByMonitorID(ctx context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error) {
	out, err := readProbeAssignments(ctx, r.db, monitorID)
	if err != nil {
		return nil, fmt.Errorf("get probe assignments: %w", probeRegistryError(err))
	}
	return out, nil
}

// Replace validates and commits a full desired set with optimistic revision
// control. Removed rows are tombstoned; their generations cannot be reused.
func (r *ProbeAssignmentStore) Replace(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy) (*domain.MonitorProbeAssignments, error) {
	ids, err := validateProbeReplacement(monitorID, expectedRevision, probeIDs, policy)
	if err != nil {
		return nil, err
	}
	var out *domain.MonitorProbeAssignments
	err = r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// A no-op UPDATE is a write/row lock on both supported engines. Do not
		// inspect RowsAffected: MariaDB may report zero for unchanged values.
		if _, err := tx.NewUpdate().Table("monitor_probe_assignment_sets").Set("revision = revision").
			Where("monitor_id = ?", monitorID).Exec(ctx); err != nil {
			return err
		}
		set := new(probeAssignmentSetModel)
		if err := tx.NewSelect().Model(set).Where("monitor_id = ?", monitorID).Scan(ctx); err != nil {
			return err
		}
		if set.Revision != expectedRevision {
			return ports.ErrConflict
		}
		// Sorting also gives concurrent replacements a consistent probe-row
		// lock order. A registration cannot be disabled during this commit.
		for _, id := range ids {
			if err := requireEnabledProbe(ctx, tx, id); err != nil {
				return err
			}
		}
		var previous []probeAssignmentModel
		if err := tx.NewSelect().Model(&previous).Where("monitor_id = ?", monitorID).Order("probe_id ASC").Scan(ctx); err != nil {
			return err
		}
		if sameProbeAssignmentSet(previous, ids) && set.HealthPolicy == policy {
			out, err = readProbeAssignments(ctx, tx, monitorID)
			return err
		}
		if set.Revision == math.MaxInt64 {
			return fmt.Errorf("assignment revision exhausted: %w", ports.ErrConflict)
		}
		now := assignmentChangeTime(set.UpdatedAt)
		if err := replaceProbeAssignmentRows(ctx, tx, monitorID, previous, ids, now); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model(set).Set("revision = revision + 1").Set("health_policy = ?", policy).
			Set("updated_at = ?", now).WherePK().Exec(ctx); err != nil {
			return err
		}
		if err := writeAssignmentHistory(ctx, tx, monitorID, expectedRevision+1, policy, now); err != nil {
			return err
		}
		out, err = readProbeAssignments(ctx, tx, monitorID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("replace probe assignments: %w", probeRegistryError(err))
	}
	return out, nil
}

func validateProbeReplacement(monitorID, revision int64, probeIDs []string, policy domain.HealthPolicy) ([]string, error) {
	if monitorID < 1 || revision < 1 || len(probeIDs) == 0 ||
		(policy != domain.HealthPolicyAnyDown && policy != domain.HealthPolicyAllDown) {
		return nil, fmt.Errorf("invalid assignment identity, revision, member set, or policy: %w", domain.ErrValidation)
	}
	ids := slices.Clone(probeIDs)
	slices.Sort(ids)
	for i, id := range ids {
		if (id != domain.LocalProbeID && !validRemoteProbeID(id)) || (i > 0 && ids[i-1] == id) {
			return nil, fmt.Errorf("invalid or duplicate probe ID: %w", domain.ErrValidation)
		}
	}
	return ids, nil
}

func requireEnabledProbe(ctx context.Context, tx bun.Tx, id string) error {
	if _, err := tx.NewUpdate().Table("probes").Set("revision = revision").Where("id = ?", id).Exec(ctx); err != nil {
		return err
	}
	probe := new(probeRegistrationModel)
	if err := tx.NewSelect().Model(probe).Where("id = ?", id).Scan(ctx); err != nil {
		return err
	}
	if !probe.Enabled {
		return fmt.Errorf("probe is disabled: %w", domain.ErrValidation)
	}
	return nil
}

func sameProbeAssignmentSet(previous []probeAssignmentModel, ids []string) bool {
	index := 0
	for _, assignment := range previous {
		if !assignment.Active {
			continue
		}
		if index == len(ids) || assignment.ProbeID != ids[index] {
			return false
		}
		index++
	}
	return index == len(ids)
}

func replaceProbeAssignmentRows(ctx context.Context, tx bun.Tx, monitorID int64, previous []probeAssignmentModel, ids []string, now time.Time) error {
	rows := make(map[string]probeAssignmentModel, len(previous))
	for _, row := range previous {
		rows[row.ProbeID] = row
	}
	for _, id := range ids {
		row, existed := rows[id]
		if existed && row.Active {
			continue
		}
		if existed {
			if row.Generation == math.MaxInt64 {
				return fmt.Errorf("assignment generation exhausted: %w", ports.ErrConflict)
			}
			if _, err := tx.NewUpdate().Model(&row).Set("active = ?", true).Set("generation = generation + 1").
				Set("updated_at = ?", now).WherePK().Exec(ctx); err != nil {
				return err
			}
		} else {
			row = probeAssignmentModel{MonitorID: monitorID, ProbeID: id, Generation: 1,
				Active: true, CreatedAt: now, UpdatedAt: now}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
		}
	}
	_, err := tx.NewUpdate().Table("monitor_probe_assignments").Set("active = ?", false).Set("updated_at = ?", now).
		Where("monitor_id = ? AND active = ?", monitorID, true).Where("probe_id NOT IN (?)", bun.List(ids)).Exec(ctx)
	return err
}

type probeAssignmentReadModel struct {
	MonitorID    int64
	Revision     int64
	HealthPolicy domain.HealthPolicy
	ProbeID      string
	Generation   int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func readProbeAssignments(ctx context.Context, db bun.IDB, monitorID int64) (*domain.MonitorProbeAssignments, error) {
	var rows []probeAssignmentReadModel
	err := db.NewSelect().TableExpr("monitor_probe_assignment_sets AS s").
		ColumnExpr("s.monitor_id, s.revision, s.health_policy, a.probe_id, a.generation, a.created_at, a.updated_at").
		Join("JOIN monitor_probe_assignments AS a ON a.monitor_id = s.monitor_id AND a.active = ?", true).
		Where("s.monitor_id = ?", monitorID).OrderExpr("a.probe_id ASC").Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ports.ErrNotFound
	}
	out := &domain.MonitorProbeAssignments{MonitorID: monitorID, Revision: rows[0].Revision,
		HealthPolicy: rows[0].HealthPolicy, Assignments: make([]domain.ProbeAssignment, len(rows))}
	for i, row := range rows {
		out.Assignments[i] = domain.ProbeAssignment{MonitorID: monitorID, ProbeID: row.ProbeID,
			Generation: row.Generation, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
	}
	return out, nil
}

func probeRegistryError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrNotFound
	}
	if err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "Duplicate entry")) {
		return ports.ErrConflict
	}
	return err
}
