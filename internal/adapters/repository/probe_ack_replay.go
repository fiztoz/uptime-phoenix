package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func (s *ProbeReplayStore) issuedAcknowledgement(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession, commandID string) (*domain.ProbeAlertAcknowledgement, error) {
	if s.commandProtector == nil || s.commandCodec == nil {
		return nil, nil
	}
	if s.commandProtector.KeyHash(session.HubID) != s.protector.KeyHash(session.HubID) {
		return nil, domain.ErrProbeKeyMismatch
	}
	var row probeCommandRow
	err := tx.NewSelect().Model(&row).Where("command_id = ? AND hub_id = ? AND probe_id = ? AND stream_id = ? AND kind = ?", commandID, session.HubID, session.ProbeID, session.StreamID, "alert.ack").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Control result and telemetry travel independently. A pending dispatched
	// request can authorize its ACK; an unsent or contradictory outcome cannot.
	if row.Attempts < 1 || row.RemoteConfirmed && row.Status != "applied" {
		return nil, nil
	}
	plain, err := s.commandProtector.OpenCommand(ctx, row.metadata(), row.ProtectedPayload)
	if err != nil {
		return nil, err
	}
	ack, err := s.commandCodec.DecodeAcknowledgement(ctx, plain)
	clear(plain)
	if err != nil || !acknowledgementMatchesMetadata(ack, row.metadata()) {
		return nil, domain.ErrValidation
	}
	return &ack, nil
}
