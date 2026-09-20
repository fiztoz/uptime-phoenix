package services_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type mockReplayRepo struct {
	ingestFn func(ctx context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch, authorizer ports.ProbeReplayAuthorizer) (*domain.ProbeReplayResult, error)
}

func (m *mockReplayRepo) IngestReplayBatch(ctx context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch, authorizer ports.ProbeReplayAuthorizer) (*domain.ProbeReplayResult, error) {
	if m.ingestFn != nil {
		return m.ingestFn(ctx, session, batch, authorizer)
	}
	return &domain.ProbeReplayResult{}, nil
}

func (m *mockReplayRepo) GetCursor(ctx context.Context, probeID, streamID string) (int64, error) {
	return 0, nil
}

func TestProbeReplayService_ProcessBatch(t *testing.T) {
	auth := &services.AccessService{}
	repo := &mockReplayRepo{
		ingestFn: func(ctx context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch, authorizer ports.ProbeReplayAuthorizer) (*domain.ProbeReplayResult, error) {
			return &domain.ProbeReplayResult{
				StreamID:      batch.StreamID,
				CommittedSeq:  batch.LastSeq,
				AcceptedCount: int64(len(batch.Events)),
			}, nil
		},
	}

	svc, err := services.NewProbeReplayService(repo, auth)
	if err != nil {
		t.Fatal(err)
	}

	session := domain.ProbeReplaySession{
		HubID:                "11111111-1111-4111-8111-111111111111",
		ProbeID:              "22222222-2222-4222-8222-222222222222",
		StreamID:             "33333333-3333-4333-8333-333333333333",
		ConnectionGeneration: 1,
		OwnerID:              "44444444-4444-4444-8444-444444444444",
	}

	batch := domain.ProbeReplayBatch{
		ProbeID:  "22222222-2222-4222-8222-222222222222",
		StreamID: "33333333-3333-4333-8333-333333333333",
		FirstSeq: 1,
		LastSeq:  2,
		Events: []domain.ProbeReplayEvent{
			{Seq: 1, Kind: domain.ReplayKindObservation, ObservedAt: time.Now().UTC(), Digest: strings.Repeat("a", 64)},
			{Seq: 2, Kind: domain.ReplayKindObservation, ObservedAt: time.Now().UTC(), Digest: strings.Repeat("a", 64)},
		},
	}

	res, err := svc.ProcessBatch(context.Background(), session, batch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.CommittedSeq != 2 || res.AcceptedCount != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Mismatched probe ID fails with ErrConflict
	badBatch := batch
	badBatch.ProbeID = "probe-other"
	if _, err := svc.ProcessBatch(context.Background(), session, badBatch); err == nil {
		t.Fatal("expected conflict on probe ID mismatch")
	}

	// Non-contiguous sequence bounds fail validation
	badSeqBatch := batch
	badSeqBatch.LastSeq = 5
	if _, err := svc.ProcessBatch(context.Background(), session, badSeqBatch); err == nil {
		t.Fatal("expected validation error on non-contiguous batch")
	}
}
