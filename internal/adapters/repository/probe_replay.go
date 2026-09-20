package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type replayReceiptModel struct {
	bun.BaseModel        `bun:"table:probe_telemetry_receipts,alias:rr"`
	ProbeID              string `bun:",pk"`
	StreamID             string `bun:",pk"`
	Seq                  int64  `bun:",pk"`
	Digest               string
	Kind                 string
	RejectionCode        string
	ReceivedAt           time.Time
	SourceAlertID        *string
	TransitionVersion    *int64
	MonitorID            *int64
	AssignmentGeneration *int64
	ConfigRevision       *int64
	Status               *string
	StartedAt            *time.Time
	ResolvedAt           *time.Time
}

// ProbeReplayStore commits mixed remote telemetry under connector authority.
// It only mirrors source results: it has no provider or dispatch dependencies.
type ProbeReplayStore struct {
	db        *bun.DB
	decoder   ports.EdgeConfigDecoder
	protector ports.ProbeConfigProtector
}

var _ ports.ProbeReplayRepository = (*ProbeReplayStore)(nil)

// NewProbeReplayStore uses the same exact protected snapshots as edge execution.
func NewProbeReplayStore(db *bun.DB, decoder ports.EdgeConfigDecoder, protector ports.ProbeConfigProtector) *ProbeReplayStore {
	return &ProbeReplayStore{db: db, decoder: decoder, protector: protector}
}

// GetCursor reads registered, nonretired progress without creating a stream.
func (s *ProbeReplayStore) GetCursor(ctx context.Context, probeID, streamID string) (int64, error) {
	return NewProbeConnectorStore(s.db).GetConnectionCursor(ctx, probeID, streamID)
}

// IngestReplayBatch atomically commits accepted evidence, rejected receipts and
// contiguous progress. Duplicate prefixes read receipts without mutating mirrors.
func (s *ProbeReplayStore) IngestReplayBatch(ctx context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch, authorizer ports.ProbeReplayAuthorizer) (*domain.ProbeReplayResult, error) {
	if s == nil || s.db == nil || s.decoder == nil || s.protector == nil || authorizer == nil || !domain.ValidProbeReplayBatch(session, batch) {
		return nil, domain.ErrValidation
	}
	var result *domain.ProbeReplayResult
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		// SERIALIZABLE prevents stale assignment snapshots. Bounded MariaDB
		// deadlock retries reuse the existing configuration authority policy.
		result = &domain.ProbeReplayResult{StreamID: batch.StreamID, Rejected: []domain.ProbeReplayRejection{}}
		if _, err := tx.ExecContext(ctx, "UPDATE probes SET id = id WHERE id = ?", session.ProbeID); err != nil {
			return err
		}
		var registration probeRegistrationModel
		if err := tx.NewSelect().Model(&registration).Where("id = ?", session.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if !registration.Enabled || registration.Kind != domain.ProbeKindRemote {
			return ports.ErrConflict
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		lease, err := readProbeSession(ctx, tx, session.ProbeID)
		if err != nil {
			return err
		}
		if lease.OwnerID != session.OwnerID || lease.Generation != session.ConnectionGeneration || lease.LeaseUntil <= now.Unix() {
			return ports.ErrConflict
		}
		var connection probeConnectionRow
		if err := tx.NewSelect().Model(&connection).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if connection.HubID != session.HubID || connection.StreamID != session.StreamID {
			return ports.ErrConflict
		}
		var installation probeInstallationModel
		if err := tx.NewSelect().Model(&installation).Where("id = 1").Scan(ctx); err != nil {
			return err
		}
		if installation.HubID != session.HubID || installation.KeyHash != s.protector.KeyHash(session.HubID) {
			return domain.ErrProbeKeyMismatch
		}
		var stream probeStreamModel
		if err := tx.NewSelect().Model(&stream).Where("probe_id = ? AND stream_id = ?", session.ProbeID, session.StreamID).Scan(ctx); err != nil {
			return err
		}
		if stream.RetiredAt != nil || batch.FirstSeq > stream.CommittedSeq && batch.FirstSeq-stream.CommittedSeq != 1 {
			return ports.ErrConflict
		}
		// Decode each retained revision at most once in this bounded transaction.
		configs := make(map[int64]*domain.EdgeResolvedConfig)
		for _, event := range batch.Events {
			if event.Seq <= stream.CommittedSeq {
				var receipt replayReceiptModel
				if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND stream_id = ? AND seq = ?", session.ProbeID, session.StreamID, event.Seq).Scan(ctx); err != nil {
					return err
				}
				if receipt.Digest != event.Digest || receipt.Kind != event.Kind {
					return ports.ErrConflict
				}
				if receipt.RejectionCode != "" {
					result.Rejected = append(result.Rejected, domain.ProbeReplayRejection{Seq: event.Seq, Code: receipt.RejectionCode})
				} else {
					result.DuplicateCount++
				}
				continue
			}
			// Both hub engines and the edge incident store persist microseconds.
			// Preserve wire digest identity, compare lifecycle identity at that
			// storage precision without mutating the caller's source event.
			if event.Incident != nil {
				incident := *event.Incident
				incident.StartedAt = incident.StartedAt.UTC().Truncate(time.Microsecond)
				if incident.ResolvedAt != nil {
					at := incident.ResolvedAt.UTC().Truncate(time.Microsecond)
					incident.ResolvedAt = &at
				}
				event.Incident = &incident
			}
			facts, err := s.replayFacts(ctx, tx, session, event, configs)
			if err != nil {
				return err
			}
			code, accepted := authorizer.AuthorizeEvent(ctx, facts, event, now)
			receipt := replayReceiptModel{ProbeID: session.ProbeID, StreamID: session.StreamID, Seq: event.Seq, Digest: event.Digest, Kind: event.Kind, RejectionCode: code, ReceivedAt: now}
			if accepted {
				switch event.Kind {
				case domain.ReplayKindObservation:
					obs := *event.Observation
					obs.ReceivedAt = now
					row := observationModel(obs)
					if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
						return err
					}
					if err := markDirtyTx(ctx, tx, domain.DirtyBucketsForObservation(obs)); err != nil {
						return err
					}
					if err := updateReplayState(ctx, tx, obs, now); err != nil {
						return err
					}
				case domain.ReplayKindAlertTransition:
					incident := *event.Incident
					if err := putIncidentTx(ctx, tx, &incident); err != nil {
						return err
					}
					receipt.SourceAlertID = &incident.SourceAlertID
					receipt.TransitionVersion = &incident.TransitionVersion
					receipt.MonitorID = &incident.MonitorID
					receipt.AssignmentGeneration = &incident.AssignmentGeneration
					receipt.ConfigRevision = &incident.ConfigRevision
					receipt.Status = &incident.Status
					receipt.StartedAt = &incident.StartedAt
					receipt.ResolvedAt = incident.ResolvedAt
				case domain.ReplayKindDeliveryResult:
					delivery := *event.Delivery
					if err := putDeliveryTx(ctx, tx, &delivery, false); err != nil {
						return err
					}
				default:
					return domain.ErrValidation
				}
				result.AcceptedCount++
			} else {
				if code == "" || len(code) > 128 {
					return domain.ErrValidation
				}
				result.Rejected = append(result.Rejected, domain.ProbeReplayRejection{Seq: event.Seq, Code: code})
			}
			if _, err := tx.NewInsert().Model(&receipt).Exec(ctx); err != nil {
				return err
			}
		}
		if batch.LastSeq > stream.CommittedSeq {
			if _, err := tx.NewUpdate().Table("probe_streams").Set("committed_seq = ?", batch.LastSeq).Set("updated_at = ?", now).Where("probe_id = ? AND stream_id = ?", session.ProbeID, session.StreamID).Exec(ctx); err != nil {
				return err
			}
		}
		committedAt, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if lease.LeaseUntil <= committedAt.Unix() {
			return ports.ErrConflict
		}
		// A bounded ACK proves this request's entire prefix, even if the stream
		// already holds more. Never ask an edge to prune beyond the batch sent.
		result.CommittedSeq = batch.LastSeq
		return nil
	})
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	return result, nil
}

func replayDatabaseTime(ctx context.Context, tx bun.Tx) (time.Time, error) {
	query := "SELECT strftime('%Y-%m-%d %H:%M:%f', 'now')"
	if tx.Dialect().Name() == dialect.MySQL {
		query = "SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f')"
	}
	var value string
	if err := tx.NewRaw(query).Scan(ctx, &value); err != nil {
		return time.Time{}, err
	}
	return time.ParseInLocation("2006-01-02 15:04:05.999999", value, time.UTC)
}

func (s *ProbeReplayStore) replayFacts(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession, event domain.ProbeReplayEvent, configs map[int64]*domain.EdgeResolvedConfig) (domain.ProbeReplayAuthorityFacts, error) {
	f := domain.ProbeReplayAuthorityFacts{ProbeID: session.ProbeID, StreamID: session.StreamID, Channels: map[int64]int64{}}
	switch {
	case event.Observation != nil:
		f.MonitorID = event.Observation.MonitorID
		f.ConfigRevision = event.Observation.ConfigRevision
	case event.Incident != nil:
		i := event.Incident
		f.MonitorID = i.MonitorID
		f.ConfigRevision = i.ConfigRevision
		var prior probeIncidentModel
		err := tx.NewSelect().Model(&prior).Where("source_alert_id = ?", i.SourceAlertID).Scan(ctx)
		if err == nil {
			v := incidentFromModel(&prior)
			f.PriorIncident = &v
		} else if !errors.Is(err, sql.ErrNoRows) {
			return f, err
		}
	case event.Delivery != nil:
		d := event.Delivery
		var parent replayReceiptModel
		err := tx.NewSelect().Model(&parent).Where("source_alert_id = ? AND transition_version = ? AND probe_id = ? AND rejection_code = ''", d.SourceAlertID, d.SourceTransitionVersion, session.ProbeID).Scan(ctx)
		if err == nil {
			if parent.MonitorID == nil || parent.AssignmentGeneration == nil || parent.ConfigRevision == nil || parent.Status == nil || parent.StartedAt == nil {
				return f, domain.ErrInternal
			}
			f.ParentTransition = &domain.RegionalIncident{SourceAlertID: d.SourceAlertID, TransitionVersion: d.SourceTransitionVersion, ProbeID: session.ProbeID, MonitorID: *parent.MonitorID, AssignmentGeneration: *parent.AssignmentGeneration, ConfigRevision: *parent.ConfigRevision, Status: *parent.Status, StartedAt: parent.StartedAt.UTC(), ResolvedAt: utcTimePtr(parent.ResolvedAt)}
			f.MonitorID = *parent.MonitorID
			f.ConfigRevision = *parent.ConfigRevision
		} else if !errors.Is(err, sql.ErrNoRows) {
			return f, err
		}
		var prior probeDeliveryModel
		err = tx.NewSelect().Model(&prior).Where("delivery_id = ?", d.DeliveryID).Scan(ctx)
		if err == nil {
			v := deliveryFromModel(&prior)
			f.PriorDelivery = &v
		} else if !errors.Is(err, sql.ErrNoRows) {
			return f, err
		}
		f.DeliveryIntentExists, err = deliveryIDExistsTx(ctx, tx, "probe_delivery_intents", d.DeliveryID)
		if err != nil {
			return f, err
		}
	}
	if f.MonitorID <= 0 || f.ConfigRevision <= 0 {
		return f, nil
	}
	var err error
	f.MonitorExists, err = tx.NewSelect().Table("monitors").Where("id = ?", f.MonitorID).Exists(ctx)
	if err != nil {
		return f, err
	}
	// Lock assignment authority before committing evidence. SERIALIZABLE also
	// protects absent membership and monitor-deletion predicates.
	if f.MonitorExists {
		if _, err := tx.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET revision = revision WHERE monitor_id = ?", f.MonitorID); err != nil {
			return f, err
		}
	}
	var history []probeAssignmentHistoryModel
	if err := tx.NewSelect().Model(&history).Where("monitor_id = ? AND probe_id = ? AND started_at <= ?", f.MonitorID, session.ProbeID, event.ObservedAt.UTC()).Where("ended_at IS NULL OR ended_at > ?", event.ObservedAt.UTC()).Scan(ctx); err != nil {
		return f, err
	}
	for _, row := range history {
		interval := domain.AssignmentInterval{ProbeID: row.ProbeID, Generation: row.Generation, Revision: row.Revision, Policy: row.HealthPolicy, From: row.StartedAt.UTC()}
		if row.EndedAt != nil {
			interval.To = row.EndedAt.UTC()
		}
		f.AssignmentHistory = append(f.AssignmentHistory, interval)
	}
	resolved, found := configs[f.ConfigRevision]
	if !found {
		// Keep one decoded revision, not 256 potentially large secret graphs.
		clear(configs)
		var snapshot probeConfigModel
		err := tx.NewSelect().Model(&snapshot).Where("probe_id = ? AND revision = ?", session.ProbeID, f.ConfigRevision).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			configs[f.ConfigRevision] = nil
			return f, nil
		}
		if err != nil {
			return f, err
		}
		if snapshot.HubID != session.HubID {
			return f, ports.ErrConflict
		}
		plain, err := s.protector.Open(ctx, snapshot.domain().ProbeConfigMetadata, snapshot.ProtectedPayload)
		if err != nil {
			return f, err
		}
		value, err := s.decoder.DecodeEdge(ctx, plain, domain.ProbeConfigTarget{HubID: session.HubID, ProbeID: session.ProbeID})
		clear(plain)
		if err != nil {
			return f, err
		}
		resolved = value
		configs[f.ConfigRevision] = resolved
	}
	if resolved == nil {
		return f, nil
	}
	f.ConfigEffectiveAt = resolved.Metadata.EffectiveAt.UTC()
	for _, a := range resolved.Assignments {
		if a.Monitor != nil && a.Monitor.ID == f.MonitorID {
			f.ConfigAssignment = &domain.EdgeAssignmentIdentity{MonitorID: a.Monitor.ID, Generation: a.Generation, Active: a.Monitor.Active}
			for _, link := range a.NotificationLinks {
				if channel, ok := resolved.Channels[link.NotificationID]; ok && channel.Notification != nil && channel.Notification.Active {
					f.Channels[link.NotificationID] = channel.Version
				}
			}
			break
		}
	}
	return f, nil
}

func updateReplayState(ctx context.Context, tx bun.Tx, obs domain.RegionalObservation, now time.Time) error {
	if obs.ObservedAt.After(now) {
		return nil
	}
	var assignment probeAssignmentModel
	err := tx.NewSelect().Model(&assignment).Where("monitor_id = ? AND probe_id = ? AND active = ?", obs.MonitorID, obs.ProbeID, true).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if assignment.Generation != obs.AssignmentGeneration {
		return nil
	}
	var existing monitorProbeStateModel
	err = tx.NewSelect().Model(&existing).Where("monitor_id = ? AND probe_id = ?", obs.MonitorID, obs.ProbeID).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (existing.AssignmentGeneration > obs.AssignmentGeneration || existing.StreamID == obs.StreamID && existing.Seq >= obs.Seq || existing.ObservedAt.After(obs.ObservedAt)) {
		return nil
	}
	state := domain.RegionalState{MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, AssignmentGeneration: obs.AssignmentGeneration, StreamID: obs.StreamID, Seq: obs.Seq, ConfigRevision: obs.ConfigRevision, Status: obs.Status, DownCount: obs.DownCount, ObservedAt: obs.ObservedAt.UTC(), ReceivedAt: now}
	if existing.AssignmentGeneration == obs.AssignmentGeneration {
		state.LastSuccessAt = utcTimePtr(existing.LastSuccessAt)
	}
	if obs.Status == domain.StatusUp {
		at := obs.ObservedAt.UTC()
		state.LastSuccessAt = &at
	}
	return upsertRegionalState(ctx, tx, state)
}
