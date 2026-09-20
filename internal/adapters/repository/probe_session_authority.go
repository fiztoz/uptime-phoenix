package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type replaySessionAuthority struct {
	stream probeStreamModel
	lease  probeSessionModel
	now    time.Time
}

// lockReplaySession shares the exact DB-clock authority fence with current-state
// application. Its caller holds a serializable transaction through final commit.
func (s *ProbeReplayStore) lockReplaySession(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession) (replaySessionAuthority, error) {
	if _, err := tx.ExecContext(ctx, "UPDATE probes SET id = id WHERE id = ?", session.ProbeID); err != nil {
		return replaySessionAuthority{}, err
	}
	var registration probeRegistrationModel
	if err := tx.NewSelect().Model(&registration).Where("id = ?", session.ProbeID).Scan(ctx); err != nil {
		return replaySessionAuthority{}, err
	}
	if !registration.Enabled || registration.Kind != domain.ProbeKindRemote {
		return replaySessionAuthority{}, ports.ErrConflict
	}
	now, err := replayDatabaseTime(ctx, tx)
	if err != nil {
		return replaySessionAuthority{}, err
	}
	lease, err := readProbeSession(ctx, tx, session.ProbeID)
	if err != nil {
		return replaySessionAuthority{}, err
	}
	if lease.OwnerID != session.OwnerID || lease.Generation != session.ConnectionGeneration || lease.LeaseUntil <= now.Unix() {
		return replaySessionAuthority{}, ports.ErrConflict
	}
	var connection probeConnectionRow
	if err := tx.NewSelect().Model(&connection).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
		return replaySessionAuthority{}, err
	}
	if connection.HubID != session.HubID || connection.StreamID != session.StreamID {
		return replaySessionAuthority{}, ports.ErrConflict
	}
	var installation probeInstallationModel
	if err := tx.NewSelect().Model(&installation).Where("id = 1").Scan(ctx); err != nil {
		return replaySessionAuthority{}, err
	}
	if installation.HubID != session.HubID || installation.KeyHash != s.protector.KeyHash(session.HubID) {
		return replaySessionAuthority{}, domain.ErrProbeKeyMismatch
	}
	var stream probeStreamModel
	if err := tx.NewSelect().Model(&stream).Where("probe_id = ? AND stream_id = ?", session.ProbeID, session.StreamID).Scan(ctx); err != nil {
		return replaySessionAuthority{}, err
	}
	if stream.RetiredAt != nil {
		return replaySessionAuthority{}, ports.ErrConflict
	}
	return replaySessionAuthority{stream: stream, lease: lease, now: now}, nil
}
