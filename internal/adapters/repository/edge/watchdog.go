package edge

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type edgeWatchdogRow struct {
	bun.BaseModel  `bun:"table:edge_watchdog_state"`
	ID             int `bun:"id,pk"`
	Version        int64
	ConfigRevision int64
	Armed          bool
	LossElapsedNS  int64 `bun:"loss_elapsed_ns"`
	PendingLoss    bool
	Status         domain.ProbeWatchdogStatus
	SourceAlertID  *string
	IncidentSeq    int64
	LastEnqueuedAt *int64
	UpdatedAt      int64
}

func validEdgeWatchdogAuthority(a domain.ProbeWatchdogAuthority) bool {
	return domain.ValidHubID(a.HubID) && domain.ValidHubID(a.ProbeID) && domain.ValidHubID(a.StreamID) && a.HealthGeneration >= 0 && a.RuntimeOwner == (domain.ProbeRuntimeLease{})
}
func checkEdgeWatchdogAuthority(a domain.ProbeWatchdogAuthority, i domain.EdgeIdentity) error {
	if a.HubID != i.HubID || a.ProbeID != i.ProbeID || a.StreamID != i.StreamID || a.HealthGeneration != 0 && a.HealthGeneration != i.ConnectionGeneration {
		return ports.ErrConflict
	}
	return nil
}

func readEdgeWatchdog(ctx context.Context, db bun.IDB, probeID string) (domain.ProbeWatchdogState, error) {
	var row edgeWatchdogRow
	err := db.NewSelect().Model(&row).Where("id = 1").Scan(ctx)
	if errors.Is(storageError(ctx, err), ports.ErrNotFound) {
		return domain.ProbeWatchdogState{Status: domain.ProbeWatchdogUnarmed}, nil
	}
	if err != nil {
		return domain.ProbeWatchdogState{}, err
	}
	out := domain.ProbeWatchdogState{Version: row.Version, ConfigRevision: row.ConfigRevision, Checkpoint: domain.ProbeWatchdogCheckpoint{Armed: row.Armed, LossElapsed: time.Duration(row.LossElapsedNS), PendingLoss: row.PendingLoss}, Status: row.Status, IncidentSeq: row.IncidentSeq, LastEnqueuedAt: timeFromMicro(row.LastEnqueuedAt)}
	if row.SourceAlertID != nil {
		var incident edgeIncidentRow
		if err := db.NewSelect().Model(&incident).Where("source_alert_id = ?", *row.SourceAlertID).Scan(ctx); err != nil {
			return domain.ProbeWatchdogState{}, err
		}
		if incident.Scope != string(domain.IncidentScopeProbeConnection) || incident.SubjectKind != domain.IncidentSubjectWatchdog || incident.MonitorID != 0 || incident.Generation != 0 {
			return domain.ProbeWatchdogState{}, ErrStorage
		}
		out.Incident = incident.incident(probeID)
	}
	return out, nil
}

// ReadWatchdog reads one coherent source checkpoint and incident. It does not
// infer elapsed outage duration from the source wall clock or sequence counter.
func (s *Store) ReadWatchdog(ctx context.Context, authority domain.ProbeWatchdogAuthority) (domain.ProbeWatchdogState, error) {
	if !validEdgeWatchdogAuthority(authority) {
		return domain.ProbeWatchdogState{}, domain.ErrValidation
	}
	var out domain.ProbeWatchdogState
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		i, err := readIdentity(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkEdgeWatchdogAuthority(authority, i); err != nil {
			return err
		}
		out, err = readEdgeWatchdog(ctx, tx, i.ProbeID)
		return err
	})
	if err != nil {
		return domain.ProbeWatchdogState{}, storageError(ctx, err)
	}
	return out, nil
}

// CommitWatchdog commits checkpoint, lifecycle, exact sequenced telemetry and
// provider intents together. A stale config, health session or version commits
// nothing. A successful checkpoint without a transition creates no telemetry.
func (s *Store) CommitWatchdog(ctx context.Context, authority domain.ProbeWatchdogAuthority, record domain.ProbeWatchdogRecord) (domain.ProbeWatchdogState, error) {
	if !validEdgeWatchdogAuthority(authority) || s.telemetry == nil {
		return domain.ProbeWatchdogState{}, domain.ErrValidation
	}
	record.At = record.At.UTC().Truncate(time.Microsecond)
	var out domain.ProbeWatchdogState
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if err := checkEdgeWatchdogAuthority(authority, i); err != nil {
			return err
		}
		if record.ConfigRevision != i.ConfigRevision || i.ConfigRevision == 0 {
			return ports.ErrConflict
		}
		before, err := readEdgeWatchdog(ctx, tx, i.ProbeID)
		if err != nil {
			return err
		}
		if before.Version != record.ExpectedVersion {
			return ports.ErrStaleLocalState
		}
		if err := domain.ValidateProbeWatchdogCommit(i.ProbeID, before, record); err != nil {
			return err
		}
		incident, seq := before.Incident, before.IncidentSeq
		if record.Incident != nil {
			if i.LastCreatedSeq == math.MaxInt64 {
				return ports.ErrConflict
			}
			incident = record.Incident
			seq = i.LastCreatedSeq + 1
			row := newEdgeIncidentRow(*incident)
			if before.Incident == nil || before.Incident.Status == domain.AlertStatusResolved {
				_, err = tx.NewInsert().Model(&row).Exec(ctx)
			} else {
				_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
			}
			if err != nil {
				return err
			}
			payload, err := s.telemetry.EncodeIncident(seq, record.At, *incident)
			if err != nil {
				return domain.ErrValidation
			}
			if err := s.appendTelemetry(ctx, tx, seq, "watchdog.transition", record.At, payload); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE edge_identity SET last_created_seq = ? WHERE id = 1", seq); err != nil {
				return err
			}
		}
		row := edgeWatchdogRow{ID: 1, Version: before.Version + 1, ConfigRevision: record.ConfigRevision, Armed: record.Checkpoint.Armed, LossElapsedNS: int64(record.Checkpoint.LossElapsed), PendingLoss: record.Checkpoint.PendingLoss, Status: record.Status, IncidentSeq: seq, LastEnqueuedAt: microFromTime(before.LastEnqueuedAt), UpdatedAt: record.At.UnixMicro()}
		if incident != nil {
			row.SourceAlertID = &incident.SourceAlertID
		}
		for _, intent := range record.DeliveryIntents {
			status := domain.StatusDown
			if incident.Status == domain.AlertStatusResolved {
				status = domain.StatusUp
			}
			item := domain.QueuedDelivery{DeliveryIntent: intent, StreamID: i.StreamID, SourceSeq: seq, ConfigRevision: record.ConfigRevision, CheckStatus: status, CheckOutput: incident.Reason, ObservedAt: record.At, IncidentStatus: incident.Status, StartedAt: incident.StartedAt, ResolvedAt: incident.ResolvedAt, CreatedAt: record.At}
			if err := insertEdgeQueuedDelivery(ctx, tx, item); err != nil {
				return err
			}
		}
		if len(record.DeliveryIntents) > 0 {
			at := record.At
			if before.LastEnqueuedAt != nil && before.LastEnqueuedAt.After(at) {
				at = *before.LastEnqueuedAt
			}
			row.LastEnqueuedAt = microFromTime(&at)
		}
		if before.Version == 0 {
			_, err = tx.NewInsert().Model(&row).Exec(ctx)
		} else {
			_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
		}
		if err != nil {
			return err
		}
		out, err = readEdgeWatchdog(ctx, tx, i.ProbeID)
		return err
	})
	if err != nil {
		return domain.ProbeWatchdogState{}, err
	}
	return out, nil
}
