package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// installJournalAttemptLimit bounds create re-runs after concurrent admission
// races. A caller whose transaction keeps losing never receives a fresh grant.
const installJournalAttemptLimit = 8

// probeInstallOperationModel stores one durable installation intent: the frozen
// nonsecret identity, the requested artifact tuple (exactly one alternative is
// populated), the immutable owner, the internal create-admission UUID and the
// fixed lifecycle fields. No secret, key path, token, password, raw remote
// output or installed-success column exists here by construction.
type probeInstallOperationModel struct {
	bun.BaseModel        `bun:"table:probe_install_operations"`
	OperationID          string    `bun:"operation_id,pk"`
	SSHAddress           string    `bun:"ssh_address"`
	SSHUser              string    `bun:"ssh_user"`
	HostKeyFingerprint   string    `bun:"host_key_fingerprint"`
	ClientKeyFingerprint string    `bun:"client_key_fingerprint"`
	RequestedBy          int64     `bun:"requested_by"`
	LayoutVersion        int       `bun:"layout_version"`
	Mode                 string    `bun:"mode"`
	BinaryVersion        *string   `bun:"binary_version"`
	BinaryArchitecture   *string   `bun:"binary_architecture"`
	BinarySHA256         *string   `bun:"binary_sha256"`
	BinarySize           *int64    `bun:"binary_size"`
	ImageRepository      *string   `bun:"image_repository"`
	ImageVersion         *string   `bun:"image_version"`
	ImageArchitecture    *string   `bun:"image_architecture"`
	ImageIndexSHA256     *string   `bun:"image_index_sha256"`
	ImageIndexSize       *int64    `bun:"image_index_size"`
	ImageManifestSHA256  *string   `bun:"image_manifest_sha256"`
	ImageConfigSHA256    *string   `bun:"image_config_sha256"`
	ImageLayerCount      *int      `bun:"image_layer_count"`
	OwnerID              string    `bun:"owner_id"`
	AdmissionID          string    `bun:"admission_id"`
	State                string    `bun:"state"`
	ErrorCode            *string   `bun:"error_code"`
	CreatedAt            time.Time `bun:"created_at"`
	UpdatedAt            time.Time `bun:"updated_at"`
	DeadlineAt           time.Time `bun:"deadline_at"`
}

// probeInstallResourceModel stores one immutable canonical resource intent and
// at most one attempt receipt. Planned rows carry no attempt residue.
type probeInstallResourceModel struct {
	bun.BaseModel `bun:"table:probe_install_resources"`
	OperationID   string     `bun:"operation_id"`
	Ordinal       int        `bun:"ordinal"`
	Kind          string     `bun:"kind"`
	Reference     string     `bun:"reference"`
	Owned         bool       `bun:"owned"`
	State         string     `bun:"state"`
	AttemptID     *string    `bun:"attempt_id"`
	ErrorCode     *string    `bun:"error_code"`
	StartedAt     *time.Time `bun:"started_at"`
	FinishedAt    *time.Time `bun:"finished_at"`
}

// probeInstallGraph is one locked operation row plus its complete canonical
// resource rows. It is only ever built from a current read taken under the
// operation row lock and is validated as a whole before it is used.
type probeInstallGraph struct {
	operation probeInstallOperationModel
	resources []probeInstallResourceModel
}

// installJournalTime normalizes database-clock samples and newly written stamps.
// Stored stamps retain their precision on reads so corruption is not repaired.
func installJournalTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// installJournalStamp keeps updated/finished stamps monotonic across DB-clock
// rollback: a rewound clock never moves a stored stamp backwards and grants
// nothing.
func installJournalStamp(stored, now time.Time) time.Time {
	now = installJournalTime(now)
	if previous := installJournalTime(stored); previous.After(now) {
		return previous
	}
	return now
}

// installJournalStartStamp clamps a new attempt start to the causal floor: it
// may never precede creation or the previous applied attempt's finish, and it
// may never rewind past the stored clock. It stays strictly before the fixed
// deadline because every input does.
func installJournalStartStamp(now, created, previousFinish time.Time) time.Time {
	started := installJournalTime(now)
	if c := installJournalTime(created); c.After(started) {
		started = c
	}
	if !previousFinish.IsZero() {
		if p := installJournalTime(previousFinish); p.After(started) {
			started = p
		}
	}
	return started
}

// installJournalFinishStamp clamps a completion stamp to its own attempt start
// so finished never precedes started even under clock rollback.
func installJournalFinishStamp(started, now time.Time) time.Time {
	finished := installJournalTime(now)
	if s := installJournalTime(started); s.After(finished) {
		return s
	}
	return finished
}

// installJournalTerminal reports whether the stored operation may no longer
// change outcome.
func installJournalTerminal(state string) bool {
	return state == domain.ProbeInstallFailed ||
		state == domain.ProbeInstallCanceled ||
		state == domain.ProbeInstallInterrupted
}

// installJournalJoined reports whether an executor has been admitted to the
// operation (Begin granted). Only a joined owner may finish work: a queued
// operation has no executor and is finished only by the queued-cancel path.
func installJournalJoined(state string) bool {
	return state == domain.ProbeInstallRunning ||
		state == domain.ProbeInstallResourcesReady ||
		state == domain.ProbeInstallCancelRequested
}

// installJournalKind accepts only the frozen layout-v1 resource kinds. Resource
// references are always derived by the domain, never caller-controlled.
func installJournalKind(kind string) bool {
	switch kind {
	case "data_directory", "artifact_directory", "unit_file", "service", "image_cache", "container":
		return true
	}
	return false
}

// errInstallJournalRaced aborts (and thereby rolls back) a create transaction
// that lost a concurrent admission race so the caller can re-run it and resolve
// through the exact-retry identity contract. It is never a fresh grant.
var errInstallJournalRaced = errors.New("probe install journal create raced a concurrent writer")

// installJournalRaced detects primary-key races and SQLite's read-to-write
// upgrade aborts between separate connections so a create transaction can be
// re-run instead of surfacing a raw driver error.
func installJournalRaced(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "UNIQUE constraint failed") ||
		strings.Contains(message, "Duplicate entry") ||
		strings.Contains(message, "database is locked") ||
		strings.Contains(message, "SQLITE_BUSY")
}

// installJournalCommit carries the transaction outcome across the commit
// boundary. An expiry reconciliation is honest durable state and must commit
// even when the caller's requested transition is refused afterwards; the
// refusal is then delivered after commit instead of rolling the reconciliation
// back with it.
type installJournalCommit struct {
	reconciled bool
	pending    error
}

// fail refuses a requested transition. When this transaction already performed
// an expiry reconciliation, the reconciliation is kept (the callback returns
// nil so it commits) and the failure is delivered to the caller afterwards.
func (c *installJournalCommit) fail(err error) error {
	if c.reconciled {
		c.pending = err
		return nil
	}
	return err
}

// begin resets all per-attempt outputs. runConfigAuthorityTx may run the
// callback more than once; a retried transaction must never inherit an
// earlier attempt's receipt or fresh-grant verdict.
func (c *installJournalCommit) begin() {
	c.reconciled = false
	c.pending = nil
}

// resource converts one stored resource row and fails closed on any attempt
// residue or missing receipt field the frozen lifecycle forbids.
func (m probeInstallResourceModel) resource() (domain.ProbeInstallResource, error) {
	out := domain.ProbeInstallResource{
		Spec:  domain.ProbeInstallResourceSpec{Ordinal: m.Ordinal, Kind: m.Kind, Reference: m.Reference, Owned: m.Owned},
		State: m.State,
	}
	if m.State == domain.ProbeInstallResourcePlanned {
		if m.AttemptID != nil || m.ErrorCode != nil || m.StartedAt != nil || m.FinishedAt != nil {
			return domain.ProbeInstallResource{}, fmt.Errorf("planned resource %d of %s carries attempt residue: %w", m.Ordinal, m.OperationID, domain.ErrValidation)
		}
		return out, nil
	}
	if m.AttemptID == nil || m.StartedAt == nil {
		return domain.ProbeInstallResource{}, fmt.Errorf("open resource %d of %s misses its attempt: %w", m.Ordinal, m.OperationID, domain.ErrValidation)
	}
	out.AttemptID = *m.AttemptID
	out.StartedAt = m.StartedAt.UTC()
	if m.State == domain.ProbeInstallResourceInFlight {
		if m.ErrorCode != nil || m.FinishedAt != nil {
			return domain.ProbeInstallResource{}, fmt.Errorf("in-flight resource %d of %s carries a finished receipt: %w", m.Ordinal, m.OperationID, domain.ErrValidation)
		}
		return out, nil
	}
	if m.FinishedAt == nil {
		return domain.ProbeInstallResource{}, fmt.Errorf("completed resource %d of %s misses its finish: %w", m.Ordinal, m.OperationID, domain.ErrValidation)
	}
	out.FinishedAt = m.FinishedAt.UTC()
	if m.State == domain.ProbeInstallResourceApplied {
		// Applied receipts carry no error code at all.
		if m.ErrorCode != nil {
			return domain.ProbeInstallResource{}, fmt.Errorf("applied resource %d of %s carries an error code: %w", m.Ordinal, m.OperationID, domain.ErrValidation)
		}
		return out, nil
	}
	if m.ErrorCode == nil || *m.ErrorCode == "" {
		return domain.ProbeInstallResource{}, fmt.Errorf("completed resource %d of %s misses its outcome: %w", m.Ordinal, m.OperationID, domain.ErrValidation)
	}
	out.ErrorCode = *m.ErrorCode
	return out, nil
}

// receipt assembles the complete stored graph and fails closed with
// domain.ErrValidation on corruption: a row whose identity, tuple, admission
// ID, state, outcome or timestamp ordering violates the frozen domain contract
// is refused instead of being served or repaired.
func (g probeInstallGraph) receipt() (domain.ProbeInstallOperation, error) {
	m := g.operation
	if !domain.ValidProbeOperationID(m.AdmissionID) {
		return domain.ProbeInstallOperation{}, fmt.Errorf("corrupt install admission on %s: %w", m.OperationID, domain.ErrValidation)
	}
	identity := domain.ProbeInstallIdentity{
		OperationID:          m.OperationID,
		Host:                 domain.ProbeSSHHost{Address: m.SSHAddress, User: m.SSHUser},
		HostKeyFingerprint:   m.HostKeyFingerprint,
		ClientKeyFingerprint: m.ClientKeyFingerprint,
		RequestedBy:          m.RequestedBy,
		LayoutVersion:        m.LayoutVersion,
		Mode:                 m.Mode,
	}
	switch m.Mode {
	case domain.ProbeInstallSystemd:
		if m.BinaryVersion == nil || m.BinaryArchitecture == nil || m.BinarySHA256 == nil || m.BinarySize == nil ||
			m.ImageRepository != nil || m.ImageVersion != nil || m.ImageArchitecture != nil ||
			m.ImageIndexSHA256 != nil || m.ImageIndexSize != nil || m.ImageManifestSHA256 != nil ||
			m.ImageConfigSHA256 != nil || m.ImageLayerCount != nil {
			return domain.ProbeInstallOperation{}, fmt.Errorf("corrupt systemd artifact tuple on %s: %w", m.OperationID, domain.ErrValidation)
		}
		identity.Binary = domain.ProbeBinaryPin{
			Version: *m.BinaryVersion, Architecture: *m.BinaryArchitecture,
			SHA256: *m.BinarySHA256, Size: *m.BinarySize,
		}
	case domain.ProbeInstallDocker:
		if m.ImageRepository == nil || m.ImageVersion == nil || m.ImageArchitecture == nil ||
			m.ImageIndexSHA256 == nil || m.ImageIndexSize == nil || m.ImageManifestSHA256 == nil ||
			m.ImageConfigSHA256 == nil || m.ImageLayerCount == nil ||
			m.BinaryVersion != nil || m.BinaryArchitecture != nil || m.BinarySHA256 != nil || m.BinarySize != nil {
			return domain.ProbeInstallOperation{}, fmt.Errorf("corrupt docker artifact tuple on %s: %w", m.OperationID, domain.ErrValidation)
		}
		identity.Image = domain.ProbeImageSelection{
			Pin: domain.ProbeImagePin{
				Repository: *m.ImageRepository, Version: *m.ImageVersion,
				Architecture: *m.ImageArchitecture, IndexSHA256: *m.ImageIndexSHA256, IndexSize: *m.ImageIndexSize,
			},
			ManifestSHA256: *m.ImageManifestSHA256,
			ConfigSHA256:   *m.ImageConfigSHA256,
			LayerCount:     *m.ImageLayerCount,
		}
	default:
		return domain.ProbeInstallOperation{}, fmt.Errorf("corrupt install mode on %s: %w", m.OperationID, domain.ErrValidation)
	}
	out := domain.ProbeInstallOperation{
		Identity:   identity,
		OwnerID:    m.OwnerID,
		State:      m.State,
		CreatedAt:  m.CreatedAt.UTC(),
		UpdatedAt:  m.UpdatedAt.UTC(),
		DeadlineAt: m.DeadlineAt.UTC(),
	}
	if m.ErrorCode != nil {
		if *m.ErrorCode == "" {
			return domain.ProbeInstallOperation{}, fmt.Errorf("empty install error code on %s: %w", m.OperationID, domain.ErrValidation)
		}
		out.ErrorCode = *m.ErrorCode
	}
	out.Resources = make([]domain.ProbeInstallResource, 0, len(g.resources))
	for _, row := range g.resources {
		resource, err := row.resource()
		if err != nil {
			return domain.ProbeInstallOperation{}, err
		}
		out.Resources = append(out.Resources, resource)
	}
	if !out.Valid() {
		return domain.ProbeInstallOperation{}, fmt.Errorf("corrupt probe install journal %s: %w", m.OperationID, domain.ErrValidation)
	}
	return out, nil
}

// resourceIndex locates the one canonical resource of the given kind.
func (g probeInstallGraph) resourceIndex(kind string) (int, bool) {
	for i := range g.resources {
		if g.resources[i].Kind == kind {
			return i, true
		}
	}
	return 0, false
}

// lockProbeInstallRow serializes every stored transition on the operation row
// and reads it back with a current locking read. The database clock is sampled
// by callers only after this lock is held.
func lockProbeInstallRow(ctx context.Context, tx bun.Tx, operationID string) (probeInstallOperationModel, error) {
	if _, err := tx.ExecContext(ctx,
		"UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", operationID); err != nil {
		return probeInstallOperationModel{}, err
	}
	row := new(probeInstallOperationModel)
	q := tx.NewSelect().Model(row).Where("operation_id = ?", operationID)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return probeInstallOperationModel{}, ports.ErrNotFound
		}
		return probeInstallOperationModel{}, err
	}
	return *row, nil
}

// readProbeInstallResources reads the complete canonical resource rows in
// ordinal order with a current locking read.
func readProbeInstallResources(ctx context.Context, tx bun.Tx, operationID string) ([]probeInstallResourceModel, error) {
	var rows []probeInstallResourceModel
	q := tx.NewSelect().Model(&rows).Where("operation_id = ?", operationID).Order("ordinal ASC")
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

// loadProbeInstallGraph locks the operation, reads the complete stored graph
// and validates it whole before any expiry reconciliation or transition is
// considered. Corruption is refused with domain.ErrValidation, never repaired.
func loadProbeInstallGraph(ctx context.Context, tx bun.Tx, operationID string) (probeInstallGraph, error) {
	row, err := lockProbeInstallRow(ctx, tx, operationID)
	if err != nil {
		return probeInstallGraph{}, err
	}
	resources, err := readProbeInstallResources(ctx, tx, operationID)
	if err != nil {
		return probeInstallGraph{}, err
	}
	graph := probeInstallGraph{operation: row, resources: resources}
	if _, err := graph.receipt(); err != nil {
		return probeInstallGraph{}, err
	}
	return graph, nil
}

// reconcileExpiry records honest cold-expiry state: a hard-expired nonterminal
// operation becomes interrupted/deadline_expired and every in-flight action
// becomes unknown/deadline_expired atomically. Historical applied and
// not-started actions stay untouched and planned actions are never fabricated
// as executed. The caller must commit this even when it then refuses the
// requested transition.
func (g *probeInstallGraph) reconcileExpiry(ctx context.Context, tx bun.Tx, now time.Time) (bool, error) {
	op := &g.operation
	// Already terminal or still inside the fixed lifetime: nothing to reconcile.
	if installJournalTerminal(op.State) || installJournalTime(now).Before(installJournalTime(op.DeadlineAt)) {
		return false, nil
	}
	updated := installJournalStamp(op.UpdatedAt, now)
	for i := range g.resources {
		resource := &g.resources[i]
		if resource.State != domain.ProbeInstallResourceInFlight {
			continue
		}
		started := installJournalTime(*resource.StartedAt)
		finished := installJournalFinishStamp(started, now)
		code := "deadline_expired"
		if _, err := tx.NewUpdate().Table("probe_install_resources").
			Set("state = ?", domain.ProbeInstallResourceUnknown).
			Set("error_code = ?", code).
			Set("finished_at = ?", finished).
			Where("operation_id = ? AND ordinal = ?", op.OperationID, resource.Ordinal).Exec(ctx); err != nil {
			return false, err
		}
		resource.State = domain.ProbeInstallResourceUnknown
		resource.ErrorCode = &code
		resource.FinishedAt = &finished
	}
	code := "deadline_expired"
	if _, err := tx.NewUpdate().Table("probe_install_operations").
		Set("state = ?", domain.ProbeInstallInterrupted).
		Set("error_code = ?", code).
		Set("updated_at = ?", updated).
		Where("operation_id = ?", op.OperationID).Exec(ctx); err != nil {
		return false, err
	}
	op.State = domain.ProbeInstallInterrupted
	op.ErrorCode = &code
	op.UpdatedAt = updated
	return true, nil
}

// installJournalIdentityMatches rechecks the complete immutable nonsecret
// request identity of an exact create retry. Any changed selector, actor,
// public key, mode or artifact pin is a conflict, never a silent reuse of an
// earlier operation.
func installJournalIdentityMatches(row probeInstallOperationModel, want domain.ProbeInstallIdentity) bool {
	probe := domain.ProbeInstallIdentity{
		OperationID:          row.OperationID,
		Host:                 domain.ProbeSSHHost{Address: row.SSHAddress, User: row.SSHUser},
		HostKeyFingerprint:   row.HostKeyFingerprint,
		ClientKeyFingerprint: row.ClientKeyFingerprint,
		RequestedBy:          row.RequestedBy,
		LayoutVersion:        row.LayoutVersion,
		Mode:                 row.Mode,
	}
	switch row.Mode {
	case domain.ProbeInstallSystemd:
		if row.BinaryVersion == nil || row.BinaryArchitecture == nil || row.BinarySHA256 == nil || row.BinarySize == nil {
			return false
		}
		probe.Binary = domain.ProbeBinaryPin{
			Version: *row.BinaryVersion, Architecture: *row.BinaryArchitecture,
			SHA256: *row.BinarySHA256, Size: *row.BinarySize,
		}
	case domain.ProbeInstallDocker:
		if row.ImageRepository == nil || row.ImageVersion == nil || row.ImageArchitecture == nil ||
			row.ImageIndexSHA256 == nil || row.ImageIndexSize == nil || row.ImageManifestSHA256 == nil ||
			row.ImageConfigSHA256 == nil || row.ImageLayerCount == nil {
			return false
		}
		probe.Image = domain.ProbeImageSelection{
			Pin: domain.ProbeImagePin{
				Repository: *row.ImageRepository, Version: *row.ImageVersion,
				Architecture: *row.ImageArchitecture, IndexSHA256: *row.ImageIndexSHA256, IndexSize: *row.ImageIndexSize,
			},
			ManifestSHA256: *row.ImageManifestSHA256,
			ConfigSHA256:   *row.ImageConfigSHA256,
			LayerCount:     *row.ImageLayerCount,
		}
	default:
		return false
	}
	return probe == want
}

// ProbeInstallJournalStore persists the M8 durable installation intent and
// resource journal on either hub engine. One implementation serves MariaDB and
// SQLite with identical semantics: every mutation is transactional, serializes
// on the stored operation row, validates the complete stored graph before any
// expiry reconciliation, samples the DB UTC clock after the lock and fences the
// current stored owner, state and deadline in the same transaction. There is
// no renewal, takeover, resume, adoption, rearm, delete or cleanup API and no
// installed/success state anywhere in the journal.
type ProbeInstallJournalStore struct{ db *bun.DB }

// NewProbeInstallJournalStore creates a Bun-backed installation journal store.
func NewProbeInstallJournalStore(db *bun.DB) *ProbeInstallJournalStore {
	return &ProbeInstallJournalStore{db: db}
}

var _ ports.ProbeInstallOperationRepository = (*ProbeInstallJournalStore)(nil)
var _ ports.ProbeInstallResourceJournal = (*ProbeInstallJournalStore)(nil)

// CreateInstall commits one queued operation plus its complete canonical
// resource graph atomically before acknowledgement. Fresh creation is proven
// by this transaction attempt's admission UUID read back under the row lock,
// never by affected-row counts. An exact identity retry returns the original
// owner, deadline and history with created=false and never replenishes
// authority, whatever owner the retrying process presents; a changed identity
// or artifact tuple conflicts.
func (s *ProbeInstallJournalStore) CreateInstall(ctx context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
	if s == nil || s.db == nil || !request.Valid() || !domain.ValidProbeOperationID(ownerID) {
		return nil, false, fmt.Errorf("invalid probe install create request: %w", domain.ErrValidation)
	}
	specs := request.Resources()
	var out *domain.ProbeInstallOperation
	for attempt := 0; ; attempt++ {
		created := false
		err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
			out, created = nil, false
			// One admission UUID per transaction attempt. If an earlier attempt
			// committed without acknowledgement, the stored UUID can never match
			// this attempt's and the caller receives the original receipt with
			// created=false — never a fresh grant after a failed commit.
			admission, err := uuid.NewRandom()
			if err != nil {
				return err
			}
			existing := new(probeInstallOperationModel)
			q := tx.NewSelect().Model(existing).Where("operation_id = ?", request.OperationID)
			if tx.Dialect().Name() == dialect.MySQL {
				q = q.For("UPDATE")
			}
			switch err := q.Scan(ctx); {
			case err == nil:
				resources, rerr := readProbeInstallResources(ctx, tx, request.OperationID)
				if rerr != nil {
					return rerr
				}
				graph := probeInstallGraph{operation: *existing, resources: resources}
				op, verr := graph.receipt()
				if verr != nil {
					return verr
				}
				if !installJournalIdentityMatches(*existing, request) {
					return ports.ErrConflict
				}
				// Exact identity retry: the stored receipt stands untouched,
				// including its owner, deadline and resource history.
				out = &op
				return nil
			case errors.Is(err, sql.ErrNoRows):
				// Fresh operation; insert below.
			default:
				return err
			}
			now, err := replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			now = installJournalTime(now)
			deadline := now.Add(domain.ProbeInstallLifetime)
			row := probeInstallOperationModel{
				OperationID:          request.OperationID,
				SSHAddress:           request.Host.Address,
				SSHUser:              request.Host.User,
				HostKeyFingerprint:   request.HostKeyFingerprint,
				ClientKeyFingerprint: request.ClientKeyFingerprint,
				RequestedBy:          request.RequestedBy,
				LayoutVersion:        request.LayoutVersion,
				Mode:                 request.Mode,
				OwnerID:              ownerID,
				AdmissionID:          admission.String(),
				State:                domain.ProbeInstallQueued,
				CreatedAt:            now,
				UpdatedAt:            now,
				DeadlineAt:           deadline,
			}
			switch request.Mode {
			case domain.ProbeInstallSystemd:
				version, arch, sha, size := request.Binary.Version, request.Binary.Architecture, request.Binary.SHA256, request.Binary.Size
				row.BinaryVersion, row.BinaryArchitecture, row.BinarySHA256, row.BinarySize = &version, &arch, &sha, &size
			case domain.ProbeInstallDocker:
				repository, version, arch := request.Image.Pin.Repository, request.Image.Pin.Version, request.Image.Pin.Architecture
				indexSHA, indexSize := request.Image.Pin.IndexSHA256, request.Image.Pin.IndexSize
				manifest, config, layers := request.Image.ManifestSHA256, request.Image.ConfigSHA256, request.Image.LayerCount
				row.ImageRepository, row.ImageVersion, row.ImageArchitecture = &repository, &version, &arch
				row.ImageIndexSHA256, row.ImageIndexSize = &indexSHA, &indexSize
				row.ImageManifestSHA256, row.ImageConfigSHA256, row.ImageLayerCount = &manifest, &config, &layers
			default:
				return fmt.Errorf("invalid probe install mode: %w", domain.ErrValidation)
			}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				if installJournalRaced(err) {
					return errInstallJournalRaced
				}
				return err
			}
			// The entire canonical resource graph commits with its operation.
			resourceRows := make([]probeInstallResourceModel, 0, len(specs))
			for _, spec := range specs {
				resourceRows = append(resourceRows, probeInstallResourceModel{
					OperationID: request.OperationID,
					Ordinal:     spec.Ordinal,
					Kind:        spec.Kind,
					Reference:   spec.Reference,
					Owned:       spec.Owned,
					State:       domain.ProbeInstallResourcePlanned,
				})
			}
			if _, err := tx.NewInsert().Model(&resourceRows).Exec(ctx); err != nil {
				if installJournalRaced(err) {
					return errInstallJournalRaced
				}
				return err
			}
			// Prove fresh creation under the row lock: only this attempt's
			// stored admission UUID may answer created=true, and it is
			// validated like every other stored identity field.
			locked, err := lockProbeInstallRow(ctx, tx, request.OperationID)
			if err != nil {
				return err
			}
			committed, err := readProbeInstallResources(ctx, tx, request.OperationID)
			if err != nil {
				return err
			}
			graph := probeInstallGraph{operation: locked, resources: committed}
			op, verr := graph.receipt()
			if verr != nil {
				return verr
			}
			if locked.AdmissionID != admission.String() {
				return errInstallJournalRaced
			}
			out, created = &op, true
			return nil
		})
		if errors.Is(err, errInstallJournalRaced) && attempt+1 < installJournalAttemptLimit {
			continue
		}
		if errors.Is(err, errInstallJournalRaced) {
			// Exhausted contention proves no stored identity mismatch. Keep
			// retries bounded, but report unavailability rather than inventing
			// a permanent conflict (SQLite busy can leave no row at all).
			return nil, false, domain.ErrInternal
		}
		if err != nil {
			return nil, false, err
		}
		return out, created, nil
	}
}

// ObserveInstall returns the durable operation and its complete resource
// graph. A hard-expired nonterminal operation is reconciled to honest cold
// interruption first, atomically and after the complete graph has been
// validated. Reads never create, dispatch, renew or grant anything.
func (s *ProbeInstallJournalStore) ObserveInstall(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid probe install operation id: %w", domain.ErrValidation)
	}
	var out *domain.ProbeInstallOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		graph, err := loadProbeInstallGraph(ctx, tx, operationID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := graph.reconcileExpiry(ctx, tx, now); err != nil {
			return err
		}
		op, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BeginInstall is the single queued-to-running transition and the dispatch
// grant for exactly one executor. It requires the exact stored owner, queued
// state and an unexpired deadline; a second begin can never dispatch again and
// nothing here ever renews, retakes or rebinds the owner.
func (s *ProbeInstallJournalStore) BeginInstall(ctx context.Context, operationID, ownerID string) (*domain.ProbeInstallOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) {
		return nil, fmt.Errorf("invalid probe install begin request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeInstallOperation
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeInstallGraph(ctx, tx, operationID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		reconciled, err := graph.reconcileExpiry(ctx, tx, now)
		if err != nil {
			return err
		}
		commit.reconciled = reconciled
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		if current.OwnerID != ownerID || current.State != domain.ProbeInstallQueued || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		updated := installJournalStamp(graph.operation.UpdatedAt, now)
		if _, err := tx.NewUpdate().Table("probe_install_operations").
			Set("state = ?", domain.ProbeInstallRunning).
			Set("error_code = ?", nil).
			Set("updated_at = ?", updated).
			Where("operation_id = ?", operationID).Exec(ctx); err != nil {
			return err
		}
		graph.operation.State = domain.ProbeInstallRunning
		graph.operation.ErrorCode = nil
		graph.operation.UpdatedAt = updated
		op, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	if commit.pending != nil {
		return nil, commit.pending
	}
	return out, nil
}

// RequestInstallCancellation persists cancellation intent on any replica; it
// never claims remote execution stopped. Queued work has no begin grant ever
// existed and becomes terminal canceled at once; active work becomes
// cancel_requested intent and only a joined owner finish may later record
// terminal canceled. Terminal receipts are returned unchanged.
func (s *ProbeInstallJournalStore) RequestInstallCancellation(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid probe install cancel request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeInstallOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		graph, err := loadProbeInstallGraph(ctx, tx, operationID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := graph.reconcileExpiry(ctx, tx, now); err != nil {
			return err
		}
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		if current.Terminal() {
			out = &current
			return nil
		}
		if current.State == domain.ProbeInstallCancelRequested {
			// Cancellation intent is already recorded; repeat requests read the
			// same history without mutating it.
			out = &current
			return nil
		}
		updated := installJournalStamp(graph.operation.UpdatedAt, now)
		state := domain.ProbeInstallCancelRequested
		var code any
		if current.State == domain.ProbeInstallQueued {
			// No begin grant ever existed: queued work is terminally canceled
			// at once, with the fixed canceled outcome code.
			state = domain.ProbeInstallCanceled
			code = "canceled"
		}
		if _, err := tx.NewUpdate().Table("probe_install_operations").
			Set("state = ?", state).
			Set("error_code = ?", code).
			Set("updated_at = ?", updated).
			Where("operation_id = ?", operationID).Exec(ctx); err != nil {
			return err
		}
		graph.operation.State = state
		if code == nil {
			graph.operation.ErrorCode = nil
		} else {
			value := code.(string)
			graph.operation.ErrorCode = &value
		}
		graph.operation.UpdatedAt = updated
		op, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FinishInstall records exactly one owner outcome: failed, canceled or the
// owner's own interruption. There is no success/installed outcome. Finish
// requires the joined owner (an admitted executor): queued work has no joined
// executor and is finished only by the queued-cancel path. Failed and canceled
// refuse unresolved in-flight/unknown actions; owner interruption converts
// in-flight actions to unknown/owner_interrupted atomically. Exact terminal
// retries return immutable history and never mutate it; a late outcome cannot
// replace expiry uncertainty.
func (s *ProbeInstallJournalStore) FinishInstall(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) ||
		!domain.ValidProbeOperationID(ownerID) || !outcome.Valid() {
		return nil, fmt.Errorf("invalid probe install finish request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeInstallOperation
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeInstallGraph(ctx, tx, operationID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		reconciled, err := graph.reconcileExpiry(ctx, tx, now)
		if err != nil {
			return err
		}
		commit.reconciled = reconciled
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		if current.Terminal() {
			if current.OwnerID == ownerID && current.State == outcome.State && current.ErrorCode == outcome.ErrorCode {
				out = &current
				return nil
			}
			return commit.fail(ports.ErrConflict)
		}
		if current.OwnerID != ownerID || !installJournalJoined(current.State) || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		if outcome.State == domain.ProbeInstallCanceled && current.State != domain.ProbeInstallCancelRequested {
			// Cancellation intent must be durable before its joined outcome.
			return commit.fail(ports.ErrConflict)
		}
		updated := installJournalStamp(graph.operation.UpdatedAt, now)
		switch outcome.State {
		case domain.ProbeInstallFailed, domain.ProbeInstallCanceled:
			for _, resource := range current.Resources {
				if resource.State == domain.ProbeInstallResourceInFlight || resource.State == domain.ProbeInstallResourceUnknown {
					// Failure and cancellation refuse unresolved actions.
					return commit.fail(ports.ErrConflict)
				}
			}
		case domain.ProbeInstallInterrupted:
			// Owner interruption marks every in-flight action unknown with the
			// same code, atomically, so the graph stays self-consistent.
			for i := range graph.resources {
				resource := &graph.resources[i]
				if resource.State != domain.ProbeInstallResourceInFlight {
					continue
				}
				started := installJournalTime(*resource.StartedAt)
				finished := installJournalFinishStamp(started, now)
				code := outcome.ErrorCode
				if _, err := tx.NewUpdate().Table("probe_install_resources").
					Set("state = ?", domain.ProbeInstallResourceUnknown).
					Set("error_code = ?", code).
					Set("finished_at = ?", finished).
					Where("operation_id = ? AND ordinal = ?", operationID, resource.Ordinal).Exec(ctx); err != nil {
					return err
				}
				resource.State = domain.ProbeInstallResourceUnknown
				resource.ErrorCode = &code
				resource.FinishedAt = &finished
			}
		default:
			return fmt.Errorf("invalid probe install outcome state: %w", domain.ErrValidation)
		}
		code := outcome.ErrorCode
		if _, err := tx.NewUpdate().Table("probe_install_operations").
			Set("state = ?", outcome.State).
			Set("error_code = ?", code).
			Set("updated_at = ?", updated).
			Where("operation_id = ?", operationID).Exec(ctx); err != nil {
			return err
		}
		graph.operation.State = outcome.State
		graph.operation.ErrorCode = &code
		graph.operation.UpdatedAt = updated
		op, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	if commit.pending != nil {
		return nil, commit.pending
	}
	return out, nil
}

// ReserveInstallResource binds one canonical resource to one canonical attempt
// UUID under the stored owner and commits in_flight before created=true is
// ever returned. A fresh grant requires the admitted owner running on a live
// deadline with every predecessor already committed applied; queue position
// never counts. Exact retries return their existing receipt with created=false
// — including historical outcomes after expiry — and never authorize another
// external attempt. A different attempt ID, a duplicate attempt UUID anywhere
// in the journal, a wrong owner or a non-running operation conflicts.
func (s *ProbeInstallJournalStore) ReserveInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) ||
		!installJournalKind(kind) || !domain.ValidProbeOperationID(attemptID) {
		return nil, false, fmt.Errorf("invalid probe install reserve request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeInstallOperation
	granted := false
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out, granted = nil, false
		commit.begin()
		graph, err := loadProbeInstallGraph(ctx, tx, operationID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		reconciled, err := graph.reconcileExpiry(ctx, tx, now)
		if err != nil {
			return err
		}
		commit.reconciled = reconciled
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		index, ok := graph.resourceIndex(kind)
		if !ok {
			return commit.fail(ports.ErrNotFound)
		}
		resource := current.Resources[index]
		if resource.State != domain.ProbeInstallResourcePlanned {
			// Immutable attempt receipt: an exact reservation retry reads its
			// history and never replays authority, whatever the outcome was.
			if current.OwnerID == ownerID && resource.AttemptID == attemptID {
				out = &current
				return nil
			}
			return commit.fail(ports.ErrConflict)
		}
		if current.OwnerID != ownerID || current.State != domain.ProbeInstallRunning || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		for i := 0; i < index; i++ {
			if current.Resources[i].State != domain.ProbeInstallResourceApplied {
				// Committed applied predecessors are required; a queue slot is
				// not authority.
				return commit.fail(ports.ErrConflict)
			}
		}
		var taken int
		if err := tx.NewSelect().Table("probe_install_resources").
			ColumnExpr("COUNT(*)").Where("attempt_id = ?", attemptID).Scan(ctx, &taken); err != nil {
			return err
		}
		if taken > 0 {
			// Attempt UUIDs are globally unique across the whole journal.
			return commit.fail(ports.ErrConflict)
		}
		var previousFinish time.Time
		if index > 0 {
			previousFinish = current.Resources[index-1].FinishedAt
		}
		started := installJournalStartStamp(now, current.CreatedAt, previousFinish)
		updated := installJournalStamp(graph.operation.UpdatedAt, now)
		if _, err := tx.NewUpdate().Table("probe_install_resources").
			Set("state = ?", domain.ProbeInstallResourceInFlight).
			Set("attempt_id = ?", attemptID).
			Set("error_code = ?", nil).
			Set("started_at = ?", started).
			Set("finished_at = ?", nil).
			Where("operation_id = ? AND ordinal = ?", operationID, resource.Spec.Ordinal).Exec(ctx); err != nil {
			if installJournalRaced(err) {
				return commit.fail(ports.ErrConflict)
			}
			return err
		}
		if _, err := tx.NewUpdate().Table("probe_install_operations").
			Set("updated_at = ?", updated).
			Where("operation_id = ?", operationID).Exec(ctx); err != nil {
			return err
		}
		graph.resources[index].State = domain.ProbeInstallResourceInFlight
		graph.resources[index].AttemptID = &attemptID
		graph.resources[index].ErrorCode = nil
		graph.resources[index].StartedAt = &started
		graph.resources[index].FinishedAt = nil
		graph.operation.UpdatedAt = updated
		op, err := graph.receipt()
		if err != nil {
			return err
		}
		out, granted = &op, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if commit.pending != nil {
		return nil, false, commit.pending
	}
	// Only the freshly committed in-flight intent is a grant; every exact
	// retry receipt — whatever its state — authorizes nothing.
	return out, granted, nil
}

// RecordInstallResource reports one joined resource action for an exact stored
// attempt under the stored owner. Applied/not_started/unknown reports are
// immutable history: an exact completed-outcome retry reads that history
// without mutation and a different or late outcome conflicts. A late result
// never replaces expiry uncertainty. The last applied report moves running to
// nonterminal resources_ready; cancellation intent stays cancel_requested and
// an unknown report atomically interrupts the operation so no dependent work
// can ever start afterwards.
func (s *ProbeInstallJournalStore) RecordInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) ||
		!installJournalKind(kind) || !domain.ValidProbeOperationID(attemptID) || !outcome.Valid() {
		return nil, fmt.Errorf("invalid probe install record request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeInstallOperation
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeInstallGraph(ctx, tx, operationID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		reconciled, err := graph.reconcileExpiry(ctx, tx, now)
		if err != nil {
			return err
		}
		commit.reconciled = reconciled
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		index, ok := graph.resourceIndex(kind)
		if !ok {
			return commit.fail(ports.ErrNotFound)
		}
		resource := current.Resources[index]
		if current.OwnerID != ownerID {
			return commit.fail(ports.ErrConflict)
		}
		if resource.State == domain.ProbeInstallResourcePlanned || resource.AttemptID != attemptID {
			// Record validates the exact stored owner, resource and attempt.
			return commit.fail(ports.ErrConflict)
		}
		if resource.State != domain.ProbeInstallResourceInFlight {
			// Completed receipt: exact outcome retries read immutable history.
			if resource.State == outcome.State && resource.ErrorCode == outcome.ErrorCode {
				out = &current
				return nil
			}
			return commit.fail(ports.ErrConflict)
		}
		if current.State != domain.ProbeInstallRunning && current.State != domain.ProbeInstallCancelRequested {
			return commit.fail(ports.ErrConflict)
		}
		if !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		started := installJournalTime(resource.StartedAt)
		finished := installJournalFinishStamp(started, now)
		updated := installJournalStamp(graph.operation.UpdatedAt, now)
		// Applied outcomes carry no error code at all: an empty code is stored
		// as NULL so the state/error allowlist never sees a blank value.
		var resourceCode any
		if outcome.ErrorCode != "" {
			resourceCode = outcome.ErrorCode
		}
		if _, err := tx.NewUpdate().Table("probe_install_resources").
			Set("state = ?", outcome.State).
			Set("error_code = ?", resourceCode).
			Set("finished_at = ?", finished).
			Where("operation_id = ? AND ordinal = ?", operationID, resource.Spec.Ordinal).Exec(ctx); err != nil {
			return err
		}
		graph.resources[index].State = outcome.State
		if resourceCode == nil {
			graph.resources[index].ErrorCode = nil
		} else {
			value := outcome.ErrorCode
			graph.resources[index].ErrorCode = &value
		}
		graph.resources[index].FinishedAt = &finished
		opState := current.State
		opCode := any(nil)
		switch outcome.State {
		case domain.ProbeInstallResourceApplied:
			// The last applied report moves running to nonterminal
			// resources_ready; cancellation intent stays cancel_requested even
			// if every resource applies.
			allApplied := true
			for i := range graph.resources {
				if graph.resources[i].State != domain.ProbeInstallResourceApplied {
					allApplied = false
					break
				}
			}
			if allApplied && current.State == domain.ProbeInstallRunning {
				opState = domain.ProbeInstallResourcesReady
			}
		case domain.ProbeInstallResourceNotStarted:
			// The executor genuinely never invoked the mutation; the operation
			// keeps running so the owner can still finish it honestly.
		case domain.ProbeInstallResourceUnknown:
			// Unknown outcome atomically interrupts the operation and preserves
			// the resource uncertainty. No dependent work can start afterwards.
			opState = domain.ProbeInstallInterrupted
			opCode = outcome.ErrorCode
		}
		if opCode == nil {
			if _, err := tx.NewUpdate().Table("probe_install_operations").
				Set("state = ?", opState).
				Set("error_code = ?", nil).
				Set("updated_at = ?", updated).
				Where("operation_id = ?", operationID).Exec(ctx); err != nil {
				return err
			}
			graph.operation.State = opState
			graph.operation.ErrorCode = nil
		} else {
			if _, err := tx.NewUpdate().Table("probe_install_operations").
				Set("state = ?", opState).
				Set("error_code = ?", opCode).
				Set("updated_at = ?", updated).
				Where("operation_id = ?", operationID).Exec(ctx); err != nil {
				return err
			}
			code := outcome.ErrorCode
			graph.operation.State = opState
			graph.operation.ErrorCode = &code
		}
		graph.operation.UpdatedAt = updated
		op, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	if commit.pending != nil {
		return nil, commit.pending
	}
	return out, nil
}
