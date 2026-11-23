package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// replayStatementCounter records the statements one replay batch sends, so a
// test can tell per-batch work from per-event and per-monitor work.
type replayStatementCounter struct {
	mu        sync.Mutex
	on        atomic.Bool
	stmts     []string
	deadlocks atomic.Int64
	waits     atomic.Int64
}

func (c *replayStatementCounter) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

// replaySnapshotConflicts, set on a query context, counts MariaDB 1020
// snapshot-isolation conflicts raised by that caller's statements.
type replaySnapshotConflicts struct{}

func (c *replayStatementCounter) AfterQuery(ctx context.Context, e *bun.QueryEvent) {
	var err *mysql.MySQLError
	if errors.As(e.Err, &err) {
		switch err.Number {
		case 1020:
			if n, ok := ctx.Value(replaySnapshotConflicts{}).(*atomic.Int64); ok {
				n.Add(1)
			}
		case 1213:
			c.deadlocks.Add(1)
		case 1205:
			c.waits.Add(1)
		}
	}
	if !c.on.Load() {
		return
	}
	q := strings.NewReplacer("`", "", `"`, "").Replace(e.Query)
	c.mu.Lock()
	c.stmts = append(c.stmts, q)
	c.mu.Unlock()
}

func (c *replayStatementCounter) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stmts
	c.stmts = nil
	return out
}

var (
	replayDirtyStmt  = regexp.MustCompile(`^INSERT INTO probe_dirty_buckets\b`)
	replayLockStmt   = regexp.MustCompile(`^UPDATE monitor_probe_assignment_sets\b|\bFROM monitors( |$)`)
	replayStateStmt  = regexp.MustCompile(`\b(monitor_probe_state|probe_missing_state|tls_info)\b|\bFROM monitor_probe_assignments( |$)`)
	replayReplaceCat = []string{"dirty", "assignment_lock", "current_state"}
)

func classifyReplayStatements(stmts []string) map[string]int {
	out := map[string]int{"total": len(stmts)}
	for _, q := range stmts {
		switch {
		case replayDirtyStmt.MatchString(q):
			out["dirty"]++
		case replayLockStmt.MatchString(q):
			out["assignment_lock"]++
		case replayStateStmt.MatchString(q):
			out["current_state"]++
		}
	}
	return out
}

// bulkReplayMonitors assigns n monitors (the fixture's own first) to the
// fixture probe and returns them with the config revision that covers them.
func bulkReplayMonitors(t *testing.T, r replayFixture, n int) ([]int64, int64) {
	t.Helper()
	ctx := t.Context()
	ids := []int64{r.monitor}
	for len(ids) < n {
		m := r.f.monitor(t)
		if _, err := r.f.assignments.InitializeLocal(ctx, m); err != nil {
			t.Fatal(err)
		}
		if _, err := r.f.assignments.Replace(ctx, m, 1, []string{r.session.ProbeID}, domain.HealthPolicyAnyDown); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m)
	}
	if _, err := r.f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_history SET started_at = ?", r.at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	meta, err := r.syncer.RefreshRemote(ctx, syncTarget(), r.at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return ids, meta.Revision
}

func (r replayFixture) observationAt(seq, monitor, revision int64, at time.Time) domain.ProbeReplayEvent {
	e := r.observation(seq)
	e.Observation.MonitorID, e.Observation.ConfigRevision, e.Observation.ObservedAt = monitor, revision, at
	e.ObservedAt = at
	return e
}

// TestObservationReplayBatchStatementsPerBatch pins dirty marking, assignment
// authority and current-state application to a fixed number of statements per
// observation batch, whatever the number of events or monitors in it.
func TestObservationReplayBatchStatementsPerBatch(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			shapes := []struct{ monitors, events int }{{2, 4}, {6, 48}}
			got := make([]map[string]int, len(shapes))
			for i, shape := range shapes {
				r := newReplayFixture(t, engine)
				monitors, revision := bulkReplayMonitors(t, r, shape.monitors)
				events := make([]domain.ProbeReplayEvent, 0, shape.events)
				for seq := 1; seq <= shape.events; seq++ {
					at := r.at.Add(-time.Duration(shape.events-seq) * 100 * time.Millisecond)
					events = append(events, r.observationAt(int64(seq), monitors[seq%len(monitors)], revision, at))
				}
				counter := &replayStatementCounter{}
				attachInPlaceQueryHook(r.f.db, counter)
				counter.on.Store(true)
				result := r.ingest(t, r.batch(events...))
				counter.on.Store(false)
				if result.AcceptedCount != int64(shape.events) || len(result.Rejected) != 0 {
					t.Fatalf("shape %+v: %+v", shape, result)
				}
				got[i] = classifyReplayStatements(counter.take())
				t.Logf("monitors=%d events=%d statements: %v", shape.monitors, shape.events, got[i])
			}
			if got[0]["dirty"] != 1 {
				t.Errorf("dirty marking: %d statements per batch, want 1", got[0]["dirty"])
			}
			if got[0]["assignment_lock"] != 2 {
				t.Errorf("assignment authority: %d statements per batch, want 2", got[0]["assignment_lock"])
			}
			for _, category := range replayReplaceCat {
				if got[0][category] != got[1][category] {
					t.Errorf("%s statements grow with the batch: %d for %+v, %d for %+v", category, got[0][category], shapes[0], got[1][category], shapes[1])
				}
			}
		})
	}
}

// replayEndState dumps every row an observation batch writes, minus columns
// that carry the transaction clock or random tokens.
func replayEndState(t *testing.T, db *bun.DB) string {
	t.Helper()
	queries := []string{
		"SELECT monitor_id, probe_id, assignment_generation, stream_id, seq, config_revision, status, raw_status, down_count, ping, message, observed_at, tls_json FROM probe_observations ORDER BY seq",
		"SELECT seq, digest, kind, rejection_code FROM probe_telemetry_receipts ORDER BY seq",
		"SELECT monitor_id, probe_id, assignment_generation, stream_id, seq, config_revision, status, down_count, ping, message, observed_at, last_success_at, active_source_alert_id, tls_json, conditions_json FROM monitor_probe_state ORDER BY monitor_id, probe_id",
		"SELECT monitor_id, probe_id, stream_id, assignment_generation, snapshot_seq, reason FROM probe_missing_state ORDER BY monitor_id, probe_id",
		"SELECT monitor_id, probe_id, assignment_generation, info_json, checked_at FROM tls_info ORDER BY monitor_id, probe_id, assignment_generation",
		"SELECT monitor_id, probe_id, resolution, bucket FROM probe_dirty_buckets ORDER BY monitor_id, probe_id, resolution, bucket",
		"SELECT monitor_id, probe_id, cursor_at, observed_through FROM probe_history_ranges ORDER BY monitor_id, probe_id",
		"SELECT probe_id, stream_id, committed_seq FROM probe_streams ORDER BY probe_id, stream_id",
	}
	var b strings.Builder
	for _, q := range queries {
		rows, err := db.QueryContext(t.Context(), q)
		if err != nil {
			t.Fatal(q, err)
		}
		cols, _ := rows.Columns()
		fmt.Fprintf(&b, "%s\n", q)
		for rows.Next() {
			values := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range values {
				if v.Valid {
					fmt.Fprintf(&b, "%s=%s ", cols[i], v.String)
				} else {
					fmt.Fprintf(&b, "%s=NULL ", cols[i])
				}
			}
			b.WriteString("\n")
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// runMixedObservationReplay applies the same two observation batches: a seed,
// then a batch with a duplicate prefix, repeated monitors, an unknown monitor,
// a stale generation, a future sample, an older wall clock at a newer
// sequence, TLS set and cleared, a missing-state fence that suppresses one
// sample and is cleared by a later one, and a down sample whose last success
// comes from earlier in the same batch.
func runMixedObservationReplay(t *testing.T, engine string, at time.Time, perEvent bool) (string, []*domain.ProbeReplayResult) {
	t.Helper()
	r := newReplayFixtureAt(t, engine, at)
	repository.SetReplayPerEventForTest(r.store, perEvent)
	monitors, revision := bulkReplayMonitors(t, r, 3)
	a, b, c := monitors[0], monitors[1], monitors[2]
	// Assignment setup marks dirty buckets at the wall-clock minute it runs.
	// The two compared runs are set up at different moments and can straddle
	// a minute, so only the buckets the batches mark are compared.
	if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM probe_dirty_buckets"); err != nil {
		t.Fatal(err)
	}
	tls := func(e domain.ProbeReplayEvent, days int) domain.ProbeReplayEvent {
		e.Observation.TLS = &domain.TLSObservation{NotAfter: at.Add(time.Duration(days) * 24 * time.Hour).Truncate(time.Second), DaysRemaining: int64(days), Issuer: "Fixture CA"}
		return e
	}
	down := func(e domain.ProbeReplayEvent, count int) domain.ProbeReplayEvent {
		e.Observation.Status, e.Observation.RawStatus, e.Observation.DownCount, e.Observation.Message = domain.StatusDown, domain.StatusDown, count, "fixture down"
		return e
	}
	seed := []domain.ProbeReplayEvent{
		tls(r.observationAt(1, a, revision, at.Add(-50*time.Second)), 30),
		r.observationAt(2, b, revision, at.Add(-49*time.Second)),
		r.observationAt(3, c, revision, at.Add(-48*time.Second)),
	}
	results := make([]*domain.ProbeReplayResult, 0, 3)
	results = append(results, r.ingest(t, r.batch(seed...)))
	// A stream-reset fence on b covers sequence 4 but not 9.
	if _, err := r.f.db.ExecContext(t.Context(), "INSERT INTO probe_missing_state (monitor_id, probe_id, stream_id, assignment_generation, config_revision, snapshot_seq, reason, created_at, applied_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		b, r.session.ProbeID, r.session.StreamID, 1, revision, 5, "stream_reset", at, at); err != nil {
		t.Fatal(err)
	}
	stale := r.observationAt(8, c, revision, at.Add(-30*time.Second))
	stale.Observation.AssignmentGeneration = 2
	future := r.observationAt(10, c, revision, at.Add(time.Hour))
	batch := append(append([]domain.ProbeReplayEvent{}, seed...),
		r.observationAt(4, b, revision, at.Add(-45*time.Second)),
		down(tls(r.observationAt(5, a, revision, at.Add(-20*time.Second)), 29), 1),
		r.observationAt(6, a, revision, at.Add(-40*time.Second)),
		r.observationAt(7, a+1000, revision, at.Add(-39*time.Second)),
		stale,
		tls(r.observationAt(9, b, revision, at.Add(-15*time.Second)), 60),
		future,
		tls(r.observationAt(11, a, revision, at.Add(-10*time.Second)), 28),
		down(r.observationAt(12, c, revision, at.Add(-5*time.Second)), 2),
		r.observationAt(13, c, revision, at.Add(-4*time.Second)),
		// b ends down: its last success is sequence 9 in this batch, not the seed.
		down(r.observationAt(14, b, revision, at.Add(-3*time.Second)), 1),
	)
	results = append(results, r.ingest(t, r.batch(batch...)))
	// Replaying the whole batch again is all duplicates: nothing to write.
	results = append(results, r.ingest(t, r.batch(batch...)))
	return replayEndState(t, r.f.db), results
}

// TestObservationReplayBatchedMatchesPerEvent applies the same mixed batches
// through the per-event path and the batched path and requires identical
// outcomes and stored rows. The per-event path here is not the pre-change
// baseline: it still takes the bulk assignment lock and marks dirty buckets
// with one multi-row statement per event (639 statements for the 48-event
// shape, where main sent 793). It isolates the deferred current-state and
// dirty-bucket writes, which is what this test compares.
func TestObservationReplayBatchedMatchesPerEvent(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			at := time.Now().UTC().Truncate(time.Second).Add(-5 * time.Second)
			wantState, wantResults := runMixedObservationReplay(t, engine, at, true)
			gotState, gotResults := runMixedObservationReplay(t, engine, at, false)
			for i := range wantResults {
				if fmt.Sprintf("%+v", wantResults[i]) != fmt.Sprintf("%+v", gotResults[i]) {
					t.Errorf("batch %d outcome: per-event %+v, batched %+v", i, wantResults[i], gotResults[i])
				}
			}
			if wantState != gotState {
				t.Errorf("end state differs\n--- per-event\n%s\n--- batched\n%s", wantState, gotState)
			}
			// The fixture must reach the branches it is meant to cover.
			for _, want := range []string{"Fixture CA", "cursor_at=", "resolution=overall"} {
				if !strings.Contains(gotState, want) {
					t.Fatalf("fixture end state lacks %q:\n%s", want, gotState)
				}
			}
			if strings.Contains(gotState, "reason=stream_reset") {
				t.Fatalf("a later sample did not clear the missing-state fence:\n%s", gotState)
			}
			t.Logf("outcomes: %+v %+v %+v", *gotResults[0], *gotResults[1], *gotResults[2])
		})
	}
}

// TestObservationReplayConcurrentWithAssignmentReplace runs observation
// batches against no-op assignment replacements of the same monitors. Replace
// takes the local registration exclusively and every assignment row the batch
// locks; neither side may deadlock, time out or lose a sample.
func TestObservationReplayConcurrentWithAssignmentReplace(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			monitors, revision := bulkReplayMonitors(t, r, 6)
			counter := &replayStatementCounter{}
			attachInPlaceQueryHook(r.f.db, counter)
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			const batches, perBatch = 12, 32
			stop := make(chan struct{})
			var replaces atomic.Int64
			replaceErr := make(chan error, len(monitors))
			var wg sync.WaitGroup
			for _, m := range monitors {
				wg.Add(1)
				go func(m int64) {
					defer wg.Done()
					expected := int64(2)
					for {
						select {
						case <-stop:
							replaceErr <- nil
							return
						default:
						}
						got, err := r.f.assignments.Replace(ctx, m, expected, []string{r.session.ProbeID}, domain.HealthPolicyAnyDown)
						if err != nil {
							replaceErr <- fmt.Errorf("replace monitor %d at revision %d: %w", m, expected, err)
							return
						}
						expected = got.Revision
						replaces.Add(1)
					}
				}(m)
			}
			seq := int64(0)
			last := map[int64]int64{}
			for i := 0; i < batches; i++ {
				events := make([]domain.ProbeReplayEvent, 0, perBatch)
				for j := 0; j < perBatch; j++ {
					seq++
					m := monitors[(i*7+j*5)%len(monitors)]
					// Descending monitor order inside each batch exercises the sorted lock set.
					if i%2 == 1 {
						m = monitors[len(monitors)-1-(j%len(monitors))]
					}
					events = append(events, r.observationAt(seq, m, revision, r.at.Add(-time.Duration(batches*perBatch-int(seq))*10*time.Millisecond)))
					last[m] = seq
				}
				result, err := r.store.IngestReplayBatch(ctx, r.session, r.batch(events...), &services.AccessService{})
				if err != nil {
					close(stop)
					wg.Wait()
					t.Fatalf("batch %d: %v (deadlocks=%d lock_wait_timeouts=%d)", i, err, counter.deadlocks.Load(), counter.waits.Load())
				}
				if result.AcceptedCount != perBatch || len(result.Rejected) != 0 {
					close(stop)
					wg.Wait()
					t.Fatalf("batch %d: %+v", i, result)
				}
			}
			close(stop)
			wg.Wait()
			close(replaceErr)
			for err := range replaceErr {
				if err != nil {
					t.Fatal(err)
				}
			}
			if counter.deadlocks.Load() != 0 || counter.waits.Load() != 0 {
				t.Fatalf("deadlocks=%d lock_wait_timeouts=%d", counter.deadlocks.Load(), counter.waits.Load())
			}
			for m, want := range last {
				var got int64
				if err := r.f.db.NewRaw("SELECT seq FROM monitor_probe_state WHERE monitor_id = ? AND probe_id = ?", m, r.session.ProbeID).Scan(ctx, &got); err != nil || got != want {
					t.Fatalf("monitor %d state seq=%d want %d (%v)", m, got, want, err)
				}
			}
			t.Logf("batches=%d events=%d replaces=%d deadlocks=0 lock_wait_timeouts=0", batches, seq, replaces.Load())
		})
	}
}

// secondReplayProbe assigns monitors to a second remote probe alongside the
// fixture probe and opens a replay session for it. It returns the fixture for
// that probe, each monitor's assignment generation for it, its config
// revision, the fixture probe's new config revision, and each monitor's
// assignment set revision.
func secondReplayProbe(t *testing.T, r replayFixture, monitors []int64) (replayFixture, map[int64]int64, int64, int64, map[int64]int64) {
	t.Helper()
	ctx := t.Context()
	const probeID = "99999999-9999-4999-8999-999999999999"
	r.f.remote(t, probeID, "edge-2")
	setRevisions := map[int64]int64{}
	for _, m := range monitors {
		set, err := r.f.assignments.Replace(ctx, m, 2, []string{r.session.ProbeID, probeID}, domain.HealthPolicyAnyDown)
		if err != nil {
			t.Fatalf("add second probe to monitor %d: %v", m, err)
		}
		setRevisions[m] = set.Revision
	}
	generations := map[int64]int64{}
	for _, m := range monitors {
		var gen int64
		if err := r.f.db.NewRaw("SELECT generation FROM monitor_probe_assignments WHERE monitor_id = ? AND probe_id = ?", m, probeID).Scan(ctx, &gen); err != nil {
			t.Fatal(err)
		}
		generations[m] = gen
	}
	if _, err := r.f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_history SET started_at = ?", r.at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	first, err := r.syncer.RefreshRemote(ctx, syncTarget(), r.at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.syncer.RefreshRemote(ctx, domain.ProbeConfigTarget{HubID: probeRegistryID2, ProbeID: probeID}, r.at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	connections := repository.NewProbeConnectorStore(r.f.db)
	metadata := domain.ProbeCredentialMetadata{HubID: probeRegistryID2, ProbeID: probeID, StreamID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", EnrollmentID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", CredentialVersion: 1, Endpoint: "wss://edge-2.example/ws/probe/v1", Fingerprint: strings.Repeat("b", 64)}
	if _, err := connections.PrepareConnection(ctx, domain.ProbeConnection{ProbeCredentialMetadata: metadata, ProtectedCredential: make([]byte, 40), State: "prepared", PreparedAt: r.at}); err != nil {
		t.Fatal(err)
	}
	lease, err := connections.AcquireConnector(ctx, probeID, probeRegistryID3)
	if err != nil {
		t.Fatal(err)
	}
	other := r
	other.session = domain.ProbeReplaySession{HubID: metadata.HubID, ProbeID: probeID, StreamID: metadata.StreamID, OwnerID: lease.OwnerID, ConnectionGeneration: lease.Generation}
	return other, generations, second.Revision, first.Revision, setRevisions
}

// TestObservationReplayTwoProbesConcurrentWithAssignmentReplace has two probes
// ingest observation batches for the same monitors and minute buckets, in
// opposite monitor orders, while assignment.replace rewrites every monitor's
// set. On MariaDB this is the shape that could deadlock (1213) if the replay
// locks (remote probe, local probe, sorted assignment sets, sorted dirty
// buckets) and replace's sorted probe locks disagreed on order. A batch that
// loses a snapshot-isolation race on the shared buckets (1020) is resent, as a
// probe does; deadlocks, lock-wait timeouts and other errors fail the test.
func TestObservationReplayTwoProbesConcurrentWithAssignmentReplace(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			monitors, _ := bulkReplayMonitors(t, r, 6)
			r2, generations2, revision2, revision1, setRevisions := secondReplayProbe(t, r, monitors)
			if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM probe_dirty_buckets"); err != nil {
				t.Fatal(err)
			}
			counter := &replayStatementCounter{}
			attachInPlaceQueryHook(r.f.db, counter)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			const batches, perBatch = 12, 32
			stop := make(chan struct{})
			var replaces atomic.Int64
			errs := make(chan error, len(monitors)+2)
			var replacers sync.WaitGroup
			for _, m := range monitors {
				replacers.Add(1)
				go func(m int64) {
					defer replacers.Done()
					expected := setRevisions[m]
					for {
						select {
						case <-stop:
							return
						default:
						}
						got, err := r.f.assignments.Replace(ctx, m, expected, []string{r.session.ProbeID, r2.session.ProbeID}, domain.HealthPolicyAnyDown)
						if err != nil {
							errs <- fmt.Errorf("replace monitor %d at revision %d: %w", m, expected, err)
							return
						}
						expected = got.Revision
						replaces.Add(1)
					}
				}(m)
			}
			type probeRun struct {
				r           replayFixture
				revision    int64
				generations map[int64]int64
				descending  bool
				last        map[int64]int64
				conflicts   atomic.Int64
				resends     int
			}
			runs := []*probeRun{{r: r, revision: revision1, descending: false, last: map[int64]int64{}}, {r: r2, revision: revision2, generations: generations2, descending: true, last: map[int64]int64{}}}
			var ingesters sync.WaitGroup
			for _, run := range runs {
				ingesters.Add(1)
				go func(run *probeRun) {
					defer ingesters.Done()
					seq := int64(0)
					for i := 0; i < batches; i++ {
						events := make([]domain.ProbeReplayEvent, 0, perBatch)
						for j := 0; j < perBatch; j++ {
							seq++
							k := (i*7 + j*5) % len(monitors)
							if run.descending {
								k = len(monitors) - 1 - (j % len(monitors))
							}
							m := monitors[k]
							// Both probes report the same instants, so they mark the same minute buckets.
							e := run.r.observationAt(seq, m, run.revision, r.at.Add(-time.Duration(batches*perBatch-int(seq))*10*time.Millisecond))
							if run.generations != nil {
								e.Observation.AssignmentGeneration = run.generations[m]
							}
							events = append(events, e)
							run.last[m] = seq
						}
						batch := run.r.batch(events...)
						var result *domain.ProbeReplayResult
						for {
							before := run.conflicts.Load()
							var err error
							result, err = run.r.store.IngestReplayBatch(context.WithValue(ctx, replaySnapshotConflicts{}, &run.conflicts), run.r.session, batch, &services.AccessService{})
							if err == nil {
								break
							}
							// Both probes upsert the same local overall buckets. Under
							// MariaDB snapshot isolation the loser of that race gets
							// 1020 and, once the bounded transaction retries are spent,
							// the batch fails whole; a probe resends it. Anything else
							// is a failure.
							if run.conflicts.Load() == before || run.resends >= 200 {
								errs <- fmt.Errorf("probe %s batch %d after %d resends: %w", run.r.session.ProbeID, i, run.resends, err)
								return
							}
							run.resends++
							time.Sleep(time.Duration(1+run.resends%5) * time.Millisecond)
						}
						if result.AcceptedCount != perBatch || len(result.Rejected) != 0 {
							errs <- fmt.Errorf("probe %s batch %d: %+v", run.r.session.ProbeID, i, result)
							return
						}
					}
				}(run)
			}
			ingesters.Wait()
			close(stop)
			replacers.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("%v (deadlocks=%d lock_wait_timeouts=%d)", err, counter.deadlocks.Load(), counter.waits.Load())
			}
			if t.Failed() {
				return
			}
			if counter.deadlocks.Load() != 0 || counter.waits.Load() != 0 {
				t.Fatalf("deadlocks=%d lock_wait_timeouts=%d", counter.deadlocks.Load(), counter.waits.Load())
			}
			for _, run := range runs {
				for m, want := range run.last {
					var got int64
					if err := r.f.db.NewRaw("SELECT seq FROM monitor_probe_state WHERE monitor_id = ? AND probe_id = ?", m, run.r.session.ProbeID).Scan(ctx, &got); err != nil || got != want {
						t.Fatalf("monitor %d probe %s state seq=%d want %d (%v)", m, run.r.session.ProbeID, got, want, err)
					}
				}
			}
			for _, m := range monitors {
				for _, probeID := range []string{r.session.ProbeID, r2.session.ProbeID, domain.LocalProbeID} {
					var n int
					if err := r.f.db.NewRaw("SELECT COUNT(*) FROM probe_dirty_buckets WHERE monitor_id = ? AND probe_id = ?", m, probeID).Scan(ctx, &n); err != nil || n == 0 {
						t.Fatalf("monitor %d probe %s dirty buckets=%d (%v)", m, probeID, n, err)
					}
				}
			}
			t.Logf("probes=2 batches=%d events=%d replaces=%d deadlocks=0 lock_wait_timeouts=0 snapshot_conflicts=%d resends=%d", 2*batches, 2*batches*perBatch, replaces.Load(), runs[0].conflicts.Load()+runs[1].conflicts.Load(), runs[0].resends+runs[1].resends)
		})
	}
}
