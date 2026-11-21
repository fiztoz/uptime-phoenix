package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

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
//
// Overall dirty buckets reference the local registration, so marking one
// takes that row's shared lock through the FK check. Requested there, after
// the installation, assignment and history locks, it inverts the order of
// monitor create, local configuration activation and every other writer that
// locks the local registration first, and InnoDB kills one side with 1213.
// Take it up front instead, in probe-id order after this remote probe's row
// (canonical UUIDs sort before "local"), as assignment replacement does.
func (s *ProbeReplayStore) lockReplaySession(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession) (replaySessionAuthority, error) {
	if tx.Dialect().Name() == dialect.MySQL {
		if _, err := tx.ExecContext(ctx, "UPDATE probes SET id = id WHERE id = ?", session.ProbeID); err != nil {
			return replaySessionAuthority{}, err
		}
		var local string
		if err := tx.NewRaw("SELECT id FROM probes WHERE id = ? LOCK IN SHARE MODE", domain.LocalProbeID).Scan(ctx, &local); err != nil {
			return replaySessionAuthority{}, err
		}
	}
	return lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
}

// lockProbeSession is shared by replay and command writes; callers retain these
// locks through commit and check the DB clock again after their final write.
func lockProbeSession(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession, keyHash string) (replaySessionAuthority, error) {
	if _, err := tx.ExecContext(ctx, "UPDATE probes SET id = id WHERE id = ?", session.ProbeID); err != nil {
		return replaySessionAuthority{}, err
	}
	var registration probeRegistrationModel
	if err := tx.NewSelect().Model(&registration).Where("id = ?", session.ProbeID).Scan(ctx); err != nil {
		return replaySessionAuthority{}, err
	}
	if !registration.Enabled || registration.RevokedAt != nil || registration.DeletedAt != nil || registration.Kind != domain.ProbeKindRemote {
		return replaySessionAuthority{}, ports.ErrConflict
	}
	if err := requireNoStreamReset(ctx, tx, session.ProbeID, false); err != nil {
		return replaySessionAuthority{}, err
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
	if installation.HubID != session.HubID || installation.KeyHash != keyHash {
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
