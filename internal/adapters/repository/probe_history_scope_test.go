package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// cloneHistoryProbe registers another probe on the fixture monitor by copying
// the fixture probe's registration, assignment, observations and dirty rows.
// The copy's observations take the given status from minute `downFrom` on.
func cloneHistoryProbe(t *testing.T, r replayFixture, k int, downFrom int) string {
	t.Helper()
	probeID := fmt.Sprintf("11111111-1111-4111-8111-%012d", k)
	streamID := fmt.Sprintf("44444444-4444-4444-8444-%012d", k)
	concat := "probe_key || '-%d'"
	if r.f.db.Dialect().Name() == dialect.MySQL {
		concat = "CONCAT(probe_key, '-%d')"
	}
	source := r.session.ProbeID
	stmts := []string{
		"CREATE TEMPORARY TABLE clone_probe AS SELECT * FROM probes WHERE id = '" + source + "'",
		fmt.Sprintf("UPDATE clone_probe SET id = '%s', probe_key = "+concat, probeID, k),
		"INSERT INTO probes SELECT * FROM clone_probe",
		"DROP TABLE clone_probe",
		"CREATE TEMPORARY TABLE clone_assignment AS SELECT * FROM monitor_probe_assignment_history WHERE probe_id = '" + source + "'",
		fmt.Sprintf("UPDATE clone_assignment SET id = id + %d, probe_id = '%s'", 100*k, probeID),
		"INSERT INTO monitor_probe_assignment_history SELECT * FROM clone_assignment",
		"DROP TABLE clone_assignment",
		"CREATE TEMPORARY TABLE clone_obs AS SELECT * FROM probe_observations WHERE probe_id = '" + source + "'",
		fmt.Sprintf("UPDATE clone_obs SET id = id + %d, probe_id = '%s', stream_id = '%s'", 1000000*k, probeID, streamID),
		fmt.Sprintf("UPDATE clone_obs SET status = %d, raw_status = %d, down_count = 1 WHERE seq > %d", domain.StatusDown, domain.StatusDown, downFrom),
		"INSERT INTO probe_observations SELECT * FROM clone_obs",
		"DROP TABLE clone_obs",
		"CREATE TEMPORARY TABLE clone_dirty AS SELECT * FROM probe_dirty_buckets WHERE probe_id = '" + source + "'",
		fmt.Sprintf("UPDATE clone_dirty SET probe_id = '%s'", probeID),
		"INSERT INTO probe_dirty_buckets SELECT * FROM clone_dirty",
		"DROP TABLE clone_dirty",
	}
	for _, stmt := range stmts {
		if _, err := r.f.db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
	return probeID
}

// Narrowing a regional item's evidence to its own probe, and the bounded seed
// read, must not change any projected row (#196).
func TestProbeHistoryMultiProbeProjection(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			const minutes = 12
			events := make([]domain.ProbeReplayEvent, 0, minutes)
			for i := int64(1); i <= minutes; i++ {
				status := domain.StatusUp
				if i == 5 || i == 6 {
					status = domain.StatusDown
				}
				events = append(events, replayObservationAt(r, i, (i-1)*60, status))
			}
			r.ingest(t, r.batch(events...))
			probes := []string{r.session.ProbeID, cloneHistoryProbe(t, r, 2, 3), cloneHistoryProbe(t, r, 3, 9)}

			store := repository.NewRegionalCommitStore(r.f.db)
			service := services.NewProbeHistoryService(store)
			for n := 0; ; n++ {
				if n > 100 {
					t.Fatal("history did not converge")
				}
				processed, err := store.ProcessHistoryWork(t.Context(), time.Now().UTC(), 100, service)
				if err != nil {
					t.Fatal(err)
				}
				if processed == 0 {
					break
				}
			}
			if remaining := replayCount(t, r.f, "probe_dirty_buckets"); remaining != 0 {
				t.Fatalf("dirty rows left: %d", remaining)
			}

			// Regional rows: the scoped evidence must give exactly what the
			// monitor-wide evidence gives.
			for _, probeID := range probes {
				for i := 0; i < minutes; i++ {
					bucket := r.at.Add(time.Duration(i) * time.Minute)
					evidence, err := store.ReadHistoryEvidence(t.Context(), r.monitor, bucket, bucket.Add(time.Minute), service)
					if err != nil {
						t.Fatal(err)
					}
					want, err := service.ProjectHistory(t.Context(), domain.ProbeHistoryWork{Bucket: domain.DirtyBucket{MonitorID: r.monitor, ProbeID: probeID, Resolution: domain.DirtyResolution1m, Bucket: bucket}, Evidence: evidence})
					if err != nil {
						t.Fatal(err)
					}
					var got repository.AggregateModel
					if err := r.f.db.NewSelect().Model(&got).ModelTableExpr("heartbeat_1m AS aggregate_model").Where("monitor_id = ? AND probe_id = ? AND bucket = ?", r.monitor, probeID, bucket).Scan(t.Context()); err != nil {
						t.Fatal(probeID, bucket, err)
					}
					w := want.Regional
					if got.TotalChecks != w.TotalChecks || got.UpCount != w.UpCount || got.DownCount != w.DownCount || got.UpUS != w.Durations.Up.Microseconds() || got.DownUS != w.Durations.Down.Microseconds() || got.UnknownUS != w.Durations.Unknown.Microseconds() {
						t.Errorf("probe %s minute %d: stored %+v, monitor-wide evidence gives %+v", probeID, i, got, *w)
					}
				}
			}

			// Overall rows: per-minute projection must agree with one
			// reconstruction of the whole window from all probes' evidence.
			from, to := r.at, r.at.Add(minutes*time.Minute)
			evidence, err := store.ReadHistoryEvidence(t.Context(), r.monitor, from, to, service)
			if err != nil {
				t.Fatal(err)
			}
			want, err := services.ReconstructOverallHistory(evidence)
			if err != nil {
				t.Fatal(err)
			}
			intervals, err := r.f.projections.ListHealthHistory(t.Context(), r.monitor, from, to)
			if err != nil {
				t.Fatal(err)
			}
			var got domain.HealthDurations
			for _, interval := range intervals {
				start, end := interval.From, interval.To
				if start.Before(from) {
					start = from
				}
				if end.After(to) {
					end = to
				}
				if !start.Before(end) {
					continue
				}
				switch interval.Status {
				case domain.StatusUp:
					got.Up += end.Sub(start)
				case domain.StatusDown:
					got.Down += end.Sub(start)
				default:
					got.Unknown += end.Sub(start)
				}
			}
			if got.Up != want.Durations.Up || got.Down != want.Durations.Down || got.Down == 0 {
				t.Fatalf("overall durations stored %+v, reconstruction %+v", got, want.Durations)
			}
		})
	}
}

// legacySeedQuery is the ROW_NUMBER() seed read replaced in #196. It ranks every
// observation before lookback and is kept here as the oracle.
const legacySeedQuery = `SELECT o.id FROM probe_observations AS o JOIN
 (SELECT id, ROW_NUMBER() OVER (PARTITION BY probe_id,assignment_generation,stream_id ORDER BY seq DESC,id DESC) AS position
 FROM probe_observations WHERE monitor_id = ? AND observed_at < ? AND probe_id IN (?) AND assignment_generation IN (?)) AS ranked ON ranked.id = o.id
 WHERE ranked.position = 1`

// The bounded seed read must pick exactly the rows the ranking query picked,
// including across stream restarts and a backward clock.
func TestProbeHistorySeedsMatchRankingQuery(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			events := make([]domain.ProbeReplayEvent, 0, 40)
			for i := int64(1); i <= 40; i++ {
				seconds := (i - 1) * 60
				if i == 30 {
					seconds = 10 * 60 // backward clock: a later seq stamped earlier
				}
				events = append(events, replayObservationAt(r, i, seconds, domain.StatusUp))
			}
			r.ingest(t, r.batch(events...))
			second := cloneHistoryProbe(t, r, 2, 20)
			// A restarted stream on the second probe that stops early.
			stmts := []string{
				"CREATE TEMPORARY TABLE restart_obs AS SELECT * FROM probe_observations WHERE probe_id = '" + second + "' AND seq <= 8",
				"UPDATE restart_obs SET id = id + 5000000, stream_id = '44444444-4444-4444-8444-0000000000aa'",
				"INSERT INTO probe_observations SELECT * FROM restart_obs",
				"DROP TABLE restart_obs",
			}
			for _, stmt := range stmts {
				if _, err := r.f.db.ExecContext(t.Context(), stmt); err != nil {
					t.Fatal(stmt, err)
				}
			}
			store := repository.NewRegionalCommitStore(r.f.db)
			service := services.NewProbeHistoryService(store)
			for _, minute := range []int{0, 5, 12, 25, 33, 45} {
				from := r.at.Add(time.Duration(minute) * time.Minute)
				to := from.Add(time.Minute)
				evidence, err := store.ReadHistoryEvidence(t.Context(), r.monitor, from, to, service)
				if err != nil {
					t.Fatal(err)
				}
				lookback := from.Add(-evidence.FreshFor)
				var want []int64
				if err := r.f.db.NewRaw(legacySeedQuery, r.monitor, lookback, bun.List([]string{r.session.ProbeID, second}), bun.List([]int64{1})).Scan(t.Context(), &want); err != nil {
					t.Fatal(err)
				}
				var got []int64
				for _, obs := range evidence.Observations {
					if obs.ObservedAt.Before(lookback) {
						got = append(got, obs.ID)
					}
				}
				slices.Sort(want)
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Fatalf("minute %d: seeds %v, ranking query %v", minute, got, want)
				}
				if minute >= 12 && len(want) < 3 {
					t.Fatalf("minute %d: fixture has too few seed streams: %v", minute, want)
				}
			}
		})
	}
}

type seedQueryHook struct {
	mu      sync.Mutex
	queries []string
}

func (h *seedQueryHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *seedQueryHook) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if strings.Contains(event.Query, "probe_observations") && strings.HasPrefix(event.Query, "SELECT") {
		h.mu.Lock()
		h.queries = append(h.queries, event.Query)
		h.mu.Unlock()
	}
}

// Observations are never pruned, so an item must not read the monitor's whole
// history. ANALYZE reports actual rows; elapsed time would be machine-dependent.
func TestProbeHistoryItemReadsBoundedByWindowMariaDB(t *testing.T) {
	r := oldHistoryFixture(t, "mariadb")
	const history = 6 * 60
	events := make([]domain.ProbeReplayEvent, 0, history)
	for i := int64(1); i <= history; i++ {
		events = append(events, replayObservationAt(r, i, (i-1)*60, domain.StatusUp))
	}
	for start := 0; start < history; start += 60 {
		r.ingest(t, r.batch(events[start:start+60]...))
	}
	cloneHistoryProbe(t, r, 2, history)
	// Keep only the newest minute's overall work: it reads both probes' evidence.
	last := r.at.Add((history - 1) * time.Minute)
	if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM probe_dirty_buckets WHERE resolution <> 'overall' OR bucket <> ?", last); err != nil {
		t.Fatal(err)
	}
	hook := new(seedQueryHook)
	attachInPlaceQueryHook(r.f.db, hook)
	store := repository.NewRegionalCommitStore(r.f.db)
	if n, err := store.ProcessHistoryWork(t.Context(), time.Now().UTC(), 1, services.NewProbeHistoryService(store)); err != nil || n != 1 {
		t.Fatal("process newest overall minute", n, err)
	}
	hook.mu.Lock()
	queries := slices.Clone(hook.queries)
	hook.mu.Unlock()
	if len(queries) == 0 {
		t.Fatal("no observation reads observed")
	}
	for _, query := range queries {
		var raw string
		if err := r.f.db.QueryRowContext(t.Context(), "ANALYZE FORMAT=JSON "+query).Scan(&raw); err != nil {
			t.Fatal(query, err)
		}
		var plan map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &plan); err != nil {
			t.Fatal(err)
		}
		var inspect func(interface{})
		inspect = func(value interface{}) {
			switch value := value.(type) {
			case map[string]interface{}:
				if name, _ := value["table_name"].(string); name != "" {
					// Two probes hold 2*history rows; a bounded read touches the
					// freshness window, not the whole history.
					if count, ok := value["r_rows"].(float64); ok && count > 20 {
						t.Errorf("%s read %v rows of %d history rows: %s", name, count, 2*history, query)
					}
				}
				for _, child := range value {
					inspect(child)
				}
			case []interface{}:
				for _, child := range value {
					inspect(child)
				}
			}
		}
		inspect(plan)
	}
}
