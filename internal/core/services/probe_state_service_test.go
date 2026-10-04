package services_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type fakeStateRepo struct {
	receipt *domain.ProbeStateReceipt
	err     error
	calls   int
}

func (f *fakeStateRepo) ApplyCurrentSnapshot(context.Context, domain.ProbeReplaySession, domain.ProbeCurrentSnapshot, ports.ProbeStateAuthorizer) (*domain.ProbeStateReceipt, error) {
	f.calls++
	return f.receipt, f.err
}

func TestProbeStateServicePagesReconciledAssignmentsAfterCommit(t *testing.T) {
	session := domain.ProbeReplaySession{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222", StreamID: "33333333-3333-4333-8333-333333333333", OwnerID: "44444444-4444-4444-8444-444444444444", ConnectionGeneration: 1}
	now := time.Now().UTC()
	snapshot := domain.ProbeCurrentSnapshot{
		ProbeID: session.ProbeID, StreamID: session.StreamID, SnapshotID: "55555555-5555-4555-8555-555555555555",
		SHA256: strings.Repeat("ab", 32), ConfigRevision: 1, CreatedAt: now, LastCreatedSeq: 4,
		States: []domain.ProbeCurrentState{{MonitorID: 7, AssignmentGeneration: 1, Seq: 4, ObservedAt: now, Status: domain.StatusDown, DownCount: 1}},
	}
	repo := &fakeStateRepo{receipt: &domain.ProbeStateReceipt{SnapshotID: snapshot.SnapshotID, StateCount: 1, MonitorIDs: []int64{9, 7}}}
	svc, err := services.NewProbeStateService(repo, &services.AccessService{})
	if err != nil {
		t.Fatal(err)
	}
	alerter := &recordingGroupAlerter{}
	svc.SetGroupAlerter(alerter)
	receipt, err := svc.ApplySnapshot(t.Context(), session, snapshot)
	if err != nil || receipt.StateCount != 1 || len(alerter.ids) != 1 || alerter.ids[0][0] != 9 || alerter.ids[0][1] != 7 {
		t.Fatalf("omitted assignment was not paged after commit: %+v %+v %v", receipt, alerter.ids, err)
	}

	repo.receipt = &domain.ProbeStateReceipt{SnapshotID: snapshot.SnapshotID, StateCount: 1}
	if _, err := svc.ApplySnapshot(t.Context(), session, snapshot); err != nil || len(alerter.ids) != 1 {
		t.Fatal("identical snapshot paged again", err)
	}

	repo.err = ports.ErrConflict
	if _, err := svc.ApplySnapshot(t.Context(), session, snapshot); !errors.Is(err, ports.ErrConflict) || len(alerter.ids) != 1 {
		t.Fatal("rejected snapshot paged a folder", err)
	}
	if repo.calls != 3 {
		t.Fatalf("repository calls = %d", repo.calls)
	}

	snapshot.SnapshotID = "not-a-uuid"
	if _, err := svc.ApplySnapshot(t.Context(), session, snapshot); !errors.Is(err, domain.ErrValidation) || repo.calls != 3 || len(alerter.ids) != 1 {
		t.Fatal("invalid snapshot reached storage or paging", err)
	}
}
