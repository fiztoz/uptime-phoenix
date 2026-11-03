package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// regionalGroupAlerter pages hub-owned folder channels after remote evidence
// commits. It must not send the monitor's own notification links.
type regionalGroupAlerter interface {
	OnRegionalEvidence(ctx context.Context, monitorIDs []int64)
}

// ProbeReplayService coordinates batch ingestion using the repository and authorizer.
type ProbeReplayService struct {
	repo        ports.ProbeReplayRepository
	authorizer  ports.ProbeReplayAuthorizer
	browser     regionalGroupAlerter
	groupAlerts regionalGroupAlerter
	recovery    regionalRecovery
}

var _ ports.ProbeReplayService = (*ProbeReplayService)(nil)

// NewProbeReplayService constructs a new ProbeReplayService.
func NewProbeReplayService(repo ports.ProbeReplayRepository, authorizer ports.ProbeReplayAuthorizer) (*ProbeReplayService, error) {
	if repo == nil || authorizer == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeReplayService{
		repo:       repo,
		authorizer: authorizer,
	}, nil
}

// SetBrowserPublisher attaches best-effort post-commit browser invalidation.
func (s *ProbeReplayService) SetBrowserPublisher(browser regionalGroupAlerter) { s.browser = browser }

// SetGroupAlerter attaches hub-owned folder paging. Optional: without it,
// committed remote evidence still ingests and direct monitor alerts stay on
// the source. Folder channels are not copied onto the assignment.
func (s *ProbeReplayService) SetGroupAlerter(alerter regionalGroupAlerter) {
	if s != nil {
		s.groupAlerts = alerter
	}
}

// SetStatusPageRecovery attaches post-commit recovery from fresh overall health.
// It never invokes direct monitor notifications or the local dispatcher.
func (s *ProbeReplayService) SetStatusPageRecovery(overall AggregateStatusReader, resolver incidentAutoResolver) {
	s.recovery = regionalRecovery{overall: overall, resolver: resolver}
}

// ProcessBatch coordinates transactional ingestion of a replayed batch.
func (s *ProbeReplayService) ProcessBatch(ctx context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
	if !domain.ValidProbeReplayBatch(session, batch) {
		return nil, fmt.Errorf("invalid replay framing: %w", domain.ErrValidation)
	}
	result, err := s.repo.IngestReplayBatch(ctx, session, batch, s.authorizer)
	if errors.Is(err, domain.ErrInternal) && ctx.Err() == nil {
		// A failed/ambiguous write is never an ACK. Read committed storage again;
		// if it is unavailable, close the session rather than inventing a cursor.
		cursor, readErr := s.repo.GetCursor(ctx, session.ProbeID, session.StreamID)
		if readErr != nil || cursor < batch.FirstSeq-1 {
			return nil, err
		}
		// ErrReplayRetry is also returned when the cursor is still the pre-batch
		// position, so the caller can retry a rolled-back write. Page only once
		// the cursor covers this batch: that is the proof the prefix committed.
		if cursor >= batch.LastSeq {
			s.notifyRegionalEvidence(ctx, batch, nil)
		}
		return &domain.ProbeReplayResult{StreamID: batch.StreamID, CommittedSeq: min(cursor, batch.LastSeq)}, domain.ErrReplayRetry
	}
	if err != nil {
		if errors.Is(err, ports.ErrConflict) && ctx.Err() == nil {
			logRefusedBatch(session, batch, err)
		}
		return result, err
	}
	var rejected []domain.ProbeReplayRejection
	if result != nil {
		rejected = result.Rejected
	}
	logReusedSequences(session, batch, rejected)
	s.notifyRegionalEvidence(ctx, batch, rejected)
	return result, nil
}

// logReusedSequences reports events dropped because their sequence was already
// committed for a different event. The stream keeps flowing, but those
// observations are lost, so an operator has to hear about it (#192).
func logReusedSequences(session domain.ProbeReplaySession, batch domain.ProbeReplayBatch, rejected []domain.ProbeReplayRejection) {
	var count, first, last int64
	for _, rejection := range rejected {
		if rejection.Code != domain.ProbeReplaySequenceReused {
			continue
		}
		if count == 0 {
			first = rejection.Seq
		}
		last = rejection.Seq
		count++
	}
	if count == 0 {
		return
	}
	slog.Error("probe telemetry reused sequence numbers the hub already committed; those events were rejected and are not stored",
		"probe_id", session.ProbeID, "stream_id", session.StreamID,
		"batch_first_seq", batch.FirstSeq, "batch_last_seq", batch.LastSeq,
		"reused_count", count, "first_reused_seq", first, "last_reused_seq", last,
		"hint", "the probe data directory was likely restored from an older backup without a stream reset (prepare-reset / probe reset-stream / activate-reset)")
}

// logRefusedBatch makes a refused batch visible: the session closes and the
// probe reconnects, which otherwise only shows up as a Disconnected probe. It
// deliberately reads nothing more; a refused batch never earns a retry.
func logRefusedBatch(session domain.ProbeReplaySession, batch domain.ProbeReplayBatch, err error) {
	slog.Warn("probe telemetry batch refused; the session will close and the probe will reconnect",
		"probe_id", session.ProbeID, "stream_id", session.StreamID,
		"first_seq", batch.FirstSeq, "last_seq", batch.LastSeq, "error", err)
}

func (s *ProbeReplayService) notifyRegionalEvidence(ctx context.Context, batch domain.ProbeReplayBatch, rejected []domain.ProbeReplayRejection) {
	if s == nil {
		return
	}
	ids := acceptedObservationMonitors(batch, rejected)
	if len(ids) == 0 {
		return
	}
	s.recovery.resolve(ctx, ids)
	if s.browser != nil {
		s.browser.OnRegionalEvidence(ctx, ids)
	}
	if s.groupAlerts != nil {
		s.groupAlerts.OnRegionalEvidence(ctx, ids)
	}
}

// acceptedObservationMonitors is the set of monitors whose observation was not
// permanently rejected. Delivery, incident and condition events do not change
// the availability evidence a folder rolls up.
func acceptedObservationMonitors(batch domain.ProbeReplayBatch, rejected []domain.ProbeReplayRejection) []int64 {
	skip := make(map[int64]bool, len(rejected))
	for _, rejection := range rejected {
		skip[rejection.Seq] = true
	}
	seen := make(map[int64]bool)
	ids := make([]int64, 0, len(batch.Events))
	for _, event := range batch.Events {
		if event.Kind != domain.ReplayKindObservation || event.Observation == nil || skip[event.Seq] {
			continue
		}
		id := event.Observation.MonitorID
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
