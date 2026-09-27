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

	"github.com/google/uuid"
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
	Endpoint      string           `bun:"endpoint"`
	TLSPin        string           `bun:"tls_fingerprint"`
	Revision      int64            `bun:"revision"`
	CreatedAt     time.Time        `bun:"created_at"`
	UpdatedAt     time.Time        `bun:"updated_at"`
}

func (m probeRegistrationModel) domain() domain.Probe {
	return domain.Probe{
		ID: m.ID, Key: m.Key, Name: m.Name, Location: m.Location, Kind: m.Kind,
		Enabled: m.Enabled, Endpoint: m.Endpoint, TLSPin: m.TLSPin, Revision: m.Revision,
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

// Create stores one remote registration and assigns revision one. When the
// caller omits the identity a canonical UUID is allocated here so declarative
// callers never have to mint runtime identities themselves.
func (r *ProbeRegistryStore) Create(ctx context.Context, probe *domain.Probe) error {
	if probe != nil && probe.ID == "" {
		probe.ID = uuid.NewString()
	}
	if err := validateProbeRegistration(probe); err != nil {
		return err
	}
	now := time.Now().UTC()
	m := &probeRegistrationModel{
		ID: probe.ID, Key: probe.Key, Name: probe.Name, Location: probe.Location,
		Kind: probe.Kind, Enabled: probe.Enabled, Endpoint: probe.Endpoint, TLSPin: probe.TLSPin,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
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

// GetByKey returns one registration by its stable probe key, including the
// reserved local row.
func (r *ProbeRegistryStore) GetByKey(ctx context.Context, key string) (*domain.Probe, error) {
	m := new(probeRegistrationModel)
	if err := r.db.NewSelect().Model(m).Where("probe_key = ?", key).Scan(ctx); err != nil {
		return nil, fmt.Errorf("get probe by key: %w", probeRegistryError(err))
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
	// Optional frozen network trust: shape-checked when present, never required
	// here (the local operator flow may prepare a connection directly).
	if p.Endpoint != "" && (len(p.Endpoint) > 2048 || strings.TrimSpace(p.Endpoint) != p.Endpoint || strings.ContainsAny(p.Endpoint, "\t\r\n \x00")) {
		return fmt.Errorf("invalid probe endpoint: %w", domain.ErrValidation)
	}
	if p.TLSPin != "" && !domain.ValidKeyHash(p.TLSPin) {
		return fmt.Errorf("invalid probe TLS fingerprint: %w", domain.ErrValidation)
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
	bun.BaseModel       `bun:"table:monitor_probe_assignments"`
	MonitorID           int64     `bun:"monitor_id,pk"`
	ProbeID             string    `bun:"probe_id,pk"`
	Generation          int64     `bun:"generation"`
	Active              bool      `bun:"active"`
	CreatedAt           time.Time `bun:"created_at"`
	UpdatedAt           time.Time `bun:"updated_at"`
	ResourceBindingKey  string    `bun:"resource_binding_key"`
	ResourceBindingKind string    `bun:"resource_binding_kind"`
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
		// Match configuration activation/read lock order: local probe before
		// monitor rows. Inserting first deadlocks a concurrent SERIALIZABLE
		// source reader (probe -> monitors) and makes ordinary creation fail.
		if _, err := tx.NewUpdate().Table("probes").Set("revision = revision").Where("id = ?", domain.LocalProbeID).Exec(ctx); err != nil {
			return err
		}

		if _, err := tx.NewInsert().Model(model).Exec(ctx); err != nil {
			return err
		}
		return InitializeLocalAssignment(ctx, tx, model.ID)
	})
}

// CreateMonitorWithAssignments inserts a monitor together with its complete
// initial desired assignment set in one transaction. Creation can never leave
// a different set behind, so an explicit remote create is atomic exactly like
// the ordinary local default. Model defaults match the engine monitor Create:
// creation time, empty config and the 200-299 accepted-code default. Every
// member must be a currently enabled registration (live Replace semantics,
// never Restore), and resource bindings follow the same rules as replacement:
// a new or recreated Docker assignment requires an explicit binding and an
// omitted binding list supplies none.
func (r *ProbeAssignmentStore) CreateMonitorWithAssignments(ctx context.Context, m *domain.Monitor, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	if r == nil || r.db == nil || m == nil {
		return nil, domain.ErrInternal
	}
	ids, err := validateProbeMemberSet(probeIDs, policy)
	if err != nil {
		return nil, err
	}
	model := MonitorModelFromDomain(m)
	now := time.Now().UTC()
	model.CreatedAt, model.UpdatedAt = now, now
	if model.Config == nil {
		model.Config = JSONField{}
	}
	if len(model.AcceptedStatusCodes) == 0 {
		model.AcceptedStatusCodes = StringListField{"200-299"}
	}
	var out *domain.MonitorProbeAssignments
	err = runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		// Match configuration activation/read lock order: probes before monitor
		// rows, sorted so concurrent writers lock in one order.
		lockIDs := append(slices.Clone(ids), domain.LocalProbeID)
		slices.Sort(lockIDs)
		lockIDs = slices.Compact(lockIDs)
		for _, id := range lockIDs {
			if _, err := tx.NewUpdate().Table("probes").Set("revision = revision").Where("id = ?", id).Exec(ctx); err != nil {
				return err
			}
		}
		if _, err := tx.NewInsert().Model(model).Exec(ctx); err != nil {
			return err
		}
		for _, id := range ids {
			if err := requireRegisteredProbe(ctx, tx, id, true); err != nil {
				return err
			}
		}
		resources, _, err := replacementResourceBindings(ctx, tx, model.ID, ids, nil, bindings)
		if err != nil {
			return err
		}
		changeAt := assignmentChangeTime(time.Time{})
		set := &probeAssignmentSetModel{MonitorID: model.ID, Revision: 1,
			HealthPolicy: policy, CreatedAt: changeAt, UpdatedAt: changeAt}
		if _, err := tx.NewInsert().Model(set).Exec(ctx); err != nil {
			return err
		}
		for _, id := range ids {
			binding := resources[id]
			row := probeAssignmentModel{MonitorID: model.ID, ProbeID: id, Generation: 1, Active: true,
				ResourceBindingKey: binding.BindingKey, ResourceBindingKind: binding.Kind, CreatedAt: changeAt, UpdatedAt: changeAt}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
		}
		if err := writeAssignmentHistory(ctx, tx, model.ID, 1, policy, changeAt); err != nil {
			return err
		}
		out, err = readProbeAssignments(ctx, tx, model.ID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create monitor with assignments: %w", probeRegistryError(err))
	}
	m.ID, m.CreatedAt, m.UpdatedAt = model.ID, model.CreatedAt, model.UpdatedAt
	return out, nil
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
	return r.ReplaceWithBindings(ctx, monitorID, expectedRevision, probeIDs, policy, nil)
}

// ReplaceWithBindings atomically replaces membership and resource references.
// Nil preserves bindings on retained members; an explicit list replaces all
// bindings. New/recreated Docker assignments always require an explicit binding.
// Live inventory is checked by the authenticated transport before activation.
// Every member must be a currently enabled registration.
func (r *ProbeAssignmentStore) ReplaceWithBindings(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	return r.replaceWithBindings(ctx, monitorID, expectedRevision, probeIDs, policy, bindings, true)
}

// Restore commits a complete desired set that arrived from a declarative
// document (backup import or config apply). Members must be registered but
// need not be enabled: a restored or just-declared identity is disabled or
// unenrolled until an operator registers and enrolls it, and nothing executes
// on a disabled registration. Everything else is identical to
// ReplaceWithBindings: one complete set, optimistic revision, tombstoned
// removals, retained generations, explicit resource bindings and history.
func (r *ProbeAssignmentStore) Restore(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	return r.replaceWithBindings(ctx, monitorID, expectedRevision, probeIDs, policy, bindings, false)
}

func (r *ProbeAssignmentStore) replaceWithBindings(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding, requireEnabled bool) (*domain.MonitorProbeAssignments, error) {
	ids, err := validateProbeReplacement(monitorID, expectedRevision, probeIDs, policy)
	if err != nil {
		return nil, err
	}
	// Discover the previous members without holding an assignment lock. The
	// transaction rechecks this revision before using them. Configuration
	// publication locks a registration before reading assignments; removed
	// members also need that order because their foreign keys lock probes.
	current, err := r.GetByMonitorID(ctx, monitorID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return nil, err
	}
	if err != nil {
		// A legacy monitor without an assignment set is initialized inside this
		// mutation's own transaction below. Revision zero is never a valid
		// precondition: the caller expects the post-initialization revision.
		current = &domain.MonitorProbeAssignments{MonitorID: monitorID}
		if expectedRevision != 1 {
			return nil, ports.ErrConflict
		}
	} else if current.Revision != expectedRevision {
		return nil, ports.ErrConflict
	}
	lockIDs := append(slices.Clone(ids), domain.LocalProbeID)
	for _, member := range current.Assignments {
		lockIDs = append(lockIDs, member.ProbeID)
	}
	slices.Sort(lockIDs)
	lockIDs = slices.Compact(lockIDs)
	var out *domain.MonitorProbeAssignments
	err = runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		for _, id := range lockIDs {
			if _, err := tx.NewUpdate().Table("probes").Set("revision = revision").Where("id = ?", id).Exec(ctx); err != nil {
				return err
			}
		}
		// A no-op UPDATE is a write/row lock on both supported engines. Do not
		// inspect RowsAffected: MariaDB may report zero for unchanged values.
		if _, err := tx.NewUpdate().Table("monitor_probe_assignment_sets").Set("revision = revision").
			Where("monitor_id = ?", monitorID).Exec(ctx); err != nil {
			return err
		}
		set := new(probeAssignmentSetModel)
		if err := tx.NewSelect().Model(set).Where("monitor_id = ?", monitorID).Scan(ctx); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			// Initialize under this transaction only; reads never create rows.
			if err := InitializeLocalAssignment(ctx, tx, monitorID); err != nil {
				return err
			}
			if err := tx.NewSelect().Model(set).Where("monitor_id = ?", monitorID).Scan(ctx); err != nil {
				return err
			}
		}
		if set.Revision != expectedRevision {
			return ports.ErrConflict
		}
		// Sorting also gives concurrent replacements a consistent probe-row
		// lock order. A registration cannot be disabled during this commit.
		for _, id := range ids {
			if err := requireRegisteredProbe(ctx, tx, id, requireEnabled); err != nil {
				return err
			}
		}
		var previous []probeAssignmentModel
		if err := tx.NewSelect().Model(&previous).Where("monitor_id = ?", monitorID).Order("probe_id ASC").Scan(ctx); err != nil {
			return err
		}
		resources, changed, err := replacementResourceBindings(ctx, tx, monitorID, ids, previous, bindings)
		if err != nil {
			return err
		}
		if sameProbeAssignmentSet(previous, ids) && set.HealthPolicy == policy && !changed {
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
		for _, id := range ids {
			binding := resources[id]
			if _, err := tx.NewUpdate().Table("monitor_probe_assignments").
				Set("resource_binding_key = ?", binding.BindingKey).Set("resource_binding_kind = ?", binding.Kind).
				Set("updated_at = ?", now).Where("monitor_id = ? AND probe_id = ?", monitorID, id).Exec(ctx); err != nil {
				return err
			}
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
	if monitorID < 1 || revision < 1 {
		return nil, fmt.Errorf("invalid assignment identity or revision: %w", domain.ErrValidation)
	}
	return validateProbeMemberSet(probeIDs, policy)
}

// validateProbeMemberSet checks the complete desired member set and policy
// before anything is written: at least one member, no duplicates, canonical
// probe identities, and one of the two supported health policies.
func validateProbeMemberSet(probeIDs []string, policy domain.HealthPolicy) ([]string, error) {
	if len(probeIDs) == 0 ||
		(policy != domain.HealthPolicyAnyDown && policy != domain.HealthPolicyAllDown) {
		return nil, fmt.Errorf("invalid member set or policy: %w", domain.ErrValidation)
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
	return requireRegisteredProbe(ctx, tx, id, true)
}

// requireRegisteredProbe confirms the member exists, optionally that it is
// enabled, and holds its registration row lock inside the calling transaction.
func requireRegisteredProbe(ctx context.Context, tx bun.Tx, id string, requireEnabled bool) error {
	if _, err := tx.NewUpdate().Table("probes").Set("revision = revision").Where("id = ?", id).Exec(ctx); err != nil {
		return err
	}
	probe := new(probeRegistrationModel)
	if err := tx.NewSelect().Model(probe).Where("id = ?", id).Scan(ctx); err != nil {
		return err
	}
	if requireEnabled && !probe.Enabled {
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
	MonitorID           int64
	Revision            int64
	HealthPolicy        domain.HealthPolicy
	ProbeID             string
	Generation          int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ResourceBindingKey  string
	ResourceBindingKind string
}

func readProbeAssignments(ctx context.Context, db bun.IDB, monitorID int64) (*domain.MonitorProbeAssignments, error) {
	var rows []probeAssignmentReadModel
	err := db.NewSelect().TableExpr("monitor_probe_assignment_sets AS s").
		ColumnExpr("s.monitor_id, s.revision, s.health_policy, a.probe_id, a.generation, a.created_at, a.updated_at, a.resource_binding_key, a.resource_binding_kind").
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
		if row.ResourceBindingKey != "" {
			out.Assignments[i].ResourceBinding = &domain.ProbeResourceBinding{BindingKey: row.ResourceBindingKey, Kind: row.ResourceBindingKind}
		}
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
