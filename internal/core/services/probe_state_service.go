package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeStateService applies current evidence without using the replay cursor.
type ProbeStateService struct {
	repository  ports.ProbeStateRepository
	authorizer  ports.ProbeStateAuthorizer
	groupAlerts regionalGroupAlerter
	recovery    regionalRecovery
}

// NewProbeStateService binds the atomic current-state persistence boundary.
func NewProbeStateService(repository ports.ProbeStateRepository, authorizer ports.ProbeStateAuthorizer) (*ProbeStateService, error) {
	if repository == nil || authorizer == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeStateService{repository: repository, authorizer: authorizer}, nil
}

// SetGroupAlerter attaches hub-owned folder paging after a current snapshot
// commits. Optional, and never consulted when the snapshot is rejected.
func (s *ProbeStateService) SetGroupAlerter(alerter regionalGroupAlerter) {
	if s != nil {
		s.groupAlerts = alerter
	}
}

// SetStatusPageRecovery attaches post-commit recovery from fresh overall health.
func (s *ProbeStateService) SetStatusPageRecovery(overall AggregateStatusReader, resolver incidentAutoResolver) {
	s.recovery = regionalRecovery{overall: overall, resolver: resolver}
}

// ApplySnapshot validates structure; storage checks transaction-bound authority.
func (s *ProbeStateService) ApplySnapshot(ctx context.Context, session domain.ProbeReplaySession, snapshot domain.ProbeCurrentSnapshot) (*domain.ProbeStateReceipt, error) {
	if !domain.ValidProbeCurrentSnapshot(session, snapshot) {
		return nil, domain.ErrValidation
	}
	receipt, err := s.repository.ApplyCurrentSnapshot(ctx, session, snapshot, s.authorizer)
	if err != nil || receipt == nil || len(receipt.MonitorIDs) == 0 {
		return receipt, err
	}
	// MonitorIDs are the active assignments this snapshot reconciled, including
	// omissions. An identical retry leaves the list empty so it cannot page twice.
	s.recovery.resolve(ctx, receipt.MonitorIDs)
	if s.groupAlerts != nil {
		s.groupAlerts.OnRegionalEvidence(ctx, receipt.MonitorIDs)
	}
	return receipt, nil
}

// AuthorizeCurrentSnapshot authorizes every present entry against the exact
// applied config. Omitted active assignments are reconciled by the transaction.
func (s *AccessService) AuthorizeCurrentSnapshot(ctx context.Context, facts domain.ProbeStateAuthorityFacts, snapshot domain.ProbeCurrentSnapshot) bool {
	if ctx.Err() != nil || facts.ProbeID != snapshot.ProbeID || facts.StreamID != snapshot.StreamID || facts.ConfigRevision != snapshot.ConfigRevision {
		return false
	}
	assignments := make(map[int64]domain.EdgeAssignmentIdentity, len(facts.Assignments))
	for _, a := range facts.Assignments {
		assignments[a.MonitorID] = a
	}
	for _, state := range snapshot.States {
		a, ok := assignments[state.MonitorID]
		if !ok || !a.Active || a.Generation != state.AssignmentGeneration {
			return false
		}
	}
	return true
}
