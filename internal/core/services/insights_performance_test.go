package services

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type insightsReadProbe struct {
	calls [3]atomic.Int64
	read  func(context.Context, int, []int64, time.Time) error
}

func (p *insightsReadProbe) before(ctx context.Context, stage int, ids []int64, from time.Time) error {
	p.calls[stage].Add(1)
	if p.read != nil {
		return p.read(ctx, stage, ids, from)
	}
	return nil
}

func (p *insightsReadProbe) ListImportantForMonitors(ctx context.Context, ids []int64, from, _ time.Time) (map[int64][]*domain.Heartbeat, error) {
	if err := p.before(ctx, 0, ids, from); err != nil {
		return nil, err
	}
	out := make(map[int64][]*domain.Heartbeat, len(ids))
	for _, id := range ids {
		out[id] = []*domain.Heartbeat{
			{ID: 2, Time: from.Add(time.Hour), Status: domain.StatusDown, Important: true},
			{ID: 3, Time: from.Add(2 * time.Hour), Status: domain.StatusUp, Important: true},
		}
	}
	return out, nil
}

func (p *insightsReadProbe) LatestImportantBeforeForMonitors(ctx context.Context, ids []int64, from time.Time) (map[int64]*domain.Heartbeat, error) {
	if err := p.before(ctx, 1, ids, from); err != nil {
		return nil, err
	}
	out := make(map[int64]*domain.Heartbeat, len(ids))
	for _, id := range ids {
		out[id] = &domain.Heartbeat{ID: 1, Time: from.Add(-time.Hour), Status: domain.StatusUp, Important: true}
	}
	return out, nil
}

func (p *insightsReadProbe) GetAggregate1hForMonitors(ctx context.Context, ids []int64, from time.Time) (map[int64][]*ports.Aggregate1h, error) {
	if err := p.before(ctx, 2, ids, from); err != nil {
		return nil, err
	}
	out := make(map[int64][]*ports.Aggregate1h, len(ids))
	for _, id := range ids {
		out[id] = []*ports.Aggregate1h{
			{Bucket: from.Truncate(time.Hour), TotalChecks: 60, PingCount: 50, AvgPing: 123},
			{Bucket: from.Truncate(time.Hour).Add(time.Hour), TotalChecks: 60, PingCount: 50, AvgPing: 123},
		}
	}
	return out, nil
}

func (p *insightsReadProbe) GetAggregate1dForMonitors(ctx context.Context, ids []int64, from time.Time) (map[int64][]*ports.Aggregate1d, error) {
	if err := p.before(ctx, 2, ids, from); err != nil {
		return nil, err
	}
	out := make(map[int64][]*ports.Aggregate1d, len(ids))
	for _, id := range ids {
		out[id] = []*ports.Aggregate1d{{Bucket: from.Truncate(24 * time.Hour), TotalChecks: 1440, PingCount: 1200, AvgPing: 123}}
	}
	return out, nil
}

func newInsightsProbeService(t *testing.T) (*InsightsService, *insightsReadProbe, *accessHarness, InsightsQuery) {
	t.Helper()
	h := newAccessHarness(t)
	admin := h.addUser(t, "admin", true)
	h.addMonitor(t, "HTTP", nil)
	p := &insightsReadProbe{}
	s := NewInsightsService(p, p, h.monitors, h.groups, h.svc)
	s.now = func() time.Time { return time.Date(2026, 9, 8, 12, 30, 0, 0, time.FixedZone("UTC+7", 7*3600)) }
	return s, p, h, InsightsQuery{UserID: admin, Period: Period24h, Type: "http"}
}

func TestInsightsConcurrentReadsUseSameUTCWindow(t *testing.T) {
	for _, period := range []InsightsPeriod{Period24h, Period7d, Period30d, Period90d} {
		t.Run(string(period), func(t *testing.T) {
			s, p, _, q := newInsightsProbeService(t)
			q.Period = period
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started := make(chan int, 3)
			release := make(chan struct{})
			p.read = func(ctx context.Context, stage int, ids []int64, from time.Time) error {
				if from.Location() != time.UTC || !from.Equal(s.now().Add(-period.duration())) || !reflect.DeepEqual(ids, []int64{1}) {
					t.Errorf("read %d: ids=%v from=%v", stage, ids, from)
				}
				started <- stage
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			done := make(chan error, 1)
			go func() { _, err := s.GetInsights(ctx, q); done <- err }()
			for range 3 {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("all three reads must start before any is allowed to finish")
				}
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInsightsConcurrentFailureCancelsSiblings(t *testing.T) {
	for _, failStage := range []int{0, 1, 2} {
		t.Run(string(rune('0'+failStage)), func(t *testing.T) {
			s, p, _, q := newInsightsProbeService(t)
			failure := errors.New("database read failed")
			var entered, canceled atomic.Int64
			ready := make(chan struct{})
			p.read = func(ctx context.Context, stage int, _ []int64, _ time.Time) error {
				if entered.Add(1) == 3 {
					close(ready)
				}
				<-ready
				if stage == failStage {
					return failure
				}
				<-ctx.Done()
				canceled.Add(1)
				return ctx.Err()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := s.GetInsights(ctx, q)
			if !errors.Is(err, failure) || result != nil || canceled.Load() != 2 {
				t.Fatalf("result=%v err=%v canceled=%d", result, err, canceled.Load())
			}
			p.read = nil
			if _, err := s.GetInsights(ctx, q); err != nil || p.calls[0].Load() != 2 {
				t.Fatalf("failed result was cached: %v", err)
			}
		})
	}
}

func TestInsightsConcurrentResultMatchesSequentialReference(t *testing.T) {
	for _, period := range []InsightsPeriod{Period24h, Period7d, Period30d, Period90d} {
		t.Run(string(period), func(t *testing.T) {
			s, _, h, q := newInsightsProbeService(t)
			q.Period = period
			ctx := context.Background()
			got, err := s.GetInsights(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			monitors, err := h.monitors.List(ctx, ports.MonitorFilter{})
			if err != nil {
				t.Fatal(err)
			}
			p := &insightsReadProbe{}
			ids := []int64{monitors[0].ID}
			transitions, _ := p.ListImportantForMonitors(ctx, ids, got.From, got.To)
			leading, _ := p.LatestImportantBeforeForMonitors(ctx, ids, got.From)
			var hourly map[int64][]*ports.Aggregate1h
			var daily map[int64][]*ports.Aggregate1d
			if period == Period24h {
				hourly, _ = p.GetAggregate1hForMonitors(ctx, ids, got.From)
			} else {
				daily, _ = p.GetAggregate1dForMonitors(ctx, ids, got.From)
			}
			want := s.computeRow(monitors[0], got.From, got.To, transitions[ids[0]], leading[ids[0]], hourly[ids[0]], daily[ids[0]])
			if !reflect.DeepEqual(got.Rows, []InsightsRow{want}) {
				t.Fatalf("concurrent result differs: got=%+v want=%+v", got.Rows, want)
			}
		})
	}
}

func TestInsightsCacheReusesMetricsAndExpires(t *testing.T) {
	s, p, _, q := newInsightsProbeService(t)
	ctx := context.Background()
	first, err := s.GetInsights(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	first.Rows[0].MonitorName = "poisoned"
	*first.Rows[0].AvailabilityPercent = -1
	*first.Rows[0].LatencyAvgMs = -1
	q.Metric = MetricLatency
	q.Type = " http "
	second, err := s.GetInsights(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if p.calls[0].Load() != 1 || second.Metric != MetricLatency || second.Rows[0].MonitorName != "HTTP" || *second.Rows[0].AvailabilityPercent < 0 || *second.Rows[0].LatencyAvgMs != 123 {
		t.Fatalf("cache was not reused safely: %+v (reads=%d)", second, p.calls[0].Load())
	}
	if second.To != first.To {
		t.Fatal("cached window was relabeled as fresh")
	}
	q.Type = ""
	if _, err := s.GetInsights(ctx, q); !errors.Is(err, ErrInsightsLatencyTypeRequired) {
		t.Fatalf("cache bypassed latency type validation: %v", err)
	}
	q.Type = "http"
	now := s.now().Add(insightsCacheTTL)
	s.now = func() time.Time { return now }
	if _, err := s.GetInsights(ctx, q); err != nil || p.calls[0].Load() != 2 {
		t.Fatalf("TTL did not expire: err=%v reads=%d", err, p.calls[0].Load())
	}
}

func TestInsightsCacheRefreshesVisibilityAndMonitorEdits(t *testing.T) {
	s, p, h, q := newInsightsProbeService(t)
	ctx := context.Background()
	member := h.addUser(t, "member", false)
	if err := h.svc.GrantMonitor(ctx, member, 1); err != nil {
		t.Fatal(err)
	}
	get := func(user int64, wantRows int) {
		t.Helper()
		q.UserID = user
		got, err := s.GetInsights(ctx, q)
		if err != nil || len(got.Rows) != wantRows {
			t.Fatalf("user=%d got=%+v err=%v want rows=%d", user, got, err, wantRows)
		}
	}
	get(1, 1)
	get(member, 1)
	if p.calls[0].Load() != 2 {
		t.Fatal("different users shared a cache entry")
	}
	if err := h.svc.RevokeMonitor(ctx, member, 1); err != nil {
		t.Fatal(err)
	}
	get(member, 0)
	if p.calls[0].Load() != 2 {
		t.Fatal("zero grants queried heartbeat data")
	}
	h.addMonitor(t, "new", nil)
	get(1, 2)
	monitor, err := h.monitors.GetByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	monitor.Name, monitor.Interval = "renamed", 120
	if err := h.monitors.Update(ctx, monitor); err != nil {
		t.Fatal(err)
	}
	get(1, 2)
	if p.calls[0].Load() != 4 {
		t.Fatal("monitor addition/edit did not invalidate the cached calculation")
	}
	if err := h.monitors.Delete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	get(1, 1)
	if p.calls[0].Load() != 5 {
		t.Fatal("deleted monitor stayed cached")
	}
}

func TestInsightsGroupFilterIntersectsGrantsBeforeReads(t *testing.T) {
	s, p, h, q := newInsightsProbeService(t)
	ctx := context.Background()
	parent := h.addGroup(t, "parent", nil)
	child := h.addGroup(t, "child", &parent)
	visible := h.addMonitor(t, "visible", &child)
	h.addMonitor(t, "hidden sibling", &child)
	member := h.addUser(t, "member", false)
	if err := h.svc.GrantMonitor(ctx, member, visible); err != nil {
		t.Fatal(err)
	}
	q.UserID, q.GroupID = member, &parent
	p.read = func(_ context.Context, _ int, ids []int64, _ time.Time) error {
		if !reflect.DeepEqual(ids, []int64{visible}) {
			t.Errorf("group filter widened grant: %v", ids)
		}
		return nil
	}
	got, err := s.GetInsights(ctx, q)
	if err != nil || len(got.Rows) != 1 || got.Rows[0].MonitorID != visible {
		t.Fatalf("descendant filter: %+v %v", got, err)
	}
	unknown := int64(9999)
	q.GroupID = &unknown
	got, err = s.GetInsights(ctx, q)
	if err != nil || len(got.Rows) != 0 || p.calls[0].Load() != 1 {
		t.Fatalf("invisible group reached aggregation: %+v %v", got, err)
	}
}

func TestInsightsCacheIsolatesFiltersAndReducedGrants(t *testing.T) {
	s, p, h, q := newInsightsProbeService(t)
	ctx := context.Background()
	parent := h.addGroup(t, "parent", nil)
	child := h.addGroup(t, "child", &parent)
	m, err := h.monitors.GetByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.GroupID = &child
	if err := h.monitors.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	queries := []InsightsQuery{
		q,
		{UserID: q.UserID, Period: Period7d, Type: "http"},
		{UserID: q.UserID, Period: Period24h},
		{UserID: q.UserID, Period: Period24h, Type: "http", GroupID: &parent},
		{UserID: q.UserID, Period: Period24h, Type: "http", GroupID: &child},
	}
	// Each filter selects the SAME monitor, yet occupies a distinct entry.
	for _, query := range queries {
		for range 2 {
			got, err := s.GetInsights(ctx, query)
			if err != nil || len(got.Rows) != 1 {
				t.Fatalf("query=%+v got=%+v err=%v", query, got, err)
			}
		}
	}
	if p.calls[0].Load() != int64(len(queries)) {
		t.Fatal("query filters were not isolated")
	}
	member := h.addUser(t, "member", false)
	other := h.addMonitor(t, "other", &child)
	for _, id := range []int64{1, other} {
		if err := h.svc.GrantMonitor(ctx, member, id); err != nil {
			t.Fatal(err)
		}
	}
	q.UserID = member
	if got, err := s.GetInsights(ctx, q); err != nil || len(got.Rows) != 2 {
		t.Fatalf("initial grant result=%+v err=%v", got, err)
	}
	if err := h.svc.RevokeMonitor(ctx, member, other); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInsights(ctx, q)
	if err != nil || len(got.Rows) != 1 || got.Rows[0].MonitorID != 1 {
		t.Fatalf("reduced allowlist received cached monitor: %+v %v", got, err)
	}
	// Moving the remaining monitor out of the selected tree must also remove
	// it on the next request, even with a warm cache for that group.
	q.GroupID = &parent
	if _, err := s.GetInsights(ctx, q); err != nil {
		t.Fatal(err)
	}
	m.GroupID = nil
	if err := h.monitors.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetInsights(ctx, q)
	if err != nil || len(got.Rows) != 0 {
		t.Fatalf("moved monitor stayed in cached group: %+v %v", got, err)
	}
}

type insightsTestObserver struct {
	shared chan struct{}
}

func (*insightsTestObserver) ObserveInsightsStage(string, time.Duration) {}
func (o *insightsTestObserver) IncInsightsCache(outcome string) {
	if outcome == "shared" {
		o.shared <- struct{}{}
	}
}

func TestInsightsCoalescesRequestsAndHandlesCancellation(t *testing.T) {
	for _, cancelLeader := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiter-cancel", true: "leader-cancel"}[cancelLeader], func(t *testing.T) {
			s, p, _, q := newInsightsProbeService(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			leaderCtx, cancelFirst := context.WithCancel(ctx)
			defer cancelFirst()
			started := make(chan struct{}, 3)
			release := make(chan struct{})
			p.read = func(ctx context.Context, _ int, _ []int64, _ time.Time) error {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return nil
				}
			}
			s.SetObserver(&insightsTestObserver{shared: make(chan struct{}, 20)})
			observer := s.observer.(*insightsTestObserver)
			first := make(chan error, 1)
			go func() { _, err := s.GetInsights(leaderCtx, q); first <- err }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("leader did not start")
			}
			var waiters sync.WaitGroup
			for range 10 {
				waiters.Add(1)
				go func() {
					defer waiters.Done()
					if _, err := s.GetInsights(ctx, q); err != nil {
						t.Errorf("live waiter: %v", err)
					}
				}()
			}
			for range 10 {
				select {
				case <-observer.shared:
				case <-ctx.Done():
					t.Fatal("waiter did not join")
				}
			}
			if cancelLeader {
				cancelFirst()
				if err := <-first; !errors.Is(err, context.Canceled) {
					t.Fatalf("leader: %v", err)
				}
			} else {
				waiterCtx, cancelWaiter := context.WithCancel(ctx)
				departed := make(chan error, 1)
				go func() { _, err := s.GetInsights(waiterCtx, q); departed <- err }()
				select {
				case <-observer.shared:
				case <-ctx.Done():
					t.Fatal("cancelable waiter did not join")
				}
				cancelWaiter()
				if err := <-departed; !errors.Is(err, context.Canceled) {
					t.Fatalf("waiter: %v", err)
				}
			}
			close(release)
			waiters.Wait()
			if !cancelLeader {
				if err := <-first; err != nil {
					t.Fatal(err)
				}
			}
			want := int64(1)
			if cancelLeader {
				want = 2
			}
			if p.calls[0].Load() != want {
				t.Fatalf("calculations=%d want=%d", p.calls[0].Load(), want)
			}
		})
	}
}

func TestInsightsCacheBoundsRetention(t *testing.T) {
	now := time.Now().UTC()
	var c insightsCache
	for id := int64(1); id <= insightsCacheMaxEntries+1; id++ {
		c.put(insightsCacheKey{userID: id}, &InsightsResult{To: now, Rows: []InsightsRow{{}}}, now)
	}
	if len(c.entries) != insightsCacheMaxEntries {
		t.Fatalf("unbounded entries: %d", len(c.entries))
	}
	c.put(insightsCacheKey{userID: 100}, &InsightsResult{To: now, Rows: make([]InsightsRow, insightsCacheMaxRows)}, now)
	if c.rows != insightsCacheMaxRows || len(c.entries) != 1 {
		t.Fatalf("row cap: rows=%d entries=%d", c.rows, len(c.entries))
	}
	c.put(insightsCacheKey{userID: 101}, &InsightsResult{To: now, Rows: make([]InsightsRow, insightsCacheMaxRows+1)}, now)
	if c.rows != insightsCacheMaxRows {
		t.Fatal("oversized response was retained")
	}
	c.put(insightsCacheKey{userID: 102}, &InsightsResult{To: now.Add(insightsCacheTTL), Rows: []InsightsRow{{}}}, now.Add(insightsCacheTTL))
	if c.rows != 1 || len(c.entries) != 1 {
		t.Fatal("expired entries were retained")
	}
}
