package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Retain through the maximum configurable telemetry horizon, even if today's
// configured horizon is shorter. Raising retention cannot invalidate a receipt.
const commandRetention = 365 * 24 * time.Hour
const maxAppliedCommands = 16384

var _ ports.EdgeCommandRepository = (*Store)(nil)

type edgeCommandRow struct {
	bun.BaseModel `bun:"table:edge_applied_commands"`
	CommandID     string `bun:"command_id,pk"`
	Kind          string
	RequestHash   []byte
	Status        string
	AppliedAt     *int64
	Code          string
	Message       string
	RetainUntil   int64
}

func (r edgeCommandRow) outcome() domain.ProbeCommandOutcome {
	return domain.ProbeCommandOutcome{CommandID: r.CommandID, Status: r.Status, AppliedAt: timeFromMicro(r.AppliedAt), Code: r.Code, Message: r.Message}
}

// ApplyAlertAcknowledgement binds the source effect, one transition and its
// immutable receipt in a single SQLite transaction. Session fencing precedes
// duplicate lookup; expiry follows it, allowing receipt recovery after expiry.
func (s *Store) ApplyAlertAcknowledgement(ctx context.Context, authority domain.EdgeCommandAuthority, command domain.ProbeAlertAcknowledgement) (domain.ProbeCommandOutcome, error) {
	if s.telemetry == nil || !validEdgeAcknowledgement(authority, command) {
		return domain.ProbeCommandOutcome{}, domain.ErrValidation
	}
	digest := acknowledgementHash(authority, command)
	var outcome domain.ProbeCommandOutcome
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity) error {
		if authority.HubID != identity.HubID || authority.ProbeID != identity.ProbeID || authority.StreamID != identity.StreamID || authority.ConnectionGeneration != identity.ConnectionGeneration || command.ProbeID != identity.ProbeID {
			return ports.ErrConflict
		}
		var prior edgeCommandRow
		err := tx.NewSelect().Model(&prior).Where("command_id = ?", command.CommandID).Scan(ctx)
		if err == nil {
			if prior.Kind != "alert.ack" || !bytes.Equal(prior.RequestHash, digest[:]) {
				return ports.ErrConflict
			}
			outcome = prior.outcome()
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := s.commandNow().UTC().Truncate(time.Microsecond)
		// Bound retained receipts independently of telemetry. Never evict a live
		// receipt to make a new command look successful.
		if _, err := tx.ExecContext(ctx, "DELETE FROM edge_applied_commands WHERE retain_until < ?", now.UnixMicro()); err != nil {
			return err
		}
		count, err := tx.NewSelect().Model((*edgeCommandRow)(nil)).Count(ctx)
		if err != nil {
			return err
		}
		if count >= maxAppliedCommands {
			return ErrQueueFull
		}
		row := edgeCommandRow{CommandID: command.CommandID, Kind: "alert.ack", RequestHash: digest[:], RetainUntil: command.ExpiresAt.Add(commandRetention).UTC().UnixMicro()}
		switch {
		case !now.Before(command.ExpiresAt):
			row.Status, row.Code, row.Message = "expired", "command_expired", "Command expired before application"
		case command.CreatedAt.After(now.Add(30 * time.Second)):
			row.Status, row.Code, row.Message = "rejected", "command_from_future", "Command creation time is too far ahead"
		default:
			if err := s.acknowledgeIncident(ctx, tx, identity, command, now, &row); err != nil {
				return err
			}
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		outcome = row.outcome()
		return nil
	})
	if err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	return outcome, nil
}

func (s *Store) acknowledgeIncident(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity, command domain.ProbeAlertAcknowledgement, now time.Time, result *edgeCommandRow) error {
	var incident edgeIncidentRow
	err := tx.NewSelect().Model(&incident).Where("source_alert_id = ? AND generation = ? AND scope = ? AND subject_kind = ?", command.SourceAlertID, command.AssignmentGeneration, domain.IncidentScopeRegional, domain.IncidentSubjectAvailability).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		result.Status, result.Code, result.Message = "rejected", "target_not_found", "Command target was not found"
		return nil
	}
	if err != nil {
		return err
	}
	result.AppliedAt = microFromTime(&now)
	switch incident.Status {
	case domain.AlertStatusResolved:
		result.Status, result.Message = "already_resolved", "Incident was already resolved"
		return nil
	case domain.AlertStatusAcked:
		result.Status, result.Message = "already_applied", "Incident was already acknowledged"
		return nil
	case domain.AlertStatusFiring:
		if incident.TransitionVersion == math.MaxInt64 || identity.LastCreatedSeq == math.MaxInt64 {
			return ports.ErrConflict
		}
	default:
		return ports.ErrConflict
	}
	incident.Status, incident.AckedAt = domain.AlertStatusAcked, microFromTime(&now)
	incident.AckCommandID, incident.AckActorDisplayName, incident.AckNote = command.CommandID, command.ActorDisplayName, command.Note
	incident.TransitionVersion++
	if _, err := tx.NewUpdate().Model(&incident).WherePK().Exec(ctx); err != nil {
		return err
	}
	seq := identity.LastCreatedSeq + 1
	payload, err := s.telemetry.EncodeIncident(seq, now, *incident.incident(identity.ProbeID))
	if err != nil {
		return domain.ErrValidation
	}
	if err := s.appendTelemetry(ctx, tx, seq, "alert.transition", now, payload); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE edge_identity SET last_created_seq = ? WHERE id = 1", seq); err != nil {
		return err
	}
	// Pending DOWN intents are reconciled by the ordinary delivery worker into
	// durable superseded outcomes. Do not discard its leases or reserved space.
	result.Status, result.Message = "applied", "Incident acknowledged"
	return nil
}

func validEdgeAcknowledgement(a domain.EdgeCommandAuthority, c domain.ProbeAlertAcknowledgement) bool {
	return domain.ValidHubID(a.HubID) && domain.ValidHubID(a.ProbeID) && domain.ValidHubID(a.StreamID) && a.ConnectionGeneration > 0 &&
		domain.ValidHubID(c.CommandID) && domain.ValidHubID(c.ProbeID) && domain.ValidHubID(c.SourceAlertID) && c.AssignmentGeneration > 0 &&
		!c.CreatedAt.IsZero() && c.ExpiresAt.After(c.CreatedAt) && c.ExpiresAt.Sub(c.CreatedAt) <= 7*24*time.Hour &&
		c.PayloadHash != [32]byte{} && strings.TrimSpace(c.ActorDisplayName) != "" && len(c.ActorDisplayName) <= 256 && utf8.ValidString(c.ActorDisplayName) &&
		(c.Note == nil || len(*c.Note) <= 4096 && utf8.ValidString(*c.Note))
}

// Bind exact wire bytes AND decoded values so accidental misuse of the port
// cannot reuse a digest with a different effect. Length prefixes prevent field
// boundary ambiguity; the transient connection generation is deliberately absent.
func acknowledgementHash(a domain.EdgeCommandAuthority, c domain.ProbeAlertAcknowledgement) [32]byte {
	b := append([]byte("phoenix.alert.ack.v1"), c.PayloadHash[:]...)
	for _, value := range []string{a.HubID, a.ProbeID, a.StreamID, c.CommandID, c.ProbeID, c.SourceAlertID, strconv.FormatInt(c.AssignmentGeneration, 10), c.CreatedAt.UTC().Format(time.RFC3339Nano), c.ExpiresAt.UTC().Format(time.RFC3339Nano), c.ActorDisplayName} {
		b = binary.BigEndian.AppendUint64(b, uint64(len(value)))
		b = append(b, value...)
	}
	if c.Note == nil {
		b = append(b, 0)
	} else {
		b = append(b, 1)
		b = append(b, *c.Note...)
	}
	return sha256.Sum256(b)
}
