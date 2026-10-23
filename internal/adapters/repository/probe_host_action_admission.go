package repository

import (
	"context"
	"encoding/hex"
	"reflect"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type hostActionAdmissionModel struct {
	bun.BaseModel  `bun:"table:probe_host_action_admissions"`
	OperationID    string    `bun:"operation_id,pk"`
	Kind           string    `bun:"kind,pk"`
	OwnerID        string    `bun:"owner_id"`
	AttemptID      string    `bun:"attempt_id"`
	AuthorityKeyID string    `bun:"authority_key_id"`
	NonceHex       string    `bun:"nonce_hex"`
	MarkerSHA256   string    `bun:"marker_sha256"`
	AdmittedAt     time.Time `bun:"admitted_at"`
}

// ProbeHostActionAdmissionStore consumes one exact journal data_directory,
// unit_file or service attempt. It shares operation-first locking with the
// journal, never signs inside a transaction and never returns a historical
// consumption as a new admission.
type ProbeHostActionAdmissionStore struct{ db *bun.DB }

var _ ports.ProbeHostActionAdmissionRepository = (*ProbeHostActionAdmissionStore)(nil)

// NewProbeHostActionAdmissionStore binds the existing SQL connection pool.
// It starts no work and owns no connection or credential lifetime.
func NewProbeHostActionAdmissionStore(db *bun.DB) *ProbeHostActionAdmissionStore {
	return &ProbeHostActionAdmissionStore{db: db}
}

// AdmitHostAction locks current authority and durably consumes exactly one
// root/systemd host-action attempt, only after every earlier resource of the
// layout is applied. All failures return a zero admission, including ambiguous
// commit acknowledgment and caller cancellation after commit. A later
// cancellation/expiry does not retroactively revoke an admitted remote action.
func (s *ProbeHostActionAdmissionStore) AdmitHostAction(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
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
		graph, err := loadProbeInstallGraph(ctx, tx, intent.Identity.OperationID)
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
		if current.Identity != intent.Identity || current.OwnerID != intent.OwnerID ||
			current.State != domain.ProbeInstallRunning || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		index, ok := graph.resourceIndex(intent.Kind)
		if !ok || index != spec.Ordinal-1 {
			return commit.fail(domain.ErrValidation)
		}
		resource := current.Resources[index]
		if resource.Spec != spec ||
			resource.State != domain.ProbeInstallResourceInFlight || resource.AttemptID != intent.AttemptID {
			return commit.fail(ports.ErrConflict)
		}
		for i := 0; i < index; i++ {
			if current.Resources[i].State != domain.ProbeInstallResourceApplied {
				return commit.fail(ports.ErrConflict)
			}
		}
		stamp := installJournalStartStamp(now, current.UpdatedAt, resource.StartedAt)
		if !stamp.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		row := hostActionAdmissionModel{
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
		if _, err := tx.NewUpdate().Table("probe_install_operations").Set("updated_at = ?", stamp).
			Where("operation_id = ?", row.OperationID).Exec(ctx); err != nil {
			return err
		}
		var saved hostActionAdmissionModel
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
