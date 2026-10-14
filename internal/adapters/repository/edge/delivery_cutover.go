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

var _ ports.EdgeDeliveryCutoverRepository = (*Store)(nil)

// Monitor-wide source gate states. Only delivery.resume releases a gate; no
// timer, lease expiry, session change, configuration activation or reset may.
const (
	deliveryGateQuiescing = "quiescing"
	deliveryGateQuiesced  = "quiesced"
	deliveryGateApplied   = "applied"
	deliveryGateReleased  = "released"
)

type deliveryCutoverGateRow struct {
	bun.BaseModel        `bun:"table:edge_delivery_cutover_gates"`
	BarrierID            string `bun:",pk"`
	MonitorID            int64
	AssignmentRevision   int64
	Generation           int64
	ConfigRevision       int64
	SHA256               string
	StreamID             string
	AlertDelivery        domain.AlertDelivery
	PreparationCommandID string
	State                string
	EstablishedAt        int64
	QuiescedAt           *int64
	AppliedAt            *int64
	ReleasedAt           *int64
}

// sameCutoverTuple proves the command binds the exact stored barrier: identity,
// document, stream, desired mode and the immutable preparation it was built on.
// A mismatch is a replacement barrier and must never be released or advanced.
func sameCutoverTuple(row deliveryCutoverGateRow, c domain.DeliveryCutoverCommand) bool {
	return row.BarrierID == c.BarrierID && row.MonitorID == c.MonitorID &&
		row.AssignmentRevision == c.AssignmentRevision && row.Generation == c.Generation &&
		row.ConfigRevision == c.ConfigRevision && row.SHA256 == c.SHA256 && row.StreamID == c.StreamID &&
		row.AlertDelivery == c.AlertDelivery && row.PreparationCommandID == c.PreparationCommandID
}

// aggregateSourceHoldActive reports whether a live aggregate source hold
// suppresses new regional availability provider starts and intents for one
// monitor. A both-mode hold retains regional work and never suppresses; a
// released gate is historical only. Certificate, capacity and probe scopes are
// independent and are never consulted here.
func aggregateSourceHoldActive(ctx context.Context, db bun.IDB, monitorID int64) (bool, error) {
	if monitorID <= 0 {
		return false, domain.ErrValidation
	}
	return db.NewSelect().Model((*deliveryCutoverGateRow)(nil)).
		Where("monitor_id = ?", monitorID).
		Where("alert_delivery = ?", domain.AlertDeliveryAggregate).
		Where("state IN (?, ?, ?)", deliveryGateQuiescing, deliveryGateQuiesced, deliveryGateApplied).
		Exists(ctx)
}

// unresolvedProviderAttempts counts in_flight and ambiguous grants for a
// monitor. Only a finished, known provider outcome stops blocking; crashed,
// unknown or errored attempts block quiescence rather than fabricate completion.
func unresolvedProviderAttempts(ctx context.Context, db bun.IDB, monitorID int64) (int64, error) {
	return db.NewSelect().Model((*providerAttemptRow)(nil)).
		Where("monitor_id = ?", monitorID).
		Where("state IN (?, ?)", domain.ProviderAttemptInFlight, domain.ProviderAttemptAmbiguous).
		Count(ctx)
}

// hasExactPreparation proves one immutable successful delivery.prepare receipt
// binds this exact tuple. Recovery of a receipt is historical evidence here, a
// prerequisite read: it is never new eligibility.
func hasExactPreparation(ctx context.Context, db bun.IDB, c domain.DeliveryCutoverCommand) (bool, error) {
	var prepared deliveryPreparationRow
	err := db.NewSelect().Model(&prepared).Where("command_id = ?", c.PreparationCommandID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if prepared.HubID != c.HubID || prepared.ProbeID != c.ProbeID || prepared.StreamID != c.StreamID ||
		prepared.MonitorID != c.MonitorID || prepared.AssignmentRevision != c.AssignmentRevision ||
		prepared.Generation != c.Generation || prepared.ConfigRevision != c.ConfigRevision ||
		prepared.SHA256 != c.SHA256 || prepared.AlertDelivery != c.AlertDelivery || prepared.PreparedAt <= 0 {
		return false, nil
	}
	var receipt edgeCommandRow
	err = db.NewSelect().Model(&receipt).Where("command_id = ?", c.PreparationCommandID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if receipt.Kind != "delivery.prepare" || receipt.AppliedAt == nil || receipt.Code != "delivery_prepared" ||
		(receipt.Status != "applied" && receipt.Status != "already_applied") {
		return false, nil
	}
	return true, nil
}

// ReadDeliveryCutoverReceipt recovers one exact immutable historical outcome
// under the current accepted session before any new configuration load. A
// duplicate never re-derives eligibility from current mutable state.
func (s *Store) ReadDeliveryCutoverReceipt(ctx context.Context, authority domain.EdgeCommandAuthority, command domain.DeliveryCutoverCommand) (*domain.ProbeCommandOutcome, error) {
	if !validDeliveryCutoverAuthority(authority, command) {
		return nil, domain.ErrValidation
	}
	digest := deliveryCutoverHash(authority, command)
	var out *domain.ProbeCommandOutcome
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity) error {
		if !deliveryCutoverAuthorityMatches(authority, identity) {
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
		if row.Kind != command.Kind || !bytes.Equal(row.RequestHash, digest[:]) {
			return ports.ErrConflict
		}
		if row.Status == "applied" || row.Status == "already_applied" {
			// Successful kind receipts must keep their exact stored barrier.
			if row.Code != domain.DeliveryCutoverSuccessCode(command.Kind) || row.AppliedAt == nil {
				return ports.ErrConflict
			}
			var gate deliveryCutoverGateRow
			if err := tx.NewSelect().Model(&gate).Where("barrier_id = ?", command.BarrierID).Scan(ctx); err != nil {
				return ports.ErrConflict
			}
			if !sameCutoverTuple(gate, command) {
				return ports.ErrConflict
			}
		}
		result := row.outcome()
		out = &result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyDeliveryCutover atomically commits one protected source-gate effect and
// its immutable kind-specific receipt. A nil capture permits resume and receipt
// recovery only; it can never apply a new current configuration.
func (s *Store) ApplyDeliveryCutover(ctx context.Context, authority domain.EdgeCommandAuthority, command domain.DeliveryCutoverCommand, captured *domain.EdgeActiveConfig) (domain.ProbeCommandOutcome, error) {
	if !validDeliveryCutoverAuthority(authority, command) {
		return domain.ProbeCommandOutcome{}, domain.ErrValidation
	}
	digest := deliveryCutoverHash(authority, command)
	return s.applyCommand(ctx, authority, command.CommandID, command.ProbeID, command.Kind, digest, command.CreatedAt, command.ExpiresAt,
		func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity, now time.Time, result *edgeCommandRow) error {
			switch command.Kind {
			case domain.DeliveryQuiesceKind, domain.DeliveryApplyKind:
				return s.commitDeliveryGate(ctx, tx, identity, command, captured, now, result)
			default:
				return s.releaseDeliveryGate(ctx, tx, command, now, result)
			}
		})
}

// commitDeliveryGate carries the quiesce and apply effects. Both require the
// exact authenticated current capture, whole-config membership, the source-local
// regional fact and the immutable successful preparation receipt.
func (s *Store) commitDeliveryGate(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity, command domain.DeliveryCutoverCommand, captured *domain.EdgeActiveConfig, now time.Time, result *edgeCommandRow) error {
	reject := func(code, message string) error {
		result.Status, result.Code, result.Message = "rejected", code, message
		return nil
	}
	if captured == nil {
		return reject("delivery_binding_changed", "Delivery cutover binding is unavailable or stale")
	}
	current, err := readActiveConfig(ctx, tx)
	if errors.Is(err, ports.ErrNotFound) {
		return reject("delivery_binding_changed", "Delivery cutover binding is unavailable or stale")
	}
	if err != nil {
		return err
	}
	// The exact bytes authenticated by the service must still be selected. A
	// second read may compare but cannot replace that authenticated capture.
	if !reflect.DeepEqual(current, *captured) {
		return reject("delivery_binding_changed", "Delivery cutover binding is unavailable or stale")
	}
	p := current.Snapshot
	if p.ProbeConfigTarget != command.ProbeConfigTarget || p.Revision != command.ConfigRevision || p.SHA256 != command.SHA256 || identity.StreamID != command.StreamID {
		return reject("delivery_binding_changed", "Delivery cutover binding is unavailable or stale")
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
		return reject("delivery_binding_changed", "Delivery cutover binding is unavailable or stale")
	}
	prepared, err := hasExactPreparation(ctx, tx, command)
	if err != nil {
		return err
	}
	if !prepared {
		return reject("delivery_preparation_missing", "Exact successful delivery preparation is missing")
	}
	var gate deliveryCutoverGateRow
	err = tx.NewSelect().Model(&gate).Where("barrier_id = ?", command.BarrierID).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if command.Kind != domain.DeliveryQuiesceKind {
			return reject("delivery_gate_missing", "Delivery barrier gate is missing")
		}
		live, err := activeDeliveryGate(ctx, tx, command.MonitorID)
		if err != nil {
			return err
		}
		if live != nil {
			// A new barrier cannot bypass a gate or drain already in progress.
			return reject("delivery_barrier_conflict", "Another delivery barrier is live for this monitor")
		}
		gate = deliveryCutoverGateRow{BarrierID: command.BarrierID, MonitorID: command.MonitorID,
			AssignmentRevision: command.AssignmentRevision, Generation: command.Generation,
			ConfigRevision: command.ConfigRevision, SHA256: command.SHA256, StreamID: command.StreamID,
			AlertDelivery: command.AlertDelivery, PreparationCommandID: command.PreparationCommandID,
			State: deliveryGateQuiescing, EstablishedAt: now.UnixMicro()}
		if _, err := tx.NewInsert().Model(&gate).Exec(ctx); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if !sameCutoverTuple(gate, command) {
			return reject("delivery_barrier_mismatch", "Delivery barrier does not match the stored gate")
		}
		if gate.State == deliveryGateReleased {
			return reject("delivery_barrier_released", "Delivery barrier was already released")
		}
	}
	if command.Kind == domain.DeliveryQuiesceKind {
		if gate.State == deliveryGateQuiesced || gate.State == deliveryGateApplied {
			result.Status, result.Code, result.Message = "already_applied", domain.DeliveryQuiescedCode, "Source gate is already quiesced under this barrier"
			result.AppliedAt = microFromTime(&now)
			return nil
		}
		// The hold above persists first and outlives a busy refusal, so later
		// immutable commands can finish the same barrier honestly.
		unresolved, err := unresolvedProviderAttempts(ctx, tx, command.MonitorID)
		if err != nil {
			return err
		}
		if unresolved > 0 {
			return reject("delivery_provider_busy", "Unresolved provider attempts block quiescence")
		}
		gate.State, gate.QuiescedAt = deliveryGateQuiesced, microFromTime(&now)
		if _, err := tx.NewUpdate().Model(&gate).WherePK().Exec(ctx); err != nil {
			return err
		}
		result.Status, result.Code, result.Message = "applied", domain.DeliveryQuiescedCode, "New regional availability provider starts are quiesced under this barrier"
		result.AppliedAt = microFromTime(&now)
		return nil
	}
	// delivery.apply: genuine mode application on the exact quiesced gate.
	if gate.State == deliveryGateApplied {
		result.Status, result.Code, result.Message = "already_applied", domain.DeliveryAppliedCode, "Source gate mode application is already committed"
		result.AppliedAt = microFromTime(&now)
		return nil
	}
	if gate.State != deliveryGateQuiesced {
		return reject("delivery_gate_not_ready", "Delivery barrier is not quiesced")
	}
	gate.State, gate.AppliedAt = deliveryGateApplied, microFromTime(&now)
	if _, err := tx.NewUpdate().Model(&gate).WherePK().Exec(ctx); err != nil {
		return err
	}
	result.Status, result.Code, result.Message = "applied", domain.DeliveryAppliedCode, "Source gate mode application committed"
	result.AppliedAt = microFromTime(&now)
	return nil
}

// releaseDeliveryGate authenticates the exact stored barrier tuple and releases
// only that gate. It may legitimately release an old configuration after
// replacement, but never a replacement barrier, and never reads a config.
func (s *Store) releaseDeliveryGate(ctx context.Context, tx bun.Tx, command domain.DeliveryCutoverCommand, now time.Time, result *edgeCommandRow) error {
	reject := func(code, message string) error {
		result.Status, result.Code, result.Message = "rejected", code, message
		return nil
	}
	var gate deliveryCutoverGateRow
	err := tx.NewSelect().Model(&gate).Where("barrier_id = ?", command.BarrierID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		live, err := activeDeliveryGate(ctx, tx, command.MonitorID)
		if err != nil {
			return err
		}
		if live != nil {
			return reject("delivery_barrier_conflict", "Another delivery barrier is live for this monitor")
		}
		// Resume can overtake an unreceived quiesce. Its released tombstone
		// prevents a late old command from installing a hold after drain.
		released := now.UnixMicro()
		gate = deliveryCutoverGateRow{BarrierID: command.BarrierID, MonitorID: command.MonitorID,
			AssignmentRevision: command.AssignmentRevision, Generation: command.Generation,
			ConfigRevision: command.ConfigRevision, SHA256: command.SHA256, StreamID: command.StreamID,
			AlertDelivery: command.AlertDelivery, PreparationCommandID: command.PreparationCommandID,
			State: deliveryGateReleased, EstablishedAt: released, ReleasedAt: &released}
		if _, err := tx.NewInsert().Model(&gate).Exec(ctx); err != nil {
			return err
		}
		result.Status, result.Code, result.Message = "applied", domain.DeliveryResumedCode, "Unstarted delivery barrier released"
		result.AppliedAt = microFromTime(&now)
		return nil
	}
	if err != nil {
		return err
	}
	if !sameCutoverTuple(gate, command) {
		return reject("delivery_barrier_mismatch", "Delivery barrier does not match the stored gate")
	}
	if gate.State == deliveryGateReleased {
		result.Status, result.Code, result.Message = "already_applied", domain.DeliveryResumedCode, "Source gate is already released under this barrier"
		result.AppliedAt = microFromTime(&now)
		return nil
	}
	gate.State, gate.ReleasedAt = deliveryGateReleased, microFromTime(&now)
	if _, err := tx.NewUpdate().Model(&gate).WherePK().Exec(ctx); err != nil {
		return err
	}
	result.Status, result.Code, result.Message = "applied", domain.DeliveryResumedCode, "Source gate released; regional availability work may resume"
	result.AppliedAt = microFromTime(&now)
	return nil
}

// activeDeliveryGate returns the monitor's live gate, if any. Released gates are
// historical and never block a later barrier.
func activeDeliveryGate(ctx context.Context, tx bun.Tx, monitorID int64) (*deliveryCutoverGateRow, error) {
	var gate deliveryCutoverGateRow
	err := tx.NewSelect().Model(&gate).Where("monitor_id = ?", monitorID).Where("state <> ?", deliveryGateReleased).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &gate, nil
}

func validDeliveryCutoverAuthority(a domain.EdgeCommandAuthority, c domain.DeliveryCutoverCommand) bool {
	return domain.ValidDeliveryCutoverCommand(c) && domain.ValidHubID(a.HubID) && domain.ValidHubID(a.ProbeID) &&
		domain.ValidHubID(a.StreamID) && a.StreamID != domain.LocalStreamID && a.ConnectionGeneration > 0 &&
		a.HubID == c.HubID && a.ProbeID == c.ProbeID
}

func deliveryCutoverAuthorityMatches(a domain.EdgeCommandAuthority, i domain.EdgeIdentity) bool {
	return a.HubID == i.HubID && a.ProbeID == i.ProbeID && a.StreamID == i.StreamID && a.ConnectionGeneration == i.ConnectionGeneration
}

// deliveryCutoverHash binds exact wire bytes AND decoded values so accidental
// port misuse cannot reuse a digest with a different source-gate effect.
func deliveryCutoverHash(a domain.EdgeCommandAuthority, c domain.DeliveryCutoverCommand) [32]byte {
	h := sha256.New()
	h.Write([]byte("phoenix.delivery_cutover.v1"))
	h.Write(c.PayloadHash[:])
	for _, v := range []string{a.HubID, a.ProbeID, a.StreamID, c.CommandID, c.BarrierID, c.PreparationCommandID, c.Kind,
		c.ProbeID, strconv.FormatInt(c.MonitorID, 10), strconv.FormatInt(c.AssignmentRevision, 10),
		strconv.FormatInt(c.Generation, 10), strconv.FormatInt(c.ConfigRevision, 10), c.SHA256, c.StreamID,
		string(c.AlertDelivery), strconv.FormatInt(c.CreatedAt.UnixMicro(), 10), strconv.FormatInt(c.ExpiresAt.UnixMicro(), 10)} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write([]byte(v))
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
