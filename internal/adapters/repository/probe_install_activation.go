package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeInstallActivationModel stores one durable activation: the frozen nonsecret
// request (the install identity itself stays in probe_install_operations and is
// read through the shared authority row), the immutable owner, the fixed
// lifecycle fields and the public probe_init facts. The one-time enrollment
// token, keys and raw remote output have no column by construction.
type probeInstallActivationModel struct {
	bun.BaseModel          `bun:"table:probe_install_activations"`
	OperationID            string    `bun:"operation_id,pk"`
	ProbeKey               string    `bun:"probe_key"`
	Name                   string    `bun:"name"`
	Location               string    `bun:"location"`
	ListenPort             int       `bun:"listen_port"`
	ClientKeyFingerprint   string    `bun:"client_key_fingerprint"`
	RequestedBy            int64     `bun:"requested_by"`
	OwnerID                string    `bun:"owner_id"`
	State                  string    `bun:"state"`
	ErrorCode              *string   `bun:"error_code"`
	ProbeID                *string   `bun:"probe_id"`
	StreamID               *string   `bun:"stream_id"`
	CertificateFingerprint *string   `bun:"certificate_fingerprint"`
	CreatedAt              time.Time `bun:"created_at"`
	UpdatedAt              time.Time `bun:"updated_at"`
	DeadlineAt             time.Time `bun:"deadline_at"`
}

// probeInstallActivationStepModel stores one immutable canonical step intent and
// at most one attempt receipt. Planned rows carry no attempt residue.
type probeInstallActivationStepModel struct {
	bun.BaseModel `bun:"table:probe_install_activation_steps"`
	OperationID   string     `bun:"operation_id"`
	Kind          string     `bun:"kind"`
	Ordinal       int        `bun:"ordinal"`
	Reference     string     `bun:"reference"`
	Owned         bool       `bun:"owned"`
	State         string     `bun:"state"`
	AttemptID     *string    `bun:"attempt_id"`
	ErrorCode     *string    `bun:"error_code"`
	StartedAt     *time.Time `bun:"started_at"`
	FinishedAt    *time.Time `bun:"finished_at"`
}

// probeActivationAdmissionModel is one consumed probe_init or service_start
// attempt. It mirrors hostActionAdmissionModel against the activation steps.
type probeActivationAdmissionModel struct {
	bun.BaseModel  `bun:"table:probe_activation_admissions"`
	OperationID    string    `bun:"operation_id,pk"`
	Kind           string    `bun:"kind,pk"`
	OwnerID        string    `bun:"owner_id"`
	AttemptID      string    `bun:"attempt_id"`
	AuthorityKeyID string    `bun:"authority_key_id"`
	NonceHex       string    `bun:"nonce_hex"`
	MarkerSHA256   string    `bun:"marker_sha256"`
	AdmittedAt     time.Time `bun:"admitted_at"`
}

// step converts one stored step row through the install journal's resource
// converter, so both fail closed on the same attempt residue.
func (m probeInstallActivationStepModel) step() (domain.ProbeInstallResource, error) {
	return probeInstallResourceModel{
		OperationID: m.OperationID, Ordinal: m.Ordinal, Kind: m.Kind, Reference: m.Reference, Owned: m.Owned,
		State: m.State, AttemptID: m.AttemptID, ErrorCode: m.ErrorCode, StartedAt: m.StartedAt, FinishedAt: m.FinishedAt,
	}.resource()
}

// probeInstallActivationGraph is the locked shared install operation (graph),
// the activation row and its complete canonical step rows. It is only built
// from a current read taken under the operation row lock and is validated as a
// whole before it is used.
type probeInstallActivationGraph struct {
	install    probeInstallGraph
	activation probeInstallActivationModel
	steps      []probeInstallActivationStepModel
}

// probeActivationTerminal reports whether the stored activation may no longer
// change outcome. Active is the one success and is as immutable as a failure.
func probeActivationTerminal(state string) bool {
	switch state {
	case domain.ProbeActivationActive, domain.ProbeActivationFailed, domain.ProbeActivationCanceled, domain.ProbeActivationInterrupted:
		return true
	}
	return false
}

// probeActivationJoined reports whether an executor has been admitted (Begin
// granted). Only a joined owner may finish work: a queued activation has no
// executor and is finished only by the queued-cancel path.
func probeActivationJoined(state string) bool {
	return state == domain.ProbeActivationRunning || state == domain.ProbeActivationCancelRequested
}

// probeActivationKind accepts only the frozen four activation step kinds.
func probeActivationKind(kind string) bool {
	switch kind {
	case domain.ProbeActivationProbeInit, domain.ProbeActivationRegistration,
		domain.ProbeActivationServiceStart, domain.ProbeActivationEnrollment:
		return true
	}
	return false
}

// receipt assembles the complete stored graph and fails closed with
// domain.ErrValidation on corruption: the install graph, the activation row,
// the steps, the facts and every timestamp must satisfy the frozen domain
// contract, or the row is refused instead of being served or repaired.
func (g probeInstallActivationGraph) receipt() (domain.ProbeActivation, error) {
	install, err := g.install.receipt()
	if err != nil {
		return domain.ProbeActivation{}, err
	}
	m := g.activation
	out := domain.ProbeActivation{
		Identity: domain.ProbeActivationIdentity{
			Install: install.Identity, ProbeKey: m.ProbeKey, Name: m.Name, Location: m.Location,
			ListenPort: m.ListenPort, ClientKeyFingerprint: m.ClientKeyFingerprint, RequestedBy: m.RequestedBy,
		},
		OwnerID:    m.OwnerID,
		State:      m.State,
		CreatedAt:  m.CreatedAt.UTC(),
		UpdatedAt:  m.UpdatedAt.UTC(),
		DeadlineAt: m.DeadlineAt.UTC(),
	}
	if m.ErrorCode != nil {
		if *m.ErrorCode == "" {
			return domain.ProbeActivation{}, fmt.Errorf("empty activation error code on %s: %w", m.OperationID, domain.ErrValidation)
		}
		out.ErrorCode = *m.ErrorCode
	}
	if m.ProbeID != nil || m.StreamID != nil || m.CertificateFingerprint != nil {
		if m.ProbeID == nil || m.StreamID == nil || m.CertificateFingerprint == nil {
			return domain.ProbeActivation{}, fmt.Errorf("partial activation facts on %s: %w", m.OperationID, domain.ErrValidation)
		}
		out.Facts = domain.ProbeActivationFacts{ProbeID: *m.ProbeID, StreamID: *m.StreamID, CertificateFingerprint: *m.CertificateFingerprint}
	}
	out.Steps = make([]domain.ProbeInstallResource, 0, len(g.steps))
	for _, row := range g.steps {
		step, err := row.step()
		if err != nil {
			return domain.ProbeActivation{}, err
		}
		out.Steps = append(out.Steps, step)
	}
	if !out.Valid() {
		return domain.ProbeActivation{}, fmt.Errorf("corrupt probe install activation %s: %w", m.OperationID, domain.ErrValidation)
	}
	return out, nil
}

// stepIndex locates the one canonical step of the given kind.
func (g probeInstallActivationGraph) stepIndex(kind string) (int, bool) {
	for i := range g.steps {
		if g.steps[i].Kind == kind {
			return i, true
		}
	}
	return 0, false
}

// readProbeActivationRows reads the activation row and its complete canonical
// step rows (ordinal order) with current locking reads. found is false when no
// activation exists for the operation.
func readProbeActivationRows(ctx context.Context, tx bun.Tx, operationID string) (probeInstallActivationModel, []probeInstallActivationStepModel, bool, error) {
	row := new(probeInstallActivationModel)
	q := tx.NewSelect().Model(row).Where("operation_id = ?", operationID)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return probeInstallActivationModel{}, nil, false, nil
		}
		return probeInstallActivationModel{}, nil, false, err
	}
	var steps []probeInstallActivationStepModel
	sq := tx.NewSelect().Model(&steps).Where("operation_id = ?", operationID).Order("ordinal ASC")
	if tx.Dialect().Name() == dialect.MySQL {
		sq = sq.For("UPDATE")
	}
	if err := sq.Scan(ctx); err != nil {
		return probeInstallActivationModel{}, nil, false, err
	}
	return *row, steps, true, nil
}

// loadProbeInstallActivationGraph locks the shared install operation row first
// (operation-first locking, exactly like the install journal and the host-action
// admissions), reads the complete stored graph and validates it whole before any
// expiry reconciliation or transition is considered. A missing install or
// activation is ports.ErrNotFound; corruption is domain.ErrValidation.
func loadProbeInstallActivationGraph(ctx context.Context, tx bun.Tx, operationID string) (probeInstallActivationGraph, error) {
	install, err := loadProbeInstallGraph(ctx, tx, operationID)
	if err != nil {
		return probeInstallActivationGraph{}, err
	}
	row, steps, found, err := readProbeActivationRows(ctx, tx, operationID)
	if err != nil {
		return probeInstallActivationGraph{}, err
	}
	if !found {
		return probeInstallActivationGraph{}, ports.ErrNotFound
	}
	graph := probeInstallActivationGraph{install: install, activation: row, steps: steps}
	if _, err := graph.receipt(); err != nil {
		return probeInstallActivationGraph{}, err
	}
	return graph, nil
}

// setLifecycle updates the lifecycle columns of the locked activation row and
// mirrors them into the in-memory graph so the receipt is rebuilt from exactly
// what was written.
func (g *probeInstallActivationGraph) setLifecycle(ctx context.Context, tx bun.Tx, state string, code *string, updated time.Time) error {
	if _, err := tx.NewUpdate().Table("probe_install_activations").
		Set("state = ?", state).
		Set("error_code = ?", code).
		Set("updated_at = ?", updated).
		Where("operation_id = ?", g.activation.OperationID).Exec(ctx); err != nil {
		return err
	}
	g.activation.State = state
	g.activation.ErrorCode = code
	g.activation.UpdatedAt = updated
	return nil
}

// reconcileExpiry records honest cold-expiry state: a hard-expired nonterminal
// activation becomes interrupted/deadline_expired and every in-flight step
// becomes unknown/deadline_expired atomically. Historical applied and
// not-started steps stay untouched and planned steps are never fabricated as
// executed. The caller must commit this even when it then refuses the requested
// transition.
func (g *probeInstallActivationGraph) reconcileExpiry(ctx context.Context, tx bun.Tx, now time.Time) (bool, error) {
	a := &g.activation
	if probeActivationTerminal(a.State) || installJournalTime(now).Before(installJournalTime(a.DeadlineAt)) {
		return false, nil
	}
	updated := installJournalStamp(a.UpdatedAt, now)
	for i := range g.steps {
		step := &g.steps[i]
		if step.State != domain.ProbeInstallResourceInFlight {
			continue
		}
		started := installJournalTime(*step.StartedAt)
		finished := installJournalFinishStamp(started, now)
		code := "deadline_expired"
		if _, err := tx.NewUpdate().Table("probe_install_activation_steps").
			Set("state = ?", domain.ProbeInstallResourceUnknown).
			Set("error_code = ?", code).
			Set("finished_at = ?", finished).
			Where("operation_id = ? AND kind = ?", a.OperationID, step.Kind).Exec(ctx); err != nil {
			return false, err
		}
		step.State = domain.ProbeInstallResourceUnknown
		step.ErrorCode = &code
		step.FinishedAt = &finished
	}
	code := "deadline_expired"
	if err := g.setLifecycle(ctx, tx, domain.ProbeActivationInterrupted, &code, updated); err != nil {
		return false, err
	}
	return true, nil
}

// ProbeInstallActivationStore persists the M8 durable probe activation and its
// step journal on either hub engine. It mirrors ProbeInstallJournalStore and
// ProbeHostActionAdmissionStore: every mutation is transactional, serializes on
// the shared install operation row first, validates the complete stored graph
// before any expiry reconciliation, samples the DB UTC clock after that lock and
// fences the current stored owner, state and deadline in the same transaction.
// There is no renewal, takeover, resume, adoption, rearm, delete or cleanup API.
//
// It is named ProbeInstallActivationStore because ProbeActivationStore already
// belongs to the unrelated probe configuration activation store in this package.
type ProbeInstallActivationStore struct{ db *bun.DB }

// NewProbeInstallActivationStore creates a Bun-backed activation store over an
// existing connection pool. It starts no work and owns no connection lifetime.
func NewProbeInstallActivationStore(db *bun.DB) *ProbeInstallActivationStore {
	return &ProbeInstallActivationStore{db: db}
}

var _ ports.ProbeActivationRepository = (*ProbeInstallActivationStore)(nil)
var _ ports.ProbeActivationStepJournal = (*ProbeInstallActivationStore)(nil)
var _ ports.ProbeActivationAdmissionRepository = (*ProbeInstallActivationStore)(nil)

// CreateActivation commits one queued activation plus its complete canonical
// step graph atomically before acknowledgement. The install operation must
// exist in state resources_ready (after its own hard-expiry reconciliation) with
// exactly identity.Install. Creation is serialized on the install operation row,
// so a successful insert in this transaction is itself the proof of freshness.
// An exact identity retry returns the original owner, deadline and history with
// created=false and never replenishes authority, whatever owner the retrying
// process presents (so an acknowledgement lost after commit can never produce a
// second fresh grant); a changed identity conflicts. A fresh activation also
// conflicts when any cleanup row exists for the operation, checked under the same
// operation lock.
func (s *ProbeInstallActivationStore) CreateActivation(ctx context.Context, identity domain.ProbeActivationIdentity, ownerID string) (*domain.ProbeActivation, bool, error) {
	if s == nil || s.db == nil || !identity.Valid() || !domain.ValidProbeOperationID(ownerID) {
		return nil, false, fmt.Errorf("invalid probe activation create request: %w", domain.ErrValidation)
	}
	specs := identity.Steps()
	operationID := identity.Install.OperationID
	var out *domain.ProbeActivation
	var commit installJournalCommit
	for attempt := 0; ; attempt++ {
		created := false
		err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
			out, created = nil, false
			commit.begin()
			install, err := loadProbeInstallGraph(ctx, tx, operationID)
			if err != nil {
				return err
			}
			existing, existingSteps, found, err := readProbeActivationRows(ctx, tx, operationID)
			if err != nil {
				return err
			}
			if found {
				graph := probeInstallActivationGraph{install: install, activation: existing, steps: existingSteps}
				stored, err := graph.receipt()
				if err != nil {
					return err
				}
				if stored.Identity != identity {
					return ports.ErrConflict
				}
				// Exact identity retry: the stored receipt stands untouched,
				// including its owner, deadline and step history.
				out = &stored
				return nil
			}
			// Once any cleanup row exists the install's host resources are being
			// (or were) removed on purpose, so a new activation can never be
			// admitted over them. CreateCleanup refuses nonterminal activations
			// under the same operation lock, so the two can never interleave.
			var cleanups int
			if err := tx.NewSelect().Table("probe_install_cleanups").
				ColumnExpr("COUNT(*)").Where("operation_id = ?", operationID).Scan(ctx, &cleanups); err != nil {
				return err
			}
			if cleanups > 0 {
				return ports.ErrConflict
			}
			now, err := replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			now = installJournalTime(now)
			reconciled, err := install.reconcileExpiry(ctx, tx, now)
			if err != nil {
				return err
			}
			commit.reconciled = reconciled
			operation, err := install.receipt()
			if err != nil {
				return err
			}
			if operation.Identity != identity.Install || operation.State != domain.ProbeInstallResourcesReady {
				return commit.fail(ports.ErrConflict)
			}
			row := probeInstallActivationModel{
				OperationID: operationID, ProbeKey: identity.ProbeKey, Name: identity.Name, Location: identity.Location,
				ListenPort: identity.ListenPort, ClientKeyFingerprint: identity.ClientKeyFingerprint, RequestedBy: identity.RequestedBy,
				OwnerID: ownerID, State: domain.ProbeActivationQueued,
				CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(domain.ProbeActivationLifetime),
			}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				if installJournalRaced(err) {
					return errInstallJournalRaced
				}
				return err
			}
			// The entire canonical step graph commits with its activation.
			stepRows := make([]probeInstallActivationStepModel, 0, len(specs))
			for _, spec := range specs {
				stepRows = append(stepRows, probeInstallActivationStepModel{
					OperationID: operationID, Kind: spec.Kind, Ordinal: spec.Ordinal, Reference: spec.Reference,
					Owned: spec.Owned, State: domain.ProbeInstallResourcePlanned,
				})
			}
			if _, err := tx.NewInsert().Model(&stepRows).Exec(ctx); err != nil {
				if installJournalRaced(err) {
					return errInstallJournalRaced
				}
				return err
			}
			// Read back under the same lock and compare: the receipt is built from
			// what the database actually holds, never from the request.
			locked, lockedSteps, found, err := readProbeActivationRows(ctx, tx, operationID)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("activation %s vanished inside its own create: %w", operationID, domain.ErrValidation)
			}
			graph := probeInstallActivationGraph{install: install, activation: locked, steps: lockedSteps}
			stored, err := graph.receipt()
			if err != nil {
				return err
			}
			if stored.Identity != identity || stored.OwnerID != ownerID || stored.State != domain.ProbeActivationQueued {
				return fmt.Errorf("activation %s read-back differs from its create: %w", operationID, domain.ErrValidation)
			}
			out, created = &stored, true
			return nil
		})
		if errors.Is(err, errInstallJournalRaced) && attempt+1 < installJournalAttemptLimit {
			continue
		}
		if errors.Is(err, errInstallJournalRaced) {
			// Exhausted contention proves no stored identity mismatch. Keep retries
			// bounded, but report unavailability rather than inventing a permanent
			// conflict.
			return nil, false, domain.ErrInternal
		}
		if err != nil {
			return nil, false, err
		}
		if commit.pending != nil {
			return nil, false, commit.pending
		}
		return out, created, nil
	}
}

// ObserveActivation returns the durable activation and its complete step graph.
// A hard-expired nonterminal activation is reconciled to honest cold
// interruption first, atomically and after the complete graph has been
// validated. Reads never create, dispatch, renew or grant anything.
func (s *ProbeInstallActivationStore) ObserveActivation(ctx context.Context, operationID string) (*domain.ProbeActivation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid probe activation operation id: %w", domain.ErrValidation)
	}
	var out *domain.ProbeActivation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		graph, err := loadProbeInstallActivationGraph(ctx, tx, operationID)
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
		out = &current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BeginActivation is the single queued-to-running transition and the dispatch
// grant for exactly one executor. It requires the exact stored owner, queued
// state and an unexpired deadline; a second begin can never dispatch again and
// nothing here ever renews, retakes or rebinds the owner.
func (s *ProbeInstallActivationStore) BeginActivation(ctx context.Context, operationID, ownerID string) (*domain.ProbeActivation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) {
		return nil, fmt.Errorf("invalid probe activation begin request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeActivation
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeInstallActivationGraph(ctx, tx, operationID)
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
		if current.OwnerID != ownerID || current.State != domain.ProbeActivationQueued || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		updated := installJournalStamp(graph.activation.UpdatedAt, now)
		if err := graph.setLifecycle(ctx, tx, domain.ProbeActivationRunning, nil, updated); err != nil {
			return err
		}
		begun, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &begun
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

// RequestActivationCancellation persists cancellation intent on any replica; it
// never claims remote execution stopped. Queued work has no begin grant ever
// existed and becomes terminal canceled at once; running work becomes
// cancel_requested intent and only a joined owner finish may later record
// terminal canceled. Terminal receipts (including active) are returned unchanged.
func (s *ProbeInstallActivationStore) RequestActivationCancellation(ctx context.Context, operationID string) (*domain.ProbeActivation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid probe activation cancel request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeActivation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		graph, err := loadProbeInstallActivationGraph(ctx, tx, operationID)
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
		if current.Terminal() || current.State == domain.ProbeActivationCancelRequested {
			// Terminal history is immutable and repeated intent reads the same
			// history without mutating it.
			out = &current
			return nil
		}
		updated := installJournalStamp(graph.activation.UpdatedAt, now)
		state := domain.ProbeActivationCancelRequested
		var code *string
		if current.State == domain.ProbeActivationQueued {
			// No begin grant ever existed: queued work is terminally canceled at
			// once, with the fixed canceled outcome code.
			state = domain.ProbeActivationCanceled
			canceled := "canceled"
			code = &canceled
		}
		if err := graph.setLifecycle(ctx, tx, state, code, updated); err != nil {
			return err
		}
		requested, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &requested
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FinishActivation records exactly one owner outcome: failed, canceled or the
// owner's own interruption, under the same rules as FinishInstall. There is no
// success outcome: active is reached only by recording the last applied step.
// Finish requires the joined owner (an admitted executor); failed and canceled
// refuse unresolved in-flight/unknown steps; owner interruption converts
// in-flight steps to unknown/owner_interrupted atomically. Exact terminal
// retries return immutable history and never mutate it; a late outcome cannot
// replace expiry uncertainty or an active activation.
func (s *ProbeInstallActivationStore) FinishActivation(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeActivation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) ||
		!domain.ValidProbeOperationID(ownerID) || !outcome.Valid() {
		return nil, fmt.Errorf("invalid probe activation finish request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeActivation
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeInstallActivationGraph(ctx, tx, operationID)
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
		if current.OwnerID != ownerID || !probeActivationJoined(current.State) || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		if outcome.State == domain.ProbeInstallCanceled && current.State != domain.ProbeActivationCancelRequested {
			// Cancellation intent must be durable before its joined outcome.
			return commit.fail(ports.ErrConflict)
		}
		updated := installJournalStamp(graph.activation.UpdatedAt, now)
		switch outcome.State {
		case domain.ProbeInstallFailed, domain.ProbeInstallCanceled:
			for _, step := range current.Steps {
				if step.State == domain.ProbeInstallResourceInFlight || step.State == domain.ProbeInstallResourceUnknown {
					// Failure and cancellation refuse unresolved actions.
					return commit.fail(ports.ErrConflict)
				}
			}
		case domain.ProbeInstallInterrupted:
			// Owner interruption marks every in-flight step unknown with the same
			// code, atomically, so the graph stays self-consistent.
			for i := range graph.steps {
				step := &graph.steps[i]
				if step.State != domain.ProbeInstallResourceInFlight {
					continue
				}
				started := installJournalTime(*step.StartedAt)
				finished := installJournalFinishStamp(started, now)
				code := outcome.ErrorCode
				if _, err := tx.NewUpdate().Table("probe_install_activation_steps").
					Set("state = ?", domain.ProbeInstallResourceUnknown).
					Set("error_code = ?", code).
					Set("finished_at = ?", finished).
					Where("operation_id = ? AND kind = ?", operationID, step.Kind).Exec(ctx); err != nil {
					return err
				}
				step.State = domain.ProbeInstallResourceUnknown
				step.ErrorCode = &code
				step.FinishedAt = &finished
			}
		default:
			return fmt.Errorf("invalid probe activation outcome state: %w", domain.ErrValidation)
		}
		code := outcome.ErrorCode
		if err := graph.setLifecycle(ctx, tx, outcome.State, &code, updated); err != nil {
			return err
		}
		finished, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &finished
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

// ReserveActivationStep binds one canonical step to one canonical attempt UUID
// under the stored owner and commits in_flight before created=true is ever
// returned. A fresh grant requires the admitted owner running on a live deadline
// with every predecessor already committed applied; queue position never
// counts. Exact retries return their existing receipt with created=false —
// including historical outcomes after expiry — and never authorize another
// external attempt. A different attempt ID, a duplicate attempt UUID anywhere in
// the activation journal, a wrong owner or a non-running activation conflicts.
func (s *ProbeInstallActivationStore) ReserveActivationStep(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeActivation, bool, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) ||
		!probeActivationKind(kind) || !domain.ValidProbeOperationID(attemptID) {
		return nil, false, fmt.Errorf("invalid probe activation reserve request: %w", domain.ErrValidation)
	}
	var out *domain.ProbeActivation
	granted := false
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out, granted = nil, false
		commit.begin()
		graph, err := loadProbeInstallActivationGraph(ctx, tx, operationID)
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
		index, ok := graph.stepIndex(kind)
		if !ok {
			return commit.fail(ports.ErrNotFound)
		}
		step := current.Steps[index]
		if step.State != domain.ProbeInstallResourcePlanned {
			// Immutable attempt receipt: an exact reservation retry reads its
			// history and never replays authority, whatever the outcome was.
			if current.OwnerID == ownerID && step.AttemptID == attemptID {
				out = &current
				return nil
			}
			return commit.fail(ports.ErrConflict)
		}
		if current.OwnerID != ownerID || current.State != domain.ProbeActivationRunning || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		for i := 0; i < index; i++ {
			if current.Steps[i].State != domain.ProbeInstallResourceApplied {
				// Committed applied predecessors are required; a queue slot is not
				// authority.
				return commit.fail(ports.ErrConflict)
			}
		}
		var taken int
		if err := tx.NewSelect().Table("probe_install_activation_steps").
			ColumnExpr("COUNT(*)").Where("attempt_id = ?", attemptID).Scan(ctx, &taken); err != nil {
			return err
		}
		if taken > 0 {
			// Attempt UUIDs are unique across the whole activation journal.
			return commit.fail(ports.ErrConflict)
		}
		var previousFinish time.Time
		if index > 0 {
			previousFinish = current.Steps[index-1].FinishedAt
		}
		started := installJournalStartStamp(now, current.CreatedAt, previousFinish)
		updated := installJournalStamp(graph.activation.UpdatedAt, now)
		if _, err := tx.NewUpdate().Table("probe_install_activation_steps").
			Set("state = ?", domain.ProbeInstallResourceInFlight).
			Set("attempt_id = ?", attemptID).
			Set("error_code = ?", nil).
			Set("started_at = ?", started).
			Set("finished_at = ?", nil).
			Where("operation_id = ? AND kind = ?", operationID, kind).Exec(ctx); err != nil {
			if installJournalRaced(err) {
				return commit.fail(ports.ErrConflict)
			}
			return err
		}
		if _, err := tx.NewUpdate().Table("probe_install_activations").
			Set("updated_at = ?", updated).
			Where("operation_id = ?", operationID).Exec(ctx); err != nil {
			return err
		}
		graph.steps[index].State = domain.ProbeInstallResourceInFlight
		graph.steps[index].AttemptID = &attemptID
		graph.steps[index].ErrorCode = nil
		graph.steps[index].StartedAt = &started
		graph.steps[index].FinishedAt = nil
		graph.activation.UpdatedAt = updated
		reserved, err := graph.receipt()
		if err != nil {
			return err
		}
		out, granted = &reserved, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if commit.pending != nil {
		return nil, false, commit.pending
	}
	// Only the freshly committed in-flight intent is a grant; every exact retry
	// receipt — whatever its state — authorizes nothing.
	return out, granted, nil
}

// RecordActivationStep reports one joined step action for an exact stored attempt
// under the stored owner. Applied/not_started/unknown reports are immutable
// history: an exact completed-outcome retry reads that history without mutation
// and a different or late outcome conflicts. A late result never replaces expiry
// uncertainty. Recording probe_init applied requires valid facts and stores them
// in the same transaction; every other kind or outcome carries zero facts. The
// last applied report makes the activation active, also when cancellation
// intent was recorded meanwhile: every step really happened, so the honest
// terminal state is active (the domain forbids cancel_requested with all steps
// applied). An unknown report atomically interrupts the activation so no
// dependent work can ever start afterwards.
func (s *ProbeInstallActivationStore) RecordActivationStep(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome, facts domain.ProbeActivationFacts) (*domain.ProbeActivation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) ||
		!probeActivationKind(kind) || !domain.ValidProbeOperationID(attemptID) || !outcome.Valid() {
		return nil, fmt.Errorf("invalid probe activation record request: %w", domain.ErrValidation)
	}
	carriesFacts := kind == domain.ProbeActivationProbeInit && outcome.State == domain.ProbeInstallResourceApplied
	if carriesFacts && !facts.Valid() {
		return nil, fmt.Errorf("probe_init applied requires valid activation facts: %w", domain.ErrValidation)
	}
	if !carriesFacts && facts != (domain.ProbeActivationFacts{}) {
		return nil, fmt.Errorf("only probe_init applied may carry activation facts: %w", domain.ErrValidation)
	}
	var out *domain.ProbeActivation
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeInstallActivationGraph(ctx, tx, operationID)
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
		index, ok := graph.stepIndex(kind)
		if !ok {
			return commit.fail(ports.ErrNotFound)
		}
		step := current.Steps[index]
		if current.OwnerID != ownerID {
			return commit.fail(ports.ErrConflict)
		}
		if step.State == domain.ProbeInstallResourcePlanned || step.AttemptID != attemptID {
			// Record validates the exact stored owner, step and attempt.
			return commit.fail(ports.ErrConflict)
		}
		if step.State != domain.ProbeInstallResourceInFlight {
			// Completed receipt: exact outcome retries read immutable history. A
			// retry that names different facts is a different claim.
			if step.State == outcome.State && step.ErrorCode == outcome.ErrorCode && (!carriesFacts || current.Facts == facts) {
				out = &current
				return nil
			}
			return commit.fail(ports.ErrConflict)
		}
		if current.State != domain.ProbeActivationRunning && current.State != domain.ProbeActivationCancelRequested {
			return commit.fail(ports.ErrConflict)
		}
		if !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		started := installJournalTime(step.StartedAt)
		finished := installJournalFinishStamp(started, now)
		updated := installJournalStamp(graph.activation.UpdatedAt, now)
		// Applied outcomes carry no error code at all: an empty code is stored as
		// NULL so the state/error allowlist never sees a blank value.
		var stepCode *string
		if outcome.ErrorCode != "" {
			code := outcome.ErrorCode
			stepCode = &code
		}
		if _, err := tx.NewUpdate().Table("probe_install_activation_steps").
			Set("state = ?", outcome.State).
			Set("error_code = ?", stepCode).
			Set("finished_at = ?", finished).
			Where("operation_id = ? AND kind = ?", operationID, kind).Exec(ctx); err != nil {
			return err
		}
		graph.steps[index].State = outcome.State
		graph.steps[index].ErrorCode = stepCode
		graph.steps[index].FinishedAt = &finished
		state := current.State
		var activationCode *string
		switch outcome.State {
		case domain.ProbeInstallResourceApplied:
			if carriesFacts {
				// The facts and the applied probe_init commit together, so an
				// applied init can never exist without them (and vice versa).
				if _, err := tx.NewUpdate().Table("probe_install_activations").
					Set("probe_id = ?", facts.ProbeID).
					Set("stream_id = ?", facts.StreamID).
					Set("certificate_fingerprint = ?", facts.CertificateFingerprint).
					Where("operation_id = ?", operationID).Exec(ctx); err != nil {
					return err
				}
				probeID, streamID, pin := facts.ProbeID, facts.StreamID, facts.CertificateFingerprint
				graph.activation.ProbeID, graph.activation.StreamID, graph.activation.CertificateFingerprint = &probeID, &streamID, &pin
			}
			allApplied := true
			for i := range graph.steps {
				if graph.steps[i].State != domain.ProbeInstallResourceApplied {
					allApplied = false
					break
				}
			}
			if allApplied {
				state = domain.ProbeActivationActive
			}
		case domain.ProbeInstallResourceNotStarted:
			// The executor genuinely never invoked the mutation; the activation
			// keeps running so the owner can still finish it honestly.
		case domain.ProbeInstallResourceUnknown:
			// Unknown outcome atomically interrupts the activation and preserves
			// the step uncertainty. No dependent work can start afterwards.
			state = domain.ProbeActivationInterrupted
			code := outcome.ErrorCode
			activationCode = &code
		}
		if err := graph.setLifecycle(ctx, tx, state, activationCode, updated); err != nil {
			return err
		}
		recorded, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &recorded
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

// AdmitActivationAction locks current authority and durably consumes exactly one
// probe_init or service_start attempt, only while the activation is running
// under the intent's owner, its install identity equals the intent's, the step
// is in flight with the intent's attempt and every earlier step is applied, all
// before the fixed deadline. The insert is plain: every uniqueness collision
// refuses, even an exact replay, and the successful insert itself proves fresh
// consumption. All failures return a zero admission, including ambiguous commit
// acknowledgment and caller cancellation after commit. A later
// cancellation/expiry does not retroactively revoke an admitted remote action.
func (s *ProbeInstallActivationStore) AdmitActivationAction(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
	var zero domain.ProbeHostActionAdmission
	if ctx == nil || (reflect.ValueOf(ctx).Kind() == reflect.Pointer && reflect.ValueOf(ctx).IsNil()) {
		return zero, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if !request.Valid() {
		return zero, domain.ErrValidation
	}
	// Install host actions (data_directory, unit_file, service) belong to the
	// install journal's admission store and are never consumable here.
	switch request.Intent.Kind {
	case domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart:
	default:
		return zero, domain.ErrValidation
	}
	if s == nil || s.db == nil {
		return zero, domain.ErrInternal
	}
	var out domain.ProbeHostActionAdmission
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = zero
		commit.begin()
		intent := request.Intent
		// Valid() already derived this spec; it is the only accepted shape.
		spec, ok := intent.Resource()
		if !ok {
			return domain.ErrValidation
		}
		graph, err := loadProbeInstallActivationGraph(ctx, tx, intent.Identity.OperationID)
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
		if current.Identity.Install != intent.Identity || current.OwnerID != intent.OwnerID ||
			current.State != domain.ProbeActivationRunning || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		index, ok := graph.stepIndex(intent.Kind)
		if !ok || index != spec.Ordinal-1 {
			return commit.fail(domain.ErrValidation)
		}
		step := current.Steps[index]
		if step.Spec != spec || step.State != domain.ProbeInstallResourceInFlight || step.AttemptID != intent.AttemptID {
			return commit.fail(ports.ErrConflict)
		}
		for i := 0; i < index; i++ {
			if current.Steps[i].State != domain.ProbeInstallResourceApplied {
				return commit.fail(ports.ErrConflict)
			}
		}
		stamp := installJournalStartStamp(now, current.UpdatedAt, step.StartedAt)
		if !stamp.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		row := probeActivationAdmissionModel{
			OperationID: intent.Identity.OperationID, Kind: intent.Kind, OwnerID: intent.OwnerID,
			AttemptID: intent.AttemptID, AuthorityKeyID: request.Challenge.AuthorityKeyID,
			NonceHex: hex.EncodeToString(request.Challenge.Nonce[:]), MarkerSHA256: request.Challenge.MarkerSHA256,
			AdmittedAt: stamp,
		}
		// Never IGNORE/upsert: the successful insert itself proves fresh
		// consumption. Every uniqueness collision refuses, even exact replay.
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			if installJournalRaced(err) {
				return ports.ErrConflict
			}
			return err
		}
		if _, err := tx.NewUpdate().Table("probe_install_activations").Set("updated_at = ?", stamp).
			Where("operation_id = ?", row.OperationID).Exec(ctx); err != nil {
			return err
		}
		var saved probeActivationAdmissionModel
		if err := tx.NewSelect().Model(&saved).Where("operation_id = ? AND kind = ?", row.OperationID, row.Kind).Scan(ctx); err != nil {
			return err
		}
		if saved.OperationID != row.OperationID || saved.Kind != row.Kind || saved.OwnerID != row.OwnerID ||
			saved.AttemptID != row.AttemptID || saved.AuthorityKeyID != row.AuthorityKeyID || saved.NonceHex != row.NonceHex ||
			saved.MarkerSHA256 != row.MarkerSHA256 || !saved.AdmittedAt.Equal(stamp) {
			return domain.ErrValidation
		}
		admission := domain.ProbeHostActionAdmission{Request: request, AdmittedAt: saved.AdmittedAt.UTC()}
		if !admission.Valid() {
			return domain.ErrValidation
		}
		out = admission
		return nil
	})
	if err != nil {
		return zero, binaryStageAdmissionError(err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if commit.pending != nil {
		return zero, binaryStageAdmissionError(commit.pending)
	}
	if !out.Valid() || out.Request != request {
		return zero, domain.ErrInternal
	}
	return out, nil
}
