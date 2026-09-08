package services

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type groupBatchHeartbeatRepo struct {
	*fakeHeartbeatRepo
	batchCalls  int
	singleCalls int
	ids         []int64
	err         error
}

func (r *groupBatchHeartbeatRepo) GetLatest(ctx context.Context, id int64) (*domain.Heartbeat, error) {
	r.singleCalls++
	return r.fakeHeartbeatRepo.GetLatest(ctx, id)
}

func (r *groupBatchHeartbeatRepo) GetLatestForMonitors(ctx context.Context, ids []int64) (map[int64]*domain.Heartbeat, error) {
	r.batchCalls++
	r.ids = append([]int64(nil), ids...)
	if r.err != nil {
		return nil, r.err
	}
	result := make(map[int64]*domain.Heartbeat)
	for _, id := range ids {
		if hb, err := r.fakeHeartbeatRepo.GetLatest(ctx, id); err == nil {
			result[id] = hb
		}
	}
	return result, nil
}

func TestGroupStatusBatchMatchesSequential(t *testing.T) {
	svc, monitors, hb := newGroupTestService()
	ctx := context.Background()
	top := &domain.MonitorGroup{UserID: 1, Name: "Top", Condition: domain.GroupConditionWorstOfChildren}
	if err := svc.Create(ctx, top); err != nil {
		t.Fatal(err)
	}
	sub := &domain.MonitorGroup{UserID: 1, Name: "Sub", ParentID: &top.ID, Condition: domain.GroupConditionWorstOfChildren}
	if err := svc.Create(ctx, sub); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 65; i++ {
		m := &domain.Monitor{UserID: 1, GroupID: &sub.ID}
		if err := monitors.Create(ctx, m); err != nil {
			t.Fatal(err)
		}
		// Include never-checked and maintenance monitors in equivalence.
		if i != 0 {
			seedHeartbeat(t, hb, m.ID, domain.Status(i%4))
		}
	}
	ungrouped := &domain.Monitor{UserID: 1}
	if err := monitors.Create(ctx, ungrouped); err != nil {
		t.Fatal(err)
	}
	want, err := svc.ResolveStatuses(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	batch := &groupBatchHeartbeatRepo{fakeHeartbeatRepo: hb}
	svc.hbRepo = batch
	got, err := svc.ResolveStatuses(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || got[top.ID] != domain.StatusDown {
		t.Fatalf("got %v, want %v", got, want)
	}
	if batch.batchCalls != 1 || batch.singleCalls != 0 || len(batch.ids) != 65 {
		t.Fatalf("batch=%d single=%d ids=%d", batch.batchCalls, batch.singleCalls, len(batch.ids))
	}
	batch.err = errors.New("database unavailable")
	if _, err := svc.ResolveStatuses(ctx, 0); !errors.Is(err, batch.err) {
		t.Fatalf("batch failure hidden: %v", err)
	}
}
