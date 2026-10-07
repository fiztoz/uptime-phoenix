package services

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type recoveryBatchPages struct {
	ports.StatusPageRepository
	calls  atomic.Int64
	err    error
	before func()
}

func (r *recoveryBatchPages) List(ctx context.Context) ([]*domain.StatusPage, error) {
	r.calls.Add(1)
	if r.before != nil {
		r.before()
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.StatusPageRepository.List(ctx)
}

type recoveryBatchOverall struct {
	calls    atomic.Int64
	monitors atomic.Int64
	read     func(context.Context, []int64, time.Time) (map[int64]domain.Status, error)
}

func (r *recoveryBatchOverall) StatusForMonitors(ctx context.Context, ids []int64, now time.Time) (map[int64]domain.Status, error) {
	r.calls.Add(1)
	r.monitors.Add(int64(len(ids)))
	if r.read != nil {
		return r.read(ctx, ids, now)
	}
	out := make(map[int64]domain.Status, len(ids))
	for _, id := range ids {
		out[id] = domain.StatusUp
	}
	return out, nil
}

type recoveryBatchMail struct{ resolved []int64 }

func (*recoveryBatchMail) NotifyIncidentCreated(context.Context, *domain.Incident) error { return nil }
func (*recoveryBatchMail) NotifyIncidentUpdated(context.Context, *domain.Incident, *domain.IncidentUpdate) error {
	return nil
}
func (n *recoveryBatchMail) NotifyIncidentResolved(_ context.Context, inc *domain.Incident) error {
	n.resolved = append(n.resolved, inc.ID)
	return nil
}

func recoveryBatchFixture() (*StatusPageService, *fakeSPRepo, *fakeIncidentRepo, *fakeSPMonitorRepo, *recoveryBatchPages, *recoveryBatchOverall) {
	pages, incidents, links := newFakeSPRepo(), newFakeIncidentRepo(), newFakeSPMonitorRepo()
	svc := newSPServiceForIncidentTests(pages, incidents, links)
	counted := &recoveryBatchPages{StatusPageRepository: pages}
	overall := &recoveryBatchOverall{}
	svc.repo = counted
	svc.SetAggregateStatus(overall)
	return svc, pages, incidents, links, counted, overall
}
func recoveryBatchPage(t testing.TB, pages *fakeSPRepo, incidents *fakeIncidentRepo, links *fakeSPMonitorRepo, enabled, active bool, ids ...int64) *domain.Incident {
	t.Helper()
	page := &domain.StatusPage{Slug: fmt.Sprint(ids), AutoResolveIncidents: enabled}
	if err := pages.Create(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := links.AddMonitor(context.Background(), page.ID, id, 1); err != nil {
			t.Fatal(err)
		}
	}
	inc := &domain.Incident{StatusPageID: page.ID, Title: "outage", Active: active}
	if err := incidents.Create(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return inc
}
func TestRegionalRecoveryBatchSkipsHealthWithoutWork(t *testing.T) {
	for _, kind := range []string{"no pages", "disabled", "unrelated", "already resolved", "invalid IDs"} {
		t.Run(kind, func(t *testing.T) {
			svc, pages, incidents, links, counted, overall := recoveryBatchFixture()
			ids := []int64{1, 2, 3, 1}
			switch kind {
			case "disabled":
				recoveryBatchPage(t, pages, incidents, links, false, true, 1)
			case "unrelated":
				recoveryBatchPage(t, pages, incidents, links, true, true, 99)
			case "already resolved":
				recoveryBatchPage(t, pages, incidents, links, true, false, 1)
			case "invalid IDs":
				ids = []int64{0, -1}
			}
			original := append([]int64(nil), ids...)
			r := regionalRecovery{overall: overall, resolver: svc}
			r.resolve(t.Context(), ids)
			if overall.calls.Load() != 0 {
				t.Fatalf("read health %d times with no incident recovery work", overall.calls.Load())
			}
			wantPages := int64(1)
			if kind == "invalid IDs" {
				wantPages = 0
			}
			if counted.calls.Load() != wantPages {
				t.Fatalf("page lists=%d want %d per batch", counted.calls.Load(), wantPages)
			}
			if !reflect.DeepEqual(ids, original) {
				t.Fatal("filter changed caller's evidence slice")
			}
		})
	}
}
func TestRegionalRecoveryBatchKeepsFreshPolicyAndNotificationSemantics(t *testing.T) {
	for _, kind := range []string{"up", "down", "unknown", "pending", "maintenance", "missing", "error", "up then down"} {
		t.Run(kind, func(t *testing.T) {
			svc, pages, incidents, links, _, overall := recoveryBatchFixture()
			active := recoveryBatchPage(t, pages, incidents, links, true, true, 7, 8)
			unrelated := recoveryBatchPage(t, pages, incidents, links, true, true, 99)
			disabled := recoveryBatchPage(t, pages, incidents, links, false, true, 7)
			updates := newFakeIncidentUpdateRepo()
			mail := &recoveryBatchMail{}
			svc.incidentUpdates = updates
			svc.incidentMail = mail
			overall.read = func(_ context.Context, ids []int64, now time.Time) (map[int64]domain.Status, error) {
				if now.Location() != time.UTC {
					t.Error("non-UTC health read")
				}
				for _, id := range ids {
					if id != 7 && id != 8 {
						t.Errorf("unrelated health read: %v", ids)
					}
				}
				if kind == "error" {
					return nil, errors.New("unavailable")
				}
				if kind == "missing" {
					return map[int64]domain.Status{}, nil
				}
				status := domain.StatusUp
				switch kind {
				case "down":
					status = domain.StatusDown
				case "unknown":
					status = domain.StatusUnknown
				case "pending":
					status = domain.StatusPending
				case "maintenance":
					status = domain.StatusMaintenance
				case "up then down":
					if overall.calls.Load() > 1 {
						status = domain.StatusDown
					}
				}
				out := map[int64]domain.Status{}
				for _, id := range ids {
					out[id] = status
				}
				return out, nil
			}
			r := regionalRecovery{overall: overall, resolver: svc}
			r.resolve(t.Context(), []int64{0, 7, 8, 7, 1})
			got, _ := incidents.GetByID(t.Context(), active.ID)
			wantResolved := kind == "up"
			if got.Active == wantResolved {
				t.Fatalf("active=%v, scenario %s", got.Active, kind)
			}
			for _, inc := range []*domain.Incident{unrelated, disabled} {
				got, _ := incidents.GetByID(t.Context(), inc.ID)
				if !got.Active {
					t.Fatal("resolved unrelated or disabled page")
				}
			}
			// Repeated/duplicate replay must not add another update or notification.
			r.resolve(t.Context(), []int64{7, 8, 7})
			entries, _ := updates.ListByIncident(t.Context(), active.ID)
			want := 0
			if wantResolved {
				want = 1
			}
			if len(entries) != want || len(mail.resolved) != want {
				t.Fatalf("updates=%d mail=%d want %d", len(entries), len(mail.resolved), want)
			}
		})
	}
}
func TestRegionalRecoveryBatchDoesNotCacheAbsentWork(t *testing.T) {
	svc, pages, incidents, links, _, overall := recoveryBatchFixture()
	r := regionalRecovery{overall: overall, resolver: svc}
	r.resolve(t.Context(), []int64{7})
	inc := recoveryBatchPage(t, pages, incidents, links, true, true, 7)
	r.resolve(t.Context(), []int64{7})
	got, _ := incidents.GetByID(t.Context(), inc.ID)
	if got.Active {
		t.Fatal("new incident hidden by earlier empty candidate set")
	}
}
func TestRegionalRecoveryBatchCandidateReadFailure(t *testing.T) {
	svc, _, _, _, pages, overall := recoveryBatchFixture()
	pages.err = errors.New("unavailable")
	regionalRecovery{overall: overall, resolver: svc}.resolve(t.Context(), []int64{7})
	if overall.calls.Load() != 0 {
		t.Fatal("health read despite failed candidate discovery")
	}
}

type recoveryBatchReplayRepo struct {
	committed bool
	writeErr  error
	cursor    int64
}

func (r *recoveryBatchReplayRepo) IngestReplayBatch(_ context.Context, _ domain.ProbeReplaySession, b domain.ProbeReplayBatch, _ ports.ProbeReplayAuthorizer) (*domain.ProbeReplayResult, error) {
	if r.writeErr != nil {
		return nil, r.writeErr
	}
	r.committed = true
	return &domain.ProbeReplayResult{StreamID: b.StreamID, CommittedSeq: b.LastSeq, AcceptedCount: int64(len(b.Events))}, nil
}
func (r *recoveryBatchReplayRepo) GetCursor(context.Context, string, string) (int64, error) {
	return r.cursor, nil
}

type recoveryBatchPublisher struct{ ids []int64 }

func (p *recoveryBatchPublisher) OnRegionalEvidence(_ context.Context, ids []int64) {
	p.ids = append(p.ids, ids...)
}
func TestRegionalRecoveryBatchPreservesReplayCommitAndFanout(t *testing.T) {
	session := domain.ProbeReplaySession{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222", StreamID: "33333333-3333-4333-8333-333333333333", OwnerID: "44444444-4444-4444-8444-444444444444", ConnectionGeneration: 1}
	batch := domain.ProbeReplayBatch{ProbeID: session.ProbeID, StreamID: session.StreamID, FirstSeq: 1, LastSeq: 2}
	for i := int64(1); i <= 2; i++ {
		batch.Events = append(batch.Events, domain.ProbeReplayEvent{Seq: i, Kind: domain.ReplayKindObservation, Digest: strings.Repeat("a", 64), ObservedAt: time.Now().UTC(), Observation: &domain.RegionalObservation{MonitorID: i}})
	}
	for _, mode := range []string{"success", "rollback", "ambiguous commit"} {
		t.Run(mode, func(t *testing.T) {
			resolver, _, _, _, pages, overall := recoveryBatchFixture()
			repo := &recoveryBatchReplayRepo{}
			if mode != "success" {
				repo.writeErr = domain.ErrInternal
				if mode == "ambiguous commit" {
					repo.cursor = 2
				}
			}
			svc, err := NewProbeReplayService(repo, &AccessService{})
			if err != nil {
				t.Fatal(err)
			}
			browser, groups := &recoveryBatchPublisher{}, &recoveryBatchPublisher{}
			svc.SetBrowserPublisher(browser)
			svc.SetGroupAlerter(groups)
			svc.SetStatusPageRecovery(overall, resolver)
			pages.before = func() {
				if !repo.committed && repo.cursor < batch.LastSeq {
					t.Error("candidate read before durable prefix")
				}
			}
			result, err := svc.ProcessBatch(t.Context(), session, batch)
			wantCalls := int64(1)
			wantIDs := []int64{1, 2}
			if mode == "rollback" {
				wantCalls = 0
				wantIDs = nil
			}
			if mode == "success" && (err != nil || result.CommittedSeq != 2) {
				t.Fatalf("success result=%+v err=%v", result, err)
			}
			if mode == "ambiguous commit" && (!errors.Is(err, domain.ErrReplayRetry) || result.CommittedSeq != 2) {
				t.Fatalf("ambiguous commit became false ACK: %+v %v", result, err)
			}
			if pages.calls.Load() != wantCalls || overall.calls.Load() != 0 {
				t.Fatalf("candidate calls=%d health=%d", pages.calls.Load(), overall.calls.Load())
			}
			if !reflect.DeepEqual(browser.ids, wantIDs) || !reflect.DeepEqual(groups.ids, wantIDs) {
				t.Fatalf("filtered status-page work altered fanout: browser=%v groups=%v", browser.ids, groups.ids)
			}
		})
	}
}
func BenchmarkRegionalRecoveryBatchNoPages(b *testing.B) {
	svc, _, _, _, pages, overall := recoveryBatchFixture()
	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	r := regionalRecovery{overall: overall, resolver: svc}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.resolve(context.Background(), ids)
	}
	b.ReportMetric(float64(pages.calls.Load())/float64(b.N), "page_lists/op")
	b.ReportMetric(float64(overall.calls.Load())/float64(b.N), "health_calls/op")
	b.ReportMetric(float64(overall.monitors.Load())/float64(b.N), "health_monitors/op")
}
func TestRegionalRecoveryBatchConcurrentNoWork(t *testing.T) {
	svc, _, _, _, pages, overall := recoveryBatchFixture()
	r := regionalRecovery{overall: overall, resolver: svc}
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func() { r.resolve(t.Context(), []int64{1, 2, 3}); done <- struct{}{} }()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if pages.calls.Load() != 8 || overall.calls.Load() != 0 {
		t.Fatalf("pages=%d health=%d", pages.calls.Load(), overall.calls.Load())
	}
}
func TestRegionalRecoveryBatchRechecksPageEligibility(t *testing.T) {
	svc, pages, incidents, links, _, overall := recoveryBatchFixture()
	inc := recoveryBatchPage(t, pages, incidents, links, true, true, 7)
	overall.read = func(ctx context.Context, ids []int64, _ time.Time) (map[int64]domain.Status, error) {
		page, err := pages.GetByID(ctx, inc.StatusPageID)
		if err != nil {
			t.Fatal(err)
		}
		page.AutoResolveIncidents = false
		if err := pages.Update(ctx, page); err != nil {
			t.Fatal(err)
		}
		return map[int64]domain.Status{7: domain.StatusUp}, nil
	}
	regionalRecovery{overall: overall, resolver: svc}.resolve(t.Context(), []int64{7})
	got, _ := incidents.GetByID(t.Context(), inc.ID)
	if !got.Active {
		t.Fatal("stale candidate bypassed disabled page")
	}
}

type recoveryBatchStateRepo struct {
	receipt   *domain.ProbeStateReceipt
	err       error
	committed bool
}

func (r *recoveryBatchStateRepo) ApplyCurrentSnapshot(context.Context, domain.ProbeReplaySession, domain.ProbeCurrentSnapshot, ports.ProbeStateAuthorizer) (*domain.ProbeStateReceipt, error) {
	r.committed = r.err == nil
	return r.receipt, r.err
}
func TestRegionalRecoveryBatchPreservesSnapshotReceiptAndFanout(t *testing.T) {
	session := domain.ProbeReplaySession{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222", StreamID: "33333333-3333-4333-8333-333333333333", OwnerID: "44444444-4444-4444-8444-444444444444", ConnectionGeneration: 1}
	now := time.Now().UTC()
	snapshot := domain.ProbeCurrentSnapshot{ProbeID: session.ProbeID, StreamID: session.StreamID, SnapshotID: "55555555-5555-4555-8555-555555555555", SHA256: strings.Repeat("ab", 32), ConfigRevision: 1, CreatedAt: now, LastCreatedSeq: 4, States: []domain.ProbeCurrentState{{MonitorID: 7, AssignmentGeneration: 1, Seq: 4, ObservedAt: now, Status: domain.StatusUp}}}
	resolver, _, _, _, pages, overall := recoveryBatchFixture()
	repo := &recoveryBatchStateRepo{receipt: &domain.ProbeStateReceipt{SnapshotID: snapshot.SnapshotID, MonitorIDs: []int64{9, 7}}}
	svc, err := NewProbeStateService(repo, &AccessService{})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetStatusPageRecovery(overall, resolver)
	browser, groups := &recoveryBatchPublisher{}, &recoveryBatchPublisher{}
	svc.SetBrowserPublisher(browser)
	svc.SetGroupAlerter(groups)
	pages.before = func() {
		if !repo.committed {
			t.Error("candidate discovery preceded snapshot commit")
		}
	}
	receipt, err := svc.ApplySnapshot(t.Context(), session, snapshot)
	if err != nil || receipt != repo.receipt || pages.calls.Load() != 1 || overall.calls.Load() != 0 {
		t.Fatalf("receipt=%+v err=%v pages=%d health=%d", receipt, err, pages.calls.Load(), overall.calls.Load())
	}
	if !reflect.DeepEqual(browser.ids, []int64{9, 7}) || !reflect.DeepEqual(groups.ids, []int64{9, 7}) {
		t.Fatal("candidate filter changed reconciled-assignment fanout")
	}
	repo.receipt = &domain.ProbeStateReceipt{SnapshotID: snapshot.SnapshotID}
	if _, err := svc.ApplySnapshot(t.Context(), session, snapshot); err != nil || pages.calls.Load() != 1 || len(browser.ids) != 2 || len(groups.ids) != 2 {
		t.Fatal("identical snapshot repeated recovery or fanout", err)
	}
	repo.err = ports.ErrConflict
	if _, err := svc.ApplySnapshot(t.Context(), session, snapshot); !errors.Is(err, ports.ErrConflict) || pages.calls.Load() != 1 {
		t.Fatal("failed snapshot reached recovery", err)
	}
}
