package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"reflect"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.EdgeDeliveryPreparationRepository = (*Store)(nil)

type deliveryPreparationRow struct {
	bun.BaseModel      `bun:"table:edge_delivery_preparations"`
	CommandID          string `bun:",pk"`
	HubID              string
	ProbeID            string
	StreamID           string
	MonitorID          int64
	AssignmentRevision int64
	Generation         int64
	ConfigRevision     int64
	SHA256             string
	AlertDelivery      domain.AlertDelivery
	PreparedAt         int64
}

// ReadDeliveryPreparationReceipt recovers an exact immutable request under the
// current accepted session. It never treats historical preparation as eligibility.
func (s *Store) ReadDeliveryPreparationReceipt(ctx context.Context, authority domain.EdgeCommandAuthority, command domain.ProbeDeliveryPreparation) (*domain.ProbeCommandOutcome, error) {
	if !validDeliveryPreparationAuthority(authority, command) {
		return nil, domain.ErrValidation
	}
	digest := deliveryPreparationHash(authority, command)
	var out *domain.ProbeCommandOutcome
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity) error {
		if !deliveryPreparationAuthorityMatches(authority, identity) {
			return ports.ErrConflict
		}
		var row edgeCommandRow
		err := tx.NewSelect().Model(&row).Where("command_id = ?", command.CommandID).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Kind != "delivery.prepare" || !bytes.Equal(row.RequestHash, digest[:]) {
			return ports.ErrConflict
		}
		if row.Status == "applied" || row.Status == "already_applied" {
			var prepared deliveryPreparationRow
			if err := tx.NewSelect().Model(&prepared).Where("command_id = ?", command.CommandID).Scan(ctx); err != nil {
				return ports.ErrConflict
			}
			if !samePreparedRequest(prepared, command) || row.Code != "delivery_prepared" || row.AppliedAt == nil || prepared.PreparedAt != *row.AppliedAt {
				return ports.ErrConflict
			}
		}
		result := row.outcome()
		out = &result
		return nil
	})
	return out, err
}

// PrepareDelivery atomically commits a pending request and its command receipt.
// It never changes effective regional facts, checks, telemetry or provider work.
func (s *Store) PrepareDelivery(ctx context.Context, authority domain.EdgeCommandAuthority, command domain.ProbeDeliveryPreparation, captured *domain.EdgeActiveConfig) (domain.ProbeCommandOutcome, error) {
	if !validDeliveryPreparationAuthority(authority, command) {
		return domain.ProbeCommandOutcome{}, domain.ErrValidation
	}
	digest := deliveryPreparationHash(authority, command)
	return s.applyCommand(ctx, authority, command.CommandID, command.ProbeID, "delivery.prepare", digest, command.CreatedAt, command.ExpiresAt,
		func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity, now time.Time, result *edgeCommandRow) error {
			reject := func() error {
				result.Status, result.Code, result.Message = "rejected", "delivery_binding_changed", "Delivery preparation binding is unavailable or stale"
				return nil
			}
			if captured == nil {
				return reject()
			}
			current, err := readActiveConfig(ctx, tx)
			if errors.Is(err, ports.ErrNotFound) {
				return reject()
			}
			if err != nil {
				return err
			}
			// The exact bytes authenticated by the service must still be selected. A
			// second read may compare but cannot replace that authenticated capture.
			if !reflect.DeepEqual(current, *captured) {
				return reject()
			}
			p := current.Snapshot
			if p.ProbeConfigTarget != command.ProbeConfigTarget || p.Revision != command.ConfigRevision || p.SHA256 != command.SHA256 || identity.StreamID != command.StreamID {
				return reject()
			}
			member, fact := false, false
			for _, a := range current.Assignments {
				if a.MonitorID == command.MonitorID && a.Generation == command.Generation {
					member = true
				}
			}
			for _, a := range current.RegionalDeliveryApplications {
				if a.ProbeConfigTarget == command.ProbeConfigTarget && a.MonitorID == command.MonitorID && a.Generation == command.Generation && a.AssignmentRevision == command.AssignmentRevision && a.ConfigRevision == command.ConfigRevision && a.SHA256 == command.SHA256 && a.StreamID == command.StreamID && a.AlertDelivery == domain.AlertDeliveryRegional {
					fact = true
				}
			}
			if !member || !fact {
				return reject()
			}
			row := deliveryPreparationRow{CommandID: command.CommandID, HubID: command.HubID, ProbeID: command.ProbeID, StreamID: command.StreamID, MonitorID: command.MonitorID, AssignmentRevision: command.AssignmentRevision, Generation: command.Generation, ConfigRevision: command.ConfigRevision, SHA256: command.SHA256, AlertDelivery: command.AlertDelivery, PreparedAt: now.UnixMicro()}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
			result.Status, result.Code, result.Message = "applied", "delivery_prepared", "Delivery preparation recorded; effective mode remains regional"
			result.AppliedAt = microFromTime(&now)
			return nil
		})
}

func validDeliveryPreparationAuthority(a domain.EdgeCommandAuthority, c domain.ProbeDeliveryPreparation) bool {
	return domain.ValidProbeDeliveryPreparation(c) && domain.ValidHubID(a.HubID) && domain.ValidHubID(a.ProbeID) && domain.ValidHubID(a.StreamID) && a.StreamID != domain.LocalStreamID && a.ConnectionGeneration > 0 && a.HubID == c.HubID && a.ProbeID == c.ProbeID && a.StreamID == c.StreamID
}
func deliveryPreparationAuthorityMatches(a domain.EdgeCommandAuthority, i domain.EdgeIdentity) bool {
	return a.HubID == i.HubID && a.ProbeID == i.ProbeID && a.StreamID == i.StreamID && a.ConnectionGeneration == i.ConnectionGeneration
}
func samePreparedRequest(r deliveryPreparationRow, c domain.ProbeDeliveryPreparation) bool {
	return r.CommandID == c.CommandID && r.HubID == c.HubID && r.ProbeID == c.ProbeID && r.StreamID == c.StreamID && r.MonitorID == c.MonitorID && r.AssignmentRevision == c.AssignmentRevision && r.Generation == c.Generation && r.ConfigRevision == c.ConfigRevision && r.SHA256 == c.SHA256 && r.AlertDelivery == c.AlertDelivery && r.PreparedAt > 0
}
func deliveryPreparationHash(a domain.EdgeCommandAuthority, c domain.ProbeDeliveryPreparation) [32]byte {
	h := sha256.New()
	for _, v := range []string{a.HubID, a.ProbeID, a.StreamID, c.CommandID, strconv.FormatInt(c.MonitorID, 10), strconv.FormatInt(c.AssignmentRevision, 10), strconv.FormatInt(c.Generation, 10), strconv.FormatInt(c.ConfigRevision, 10), c.SHA256, string(c.AlertDelivery), strconv.FormatInt(c.CreatedAt.UnixMicro(), 10), strconv.FormatInt(c.ExpiresAt.UnixMicro(), 10)} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write([]byte(v))
	}
	h.Write(c.PayloadHash[:])
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
