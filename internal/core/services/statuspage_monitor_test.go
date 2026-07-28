package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// stubSPMonitorRepo records what it was asked to do and returns a canned error,
// so the service's error translation can be tested without a database.
type stubSPMonitorRepo struct {
	addErr error

	reorderSpID int64
	reorderIDs  []int64
	reorderCall int
}

func (s *stubSPMonitorRepo) AddMonitor(context.Context, int64, int64, int) error {
	return s.addErr
}

func (s *stubSPMonitorRepo) RemoveMonitor(context.Context, int64, int64) error { return nil }

func (s *stubSPMonitorRepo) ListByStatusPage(context.Context, int64) ([]*domain.StatusPageMonitor, error) {
	return nil, nil
}

func (s *stubSPMonitorRepo) ReorderMonitors(_ context.Context, spID int64, monitorIDs []int64) error {
	s.reorderCall++
	s.reorderSpID = spID
	s.reorderIDs = monitorIDs
	return nil
}

// A duplicate monitor-add reaches the service as the storage-level
// ports.ErrConflict — the same sentinel a taken slug produces. If the service
// passed it straight through, the handler could only answer with the generic
// "slug or custom domain already in use". Translating it here is what makes a
// precise 409 possible.
func TestAddMonitorTranslatesConflictToAlreadyLinked(t *testing.T) {
	repo := &stubSPMonitorRepo{addErr: ports.ErrConflict}
	svc := NewStatusPageService(nil, nil, nil, repo, nil, nil, nil)

	err := svc.AddMonitor(context.Background(), 1, 2, 0)

	if !errors.Is(err, ports.ErrMonitorAlreadyLinked) {
		t.Fatalf("got %v, want ports.ErrMonitorAlreadyLinked", err)
	}
}

// Any other failure must NOT be dressed up as an already-linked conflict —
// that would hide a real storage fault behind a 409.
func TestAddMonitorPassesOtherErrorsThrough(t *testing.T) {
	boom := errors.New("driver exploded")
	repo := &stubSPMonitorRepo{addErr: boom}
	svc := NewStatusPageService(nil, nil, nil, repo, nil, nil, nil)

	err := svc.AddMonitor(context.Background(), 1, 2, 0)

	if errors.Is(err, ports.ErrMonitorAlreadyLinked) {
		t.Fatal("a generic storage error was reported as a monitor-already-linked conflict")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the underlying error preserved", err)
	}
}

// The reorder must reach the repository verbatim and in ONE call — the whole
// point of the endpoint is that it is a single transactional write, not a
// sequence of removes and re-adds that can lose an assignment part-way.
func TestReorderMonitorsDelegatesInOneCall(t *testing.T) {
	repo := &stubSPMonitorRepo{}
	svc := NewStatusPageService(nil, nil, nil, repo, nil, nil, nil)

	if err := svc.ReorderMonitors(context.Background(), 7, []int64{3, 1, 2}); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	if repo.reorderCall != 1 {
		t.Errorf("repo hit %d times, want exactly 1", repo.reorderCall)
	}
	if repo.reorderSpID != 7 {
		t.Errorf("status page id = %d, want 7", repo.reorderSpID)
	}
	want := []int64{3, 1, 2}
	if len(repo.reorderIDs) != len(want) {
		t.Fatalf("got %v, want %v", repo.reorderIDs, want)
	}
	for i, id := range want {
		if repo.reorderIDs[i] != id {
			t.Fatalf("order not preserved: got %v, want %v", repo.reorderIDs, want)
		}
	}
}
