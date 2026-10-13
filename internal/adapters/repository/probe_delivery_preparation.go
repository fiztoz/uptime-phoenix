package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Delivery preparation reuses the existing probe_commands ledger and its
// protection, dispatch and result machinery. A delivery.prepare row is a
// durable REQUEST only: recording one never asserts mode application,
// suppression, send authority, source-clock authority or a cutover barrier.

// deliveryPrepareKind is the single new probe_commands kind added by delivery
// preparation. The hub schema stores kinds without a CHECK, so no migration is
// required to reuse it.
const deliveryPrepareKind = "delivery.prepare"

// deliveryPreparedResultCode is the only success code a preparation receipt may
// carry: the PREPARATION command was recorded, nothing stronger. It is never
// credential, certificate or effective application evidence.
const deliveryPreparedResultCode = "delivery_prepared"

// deliveryPrepareLifetime is the fixed lifetime of a DB-clock-created request.
const deliveryPrepareLifetime = 24 * time.Hour

var _ ports.RemoteDeliveryPreparationIssuer = (*RemoteProbeConfigSyncStore)(nil)

// IssueRemoteDeliveryPreparations records immutable preparation requests for
// members whose CURRENT desired set still matches the exact stored regional
// report, the committed active pointer and receipt, and the authenticated
// retained document, all under live session/lease/installation authority.
// A stale historical report or a member changed by an assignment ABA is still a
// legitimate report: it issues nothing and never fails the report session.
// Duplicates in one immutable shared round converge on the tuple/nonce-bound
// command ID. Expired/current-failed rounds rearm with new IDs, preserving old
// protected bytes, creation time and expiry.
func (s *RemoteProbeConfigSyncStore) IssueRemoteDeliveryPreparations(ctx context.Context, session domain.ProbeReplaySession, facts []domain.ProbeDeliveryModeApplication) error {
	if s == nil || s.db == nil || s.decoder == nil || s.protector == nil || s.preparationCodec == nil || s.commandProtector == nil ||
		len(facts) == 0 || len(facts) > 256 || !validCommandSession(session) || session.StreamID == domain.LocalStreamID {
		return domain.ErrValidation
	}
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		_, err := lockProbeSession(ctx, tx, session, s.commandProtector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		target := domain.ProbeConfigTarget{HubID: session.HubID, ProbeID: session.ProbeID}
		document := facts[0]
		if document.ProbeConfigTarget != target || document.StreamID != session.StreamID || document.ConfigRevision <= 0 || !domain.ValidKeyHash(document.SHA256) {
			return domain.ErrValidation
		}
		members, stale, err := s.preparationDocument(ctx, tx, session, document)
		if err != nil {
			return err
		}
		if stale {
			return nil // A newer document is active: historical reports issue nothing.
		}
		seen := make(map[int64]bool, len(facts))
		var roundDeadlines []time.Time
		for _, fact := range facts {
			if seen[fact.MonitorID] || !domain.ValidProbeDeliveryModeApplication(fact) || fact.AlertDelivery != domain.AlertDeliveryRegional || fact.ProbeConfigTarget != target ||
				fact.StreamID != session.StreamID || fact.ConfigRevision != document.ConfigRevision || fact.SHA256 != document.SHA256 {
				return ports.ErrConflict
			}
			seen[fact.MonitorID] = true
			var stored remoteRegionalApplicationModel
			err := tx.NewSelect().Model(&stored).Where("probe_id = ? AND config_revision = ? AND monitor_id = ? AND stream_id = ?", fact.ProbeID, fact.ConfigRevision, fact.MonitorID, fact.StreamID).Scan(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound // Negotiation order is never a stored report.
			}
			if err != nil {
				return err
			}
			if stored.AssignmentRevision != fact.AssignmentRevision || stored.Generation != fact.Generation || stored.HubID != fact.HubID ||
				stored.SHA256 != fact.SHA256 || stored.AlertDelivery != fact.AlertDelivery || stored.ConnectionGeneration <= 0 || stored.RecordedAt.IsZero() {
				return ports.ErrConflict
			}
			member, exists := members[fact.MonitorID]
			if !exists || member.AssignmentRevision != fact.AssignmentRevision || member.Generation != fact.Generation {
				return ports.ErrConflict // A report can never contradict its own document.
			}
			mode := member.DesiredAlertDelivery
			if mode != domain.AlertDeliveryAggregate && mode != domain.AlertDeliveryBoth {
				continue // Regional members never receive a preparation request.
			}
			set, err := readProbeAssignments(ctx, tx, fact.MonitorID)
			if errors.Is(err, ports.ErrNotFound) {
				continue // The member left the current set after an ABA.
			}
			if err != nil {
				return err
			}
			if !currentSetMatches(set, fact, member) {
				continue // Stale after an ABA: issues nothing and keeps the session.
			}
			now, err := replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			round, err := selectPreparationRoundTx(ctx, tx, session.HubID, fact.MonitorID, fact.AssignmentRevision, mode, now)
			if err != nil {
				return err
			}
			if err := s.issuePreparation(ctx, tx, now, session, fact, mode, round); err != nil {
				return err
			}
			roundDeadlines = append(roundDeadlines, round.ExpiresAt.UTC())
		}
		current, err := readProbeSession(ctx, tx, session.ProbeID)
		if err != nil {
			return err
		}
		finalNow, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if current.OwnerID != session.OwnerID || current.Generation != session.ConnectionGeneration || current.LeaseUntil <= finalNow.Unix() {
			return ports.ErrConflict
		}
		for _, deadline := range roundDeadlines {
			if !finalNow.Before(deadline) {
				return ports.ErrConflict
			}
		}
		return nil
	})
	return remoteSyncError(ctx, err)
}

// preparationDocument independently verifies the committed active pointer,
// durable receipt and authenticated retained document for the report's exact
// document and returns its immutable members. A newer active revision makes the
// report historical (stale), which is never an error; corruption and internal
// incoherence always are.
func (s *RemoteProbeConfigSyncStore) preparationDocument(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession, document domain.ProbeDeliveryModeApplication) (map[int64]domain.EdgeResolvedAssignment, bool, error) {
	target := domain.ProbeConfigTarget{HubID: session.HubID, ProbeID: session.ProbeID}
	var active probeActiveConfigModel
	if err := tx.NewSelect().Model(&active).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
		return nil, false, err
	}
	if active.Revision > document.ConfigRevision {
		return nil, true, nil
	}
	if !domain.ValidProbeActiveConfig(domain.ProbeActiveConfig{ProbeConfigTarget: target, Revision: active.Revision, SHA256: active.SHA256, AppliedAt: active.AppliedAt, AssignmentCount: active.AssignmentCount}) ||
		active.HubID != session.HubID || active.Revision != document.ConfigRevision || active.SHA256 != document.SHA256 {
		return nil, false, ports.ErrConflict
	}
	var receipt probeConfigAppliedReceiptModel
	if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND revision = ?", session.ProbeID, document.ConfigRevision).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ports.ErrConflict // Queue order never proves a committed receipt.
		}
		return nil, false, err
	}
	var snapshot probeConfigModel
	if err := tx.NewSelect().Model(&snapshot).Where("probe_id = ? AND revision = ?", session.ProbeID, document.ConfigRevision).Scan(ctx); err != nil {
		return nil, false, err
	}
	if receipt.HubID != active.HubID || receipt.SHA256 != active.SHA256 || receipt.AssignmentCount != active.AssignmentCount ||
		!receipt.AppliedAt.Equal(active.AppliedAt) || snapshot.HubID != active.HubID || snapshot.SHA256 != active.SHA256 {
		return nil, false, ports.ErrConflict
	}
	plain, err := s.protector.Open(ctx, snapshot.domain().ProbeConfigMetadata, snapshot.ProtectedPayload)
	if err != nil {
		return nil, false, err
	}
	defer clear(plain)
	resolved, err := s.decoder.DecodeEdge(ctx, plain, target)
	if err != nil || resolved == nil || !domain.SameProbeConfigMetadata(resolved.Metadata, snapshot.domain().ProbeConfigMetadata) || len(resolved.Assignments) != active.AssignmentCount {
		return nil, false, ports.ErrConflict // Semantic failures are redacted, never repaired.
	}
	members := make(map[int64]domain.EdgeResolvedAssignment, len(resolved.Assignments))
	for _, assignment := range resolved.Assignments {
		if assignment.Monitor == nil {
			return nil, false, ports.ErrConflict
		}
		if _, duplicate := members[assignment.Monitor.ID]; duplicate {
			return nil, false, ports.ErrConflict
		}
		members[assignment.Monitor.ID] = assignment
	}
	return members, false, nil
}

// currentSetMatches reports whether the CURRENT desired set still binds exactly
// the immutable document member. A changed set or member after an ABA is
// staleness: it can neither issue a request nor invalidate a stored report.
func currentSetMatches(set *domain.MonitorProbeAssignments, fact domain.ProbeDeliveryModeApplication, member domain.EdgeResolvedAssignment) bool {
	if set == nil || set.Revision != fact.AssignmentRevision || set.AlertDelivery != member.DesiredAlertDelivery {
		return false
	}
	for _, assignment := range set.Assignments {
		if assignment.ProbeID == fact.ProbeID {
			return assignment.Generation == fact.Generation
		}
	}
	return false
}

// issuePreparation records one immutable round-bound request. The command ID
// binds the exact semantic tuple and retained round nonce, so a reconnect authenticates and
// keeps the original protected bytes, creation time and expiry instead of
// rewriting them. Nothing here enriches the request from mutable current rows.
func (s *RemoteProbeConfigSyncStore) issuePreparation(ctx context.Context, tx bun.Tx, now time.Time, session domain.ProbeReplaySession, fact domain.ProbeDeliveryModeApplication, mode domain.AlertDelivery, round preparationRoundModel) error {
	created := now.UTC().Truncate(time.Microsecond)
	bound := domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: session.HubID, ProbeID: session.ProbeID},
		MonitorID:          fact.MonitorID,
		AssignmentRevision: fact.AssignmentRevision,
		Generation:         fact.Generation,
		ConfigRevision:     fact.ConfigRevision,
		SHA256:             fact.SHA256,
		StreamID:           session.StreamID,
		AlertDelivery:      mode,
	}
	request := domain.ProbeDeliveryPreparation{
		ProbeDeliveryModeApplication: bound,
		CommandID:                    preparationRoundRequestID(bound, round),
		CreatedAt:                    created,
		ExpiresAt:                    round.ExpiresAt.UTC(),
	}
	payload, err := s.preparationCodec.EncodeDeliveryPreparation(ctx, request)
	if err != nil {
		return err
	}
	decoded, err := s.preparationCodec.DecodeDeliveryPreparation(ctx, payload)
	if err != nil || !sameDeliveryPreparationRequest(decoded, request) {
		clear(payload)
		return domain.ErrValidation
	}
	request.PayloadHash = decoded.PayloadHash
	generation := request.Generation
	meta := domain.ProbeCommandMetadata{
		CommandID:            request.CommandID,
		HubID:                bound.HubID,
		ProbeID:              bound.ProbeID,
		StreamID:             bound.StreamID,
		Kind:                 deliveryPrepareKind,
		AssignmentGeneration: &generation,
		CreatedAt:            request.CreatedAt,
		ExpiresAt:            request.ExpiresAt,
		PayloadSHA256:        hex.EncodeToString(request.PayloadHash[:]),
	}
	if !domain.ValidProbeCommandMetadata(meta) || !domain.ValidProbeDeliveryPreparation(request) {
		clear(payload)
		return domain.ErrValidation
	}
	protected, err := s.commandProtector.SealCommand(ctx, meta, payload)
	clear(payload)
	if err != nil {
		return err
	}
	var existing probeCommandRow
	err = tx.NewSelect().Model(&existing).Where("command_id = ?", request.CommandID).Scan(ctx)
	if err == nil {
		if err := s.authenticateExistingPreparation(ctx, existing, request); err != nil {
			return err
		}
		return writePreparationRoundRequestTx(ctx, tx, round, bound, request.CommandID, now, true, false)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := reserveCommandCapacity(ctx, tx, bound.ProbeID, now, 1, len(protected)); err != nil {
		return err
	}
	row := newPendingCommandRow(domain.ProtectedProbeCommand{ProbeCommandMetadata: meta, ProtectedPayload: protected, RequestedBy: 0}, now)
	if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
		return err
	}
	return writePreparationRoundRequestTx(ctx, tx, round, bound, request.CommandID, now, true, true)
}

// authenticateExistingPreparation re-authenticates a stored request instead of
// rewriting it. Protected bytes, creation time, expiry and provenance stay
// immutable across reconnects and duplicates; only the exact semantic tuple and
// payload/row coherence are compared.
func (s *RemoteProbeConfigSyncStore) authenticateExistingPreparation(ctx context.Context, stored probeCommandRow, want domain.ProbeDeliveryPreparation) error {
	if stored.Kind != deliveryPrepareKind || stored.RequestedBy != 0 || stored.SourceAlertID != nil ||
		stored.AssignmentGeneration == nil || *stored.AssignmentGeneration != want.Generation ||
		stored.HubID != want.HubID || stored.ProbeID != want.ProbeID || stored.StreamID != want.StreamID {
		return ports.ErrConflict
	}
	plain, err := s.commandProtector.OpenCommand(ctx, stored.metadata(), stored.ProtectedPayload)
	if err != nil {
		return err
	}
	defer clear(plain)
	got, err := s.preparationCodec.DecodeDeliveryPreparation(ctx, plain)
	if err != nil {
		return err
	}
	if !sameDeliveryPreparationTuple(got, want) || !got.CreatedAt.Equal(stored.CreatedAt.UTC()) || !got.ExpiresAt.Equal(stored.ExpiresAt.UTC()) ||
		hex.EncodeToString(got.PayloadHash[:]) != stored.PayloadSHA256 || !domain.ValidProbeDeliveryPreparation(got) {
		return ports.ErrConflict
	}
	return nil
}

// sameDeliveryPreparationTuple compares the exact semantic request identity,
// never the caller's timestamps or payload hash.
func sameDeliveryPreparationTuple(a, b domain.ProbeDeliveryPreparation) bool {
	return a.CommandID == b.CommandID && a.ProbeDeliveryModeApplication == b.ProbeDeliveryModeApplication
}

// sameDeliveryPreparationRequest compares one full request binding, including
// its immutable timestamps, but not the payload hash.
func sameDeliveryPreparationRequest(a, b domain.ProbeDeliveryPreparation) bool {
	return sameDeliveryPreparationTuple(a, b) && a.CreatedAt.Equal(b.CreatedAt) && a.ExpiresAt.Equal(b.ExpiresAt)
}

// deliveryPreparationCommandID derives the legacy tuple identity, also used as
// the semantic component of a new nonce-bound round request ID. It binds the
// exact tuple (hub, probe, monitor, assignment-set revision, generation,
// document revision, document hash, stream, mode). Reconnects and duplicates
// converge on one immutable row without any fabricated operator input.
func deliveryPreparationCommandID(bound domain.ProbeDeliveryModeApplication) string {
	name := make([]byte, 0, 256)
	name = append(name, "uptime-phoenix/delivery.prepare"...)
	name = append(name, 0)
	for _, part := range []string{
		bound.HubID, bound.ProbeID,
		strconv.FormatInt(bound.MonitorID, 10), strconv.FormatInt(bound.AssignmentRevision, 10),
		strconv.FormatInt(bound.Generation, 10), strconv.FormatInt(bound.ConfigRevision, 10),
		bound.SHA256, bound.StreamID, string(bound.AlertDelivery),
	} {
		name = append(name, part...)
		name = append(name, 0)
	}
	sum := sha256.Sum256(name)
	var id [16]byte
	copy(id[:], sum[:])
	id[6] = (id[6] & 0x0f) | 0x80 // Deterministic (RFC 9562 version 8) UUID.
	id[8] = (id[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(id[:])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// validDeliveryPreparationOutcome accepts only the bounded nonsecret receipt of
// a preparation request. Success records exactly the delivery_prepared code and
// can never carry credential, certificate or stronger application facts.
func validDeliveryPreparationOutcome(r domain.ProbeCommandOutcome) bool {
	if r.CredentialVersion != 0 || r.CertificateVersion != 0 || r.CertificateFingerprint != "" || r.CertificateNotAfter != nil {
		return false
	}
	if !domain.ValidHubID(r.CommandID) || len(r.Message) > 4096 || len(r.Code) > 128 {
		return false
	}
	switch r.Status {
	case "applied", "already_applied":
		return r.AppliedAt != nil && !r.AppliedAt.IsZero() && r.Code == deliveryPreparedResultCode
	case "rejected":
		return r.AppliedAt == nil && r.Code != "" && r.Code != deliveryPreparedResultCode
	case "expired":
		return r.AppliedAt == nil
	default:
		return false
	}
}
