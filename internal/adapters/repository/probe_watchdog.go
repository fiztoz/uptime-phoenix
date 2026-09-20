package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeWatchdogStore records hub-owned connection health in either hub engine.
// Its journal is independent of the remote edge stream and never advances it.
type ProbeWatchdogStore struct {
	db        *bun.DB
	protector ports.ProbeConfigProtector
	telemetry ports.EdgeTelemetryEncoder
}

// NewProbeWatchdogStore wires explicit installation authority and wire encoding.
func NewProbeWatchdogStore(db *bun.DB, protector ports.ProbeConfigProtector, telemetry ports.EdgeTelemetryEncoder) *ProbeWatchdogStore {
	return &ProbeWatchdogStore{db: db, protector: protector, telemetry: telemetry}
}

var _ ports.ProbeWatchdogRepository = (*ProbeWatchdogStore)(nil)

type probeWatchdogModel struct {
	bun.BaseModel  `bun:"table:probe_watchdog_state"`
	ProbeID        string `bun:",pk"`
	Version        int64
	ConfigRevision int64
	Armed          bool
	LossElapsedNS  int64 `bun:"loss_elapsed_ns"`
	PendingLoss    bool
	Status         domain.ProbeWatchdogStatus
	SourceAlertID  *string
	IncidentSeq    int64
	LastEnqueuedAt *time.Time
	UpdatedAt      time.Time
}

type probeWatchdogEventModel struct {
	bun.BaseModel     `bun:"table:probe_watchdog_events"`
	ProbeID           string `bun:",pk"`
	Seq               int64  `bun:",pk"`
	SourceAlertID     string
	TransitionVersion int64
	ObservedAt        time.Time
	Payload           []byte
}

func hubOwnsWatchdogIncident(ctx context.Context, db bun.IDB, sourceID string) (bool, error) {
	return db.NewSelect().Table("probe_hub_watchdog_incidents").Where("source_alert_id = ?", sourceID).Exists(ctx)
}

func validHubWatchdogAuthority(a domain.ProbeWatchdogAuthority) bool {
	return domain.ValidHubID(a.HubID) && validRemoteProbeID(a.ProbeID) && domain.ValidHubID(a.StreamID) && a.StreamID != domain.LocalStreamID && a.HealthGeneration >= 0 && a.RuntimeOwner.ProbeID == a.ProbeID && domain.ValidHubID(a.RuntimeOwner.OwnerID) && a.RuntimeOwner.Epoch > 0
}

// lockAuthority follows registration -> runtime -> session -> configuration
// ordering. DB time owns leases; caller wall time never authorizes a source write.
func (s *ProbeWatchdogStore) lockAuthority(ctx context.Context, tx bun.Tx, a domain.ProbeWatchdogAuthority) (probeRuntimeModel, error) {
	var zero probeRuntimeModel
	if _, err := tx.ExecContext(ctx, "UPDATE probes SET id=id WHERE id=?", a.ProbeID); err != nil {
		return zero, err
	}
	var registration probeRegistrationModel
	if err := tx.NewSelect().Model(&registration).Where("id=?", a.ProbeID).Scan(ctx); err != nil {
		return zero, err
	}
	if !registration.Enabled || registration.Kind != domain.ProbeKindRemote {
		return zero, ports.ErrConflict
	}
	now, err := replayDatabaseTime(ctx, tx)
	if err != nil {
		return zero, err
	}
	parent, err := readProbeRuntime(ctx, tx, a.ProbeID)
	if err != nil {
		return zero, err
	}
	if !matchesProbeRuntime(parent, a.RuntimeOwner) || parent.LeaseUntil <= now.Unix() {
		return zero, ports.ErrConflict
	}
	if a.HealthGeneration > 0 {
		child, err := readProbeSession(ctx, tx, a.ProbeID)
		if err != nil {
			return zero, err
		}
		if child.OwnerID != a.RuntimeOwner.OwnerID || child.Generation != a.HealthGeneration || child.LeaseUntil <= now.Unix() {
			return zero, ports.ErrConflict
		}
		parent.LeaseUntil = min(parent.LeaseUntil, child.LeaseUntil)
	}
	var connection probeConnectionRow
	if err := tx.NewSelect().Model(&connection).Where("probe_id=?", a.ProbeID).Scan(ctx); err != nil {
		return zero, err
	}
	if connection.HubID != a.HubID || connection.StreamID != a.StreamID {
		return zero, ports.ErrConflict
	}
	var installation probeInstallationModel
	if err := tx.NewSelect().Model(&installation).Where("id=1").Scan(ctx); err != nil {
		return zero, err
	}
	if installation.HubID != a.HubID || installation.KeyHash != s.protector.KeyHash(a.HubID) {
		return zero, domain.ErrProbeKeyMismatch
	}
	var stream probeStreamModel
	if err := tx.NewSelect().Model(&stream).Where("probe_id=? AND stream_id=?", a.ProbeID, a.StreamID).Scan(ctx); err != nil {
		return zero, err
	}
	if stream.RetiredAt != nil {
		return zero, ports.ErrConflict
	}
	return parent, nil
}

func readHubWatchdog(ctx context.Context, tx bun.Tx, probeID string) (domain.ProbeWatchdogState, error) {
	var row probeWatchdogModel
	err := tx.NewSelect().Model(&row).Where("probe_id=?", probeID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProbeWatchdogState{Status: domain.ProbeWatchdogUnarmed}, nil
	}
	if err != nil {
		return domain.ProbeWatchdogState{}, err
	}
	out := domain.ProbeWatchdogState{Version: row.Version, ConfigRevision: row.ConfigRevision, Checkpoint: domain.ProbeWatchdogCheckpoint{Armed: row.Armed, LossElapsed: time.Duration(row.LossElapsedNS), PendingLoss: row.PendingLoss}, Status: row.Status, IncidentSeq: row.IncidentSeq, LastEnqueuedAt: utcTimePtr(row.LastEnqueuedAt)}
	if row.SourceAlertID != nil {
		var inc probeIncidentModel
		if err := tx.NewSelect().Model(&inc).Where("source_alert_id=?", *row.SourceAlertID).Scan(ctx); err != nil {
			return domain.ProbeWatchdogState{}, err
		}
		owned, err := hubOwnsWatchdogIncident(ctx, tx, inc.SourceAlertID)
		if err != nil {
			return domain.ProbeWatchdogState{}, err
		}
		if !owned || inc.ProbeID != probeID || inc.Scope != string(domain.IncidentScopeProbeConnection) || inc.SubjectKind != domain.IncidentSubjectWatchdog || inc.MonitorID != nil || inc.AssignmentGeneration != nil {
			return domain.ProbeWatchdogState{}, domain.ErrInternal
		}
		value := incidentFromModel(&inc)
		out.Incident = &value
	}
	return out, nil
}

// ReadWatchdog returns a coherent source snapshot only to its current runtime.
func (s *ProbeWatchdogStore) ReadWatchdog(ctx context.Context, a domain.ProbeWatchdogAuthority) (domain.ProbeWatchdogState, error) {
	if s == nil || s.db == nil || s.protector == nil || !validHubWatchdogAuthority(a) {
		return domain.ProbeWatchdogState{}, domain.ErrValidation
	}
	var out domain.ProbeWatchdogState
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := s.lockAuthority(ctx, tx, a); err != nil {
			return err
		}
		var err error
		out, err = readHubWatchdog(ctx, tx, a.ProbeID)
		return err
	})
	if err != nil {
		return domain.ProbeWatchdogState{}, watchdogStorageError(ctx, err)
	}
	return out, nil
}

// CommitWatchdog atomically commits source history, checkpoint and send intents.
// It does not mutate the edge replay cursor, monitor health or mirrored receipts.
func (s *ProbeWatchdogStore) CommitWatchdog(ctx context.Context, a domain.ProbeWatchdogAuthority, r domain.ProbeWatchdogRecord) (domain.ProbeWatchdogState, error) {
	if s == nil || s.db == nil || s.protector == nil || s.telemetry == nil || !validHubWatchdogAuthority(a) {
		return domain.ProbeWatchdogState{}, domain.ErrValidation
	}
	r.At = r.At.UTC().Truncate(time.Microsecond)
	var out domain.ProbeWatchdogState
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		parent, err := s.lockAuthority(ctx, tx, a)
		if err != nil {
			return err
		}
		var active probeActiveConfigModel
		if err := tx.NewSelect().Model(&active).Where("probe_id=?", a.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if active.HubID != a.HubID || active.Revision != r.ConfigRevision {
			return ports.ErrConflict
		}
		before, err := readHubWatchdog(ctx, tx, a.ProbeID)
		if err != nil {
			return err
		}
		if before.Version != r.ExpectedVersion {
			return ports.ErrStaleLocalState
		}
		if err := domain.ValidateProbeWatchdogCommit(a.ProbeID, before, r); err != nil {
			return err
		}
		incident, seq := before.Incident, before.IncidentSeq
		if r.Incident != nil {
			if seq == math.MaxInt64 {
				return ports.ErrConflict
			}
			copy := *r.Incident
			copy.StartedAt = outboxTime(copy.StartedAt)
			if copy.ResolvedAt != nil {
				at := outboxTime(*copy.ResolvedAt)
				copy.ResolvedAt = &at
			}
			if copy.AckedAt != nil {
				at := outboxTime(*copy.AckedAt)
				copy.AckedAt = &at
			}
			incident = &copy
			seq++
			if err := putIncidentWithOwnerTx(ctx, tx, incident, true); err != nil {
				return err
			}
			payload, err := s.telemetry.EncodeIncident(seq, r.At, *incident)
			if err != nil {
				return domain.ErrValidation
			}
			event := probeWatchdogEventModel{ProbeID: a.ProbeID, Seq: seq, SourceAlertID: incident.SourceAlertID, TransitionVersion: incident.TransitionVersion, ObservedAt: r.At, Payload: payload}
			if _, err := tx.NewInsert().Model(&event).Exec(ctx); err != nil {
				return err
			}
		}
		row := probeWatchdogModel{ProbeID: a.ProbeID, Version: before.Version + 1, ConfigRevision: r.ConfigRevision, Armed: r.Checkpoint.Armed, LossElapsedNS: int64(r.Checkpoint.LossElapsed), PendingLoss: r.Checkpoint.PendingLoss, Status: r.Status, IncidentSeq: seq, LastEnqueuedAt: utcTimePtr(before.LastEnqueuedAt), UpdatedAt: r.At}
		if incident != nil {
			row.SourceAlertID = &incident.SourceAlertID
		}
		for _, intent := range r.DeliveryIntents {
			exists, err := deliveryIDExistsTx(ctx, tx, "probe_delivery_events", intent.DeliveryID)
			if err != nil {
				return err
			}
			if exists {
				return ports.ErrConflict
			}
			status := domain.StatusDown
			if incident.Status == domain.AlertStatusResolved {
				status = domain.StatusUp
			}
			item := deliveryIntentModel{DeliveryID: intent.DeliveryID, SourceAlertID: intent.SourceAlertID, SourceTransitionVersion: intent.SourceTransitionVersion, ProbeID: a.ProbeID, NotificationID: intent.NotificationID, NotificationVersion: intent.NotificationVersion, EventKind: domain.DeliveryEventProbeConnection, SourceSeq: seq, ConfigRevision: r.ConfigRevision, CheckStatus: int(status), CheckOutput: incident.Reason, ObservedAt: r.At, IncidentStatus: incident.Status, StartedAt: outboxTime(incident.StartedAt), ResolvedAt: utcTimePtr(incident.ResolvedAt), AvailableAt: outboxTime(intent.AvailableAt), Status: domain.DeliveryStatusPending, CreatedAt: r.At}
			if _, err := tx.NewInsert().Model(&item).Exec(ctx); err != nil {
				return err
			}
		}
		if len(r.DeliveryIntents) > 0 {
			at := r.At
			if before.LastEnqueuedAt != nil && before.LastEnqueuedAt.After(at) {
				at = *before.LastEnqueuedAt
			}
			row.LastEnqueuedAt = &at
		}
		if before.Version == 0 {
			_, err = tx.NewInsert().Model(&row).Exec(ctx)
		} else {
			_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
		}
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if parent.LeaseUntil <= now.Unix() {
			return ports.ErrConflict
		}
		out, err = readHubWatchdog(ctx, tx, a.ProbeID)
		return err
	})
	if err != nil {
		return domain.ProbeWatchdogState{}, watchdogStorageError(ctx, err)
	}
	return out, nil
}

func watchdogStorageError(ctx context.Context, err error) error {
	if errors.Is(err, ports.ErrStaleLocalState) {
		return ports.ErrStaleLocalState
	}
	return remoteSyncError(ctx, probeRegistryError(err))
}
