package repository_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// M6-F: restart/kill at the hub transaction boundary. A child process ingests
// remote telemetry batches in a loop and is SIGKILLed at a random moment,
// often inside the ingest transaction. The parent checks that every batch is
// all there or all absent, that no acknowledged batch was lost, and that
// replaying from the probe's own view (its last acknowledged sequence) leaves
// exactly one row per sequence.

const killIngestBatch = 5

func killIngestRepo(f probeRegistryFixture) ports.ProbeIngestRepository {
	if f.engine == "sqlite" {
		return sqlite.NewRegionalCommitRepo(f.db)
	}
	return mariadb.NewRegionalCommitRepo(f.db)
}

func killIngestBatchFrom(monitorID int64, probeID string, from int64) domain.ProbeIngestBatch {
	at := time.Now().UTC().Add(-time.Hour)
	events := make([]domain.RegionalObservation, 0, killIngestBatch)
	for i := int64(0); i < killIngestBatch; i++ {
		seq := from + i
		status := domain.StatusUp
		if (seq/7)%2 == 1 {
			status = domain.StatusDown
		}
		events = append(events, regionalSample(monitorID, probeID, remoteStreamID, 1, seq, status, int(seq%3), at.Add(time.Duration(seq)*time.Second)))
	}
	return domain.ProbeIngestBatch{ProbeID: probeID, StreamID: remoteStreamID, FromSeq: from, ThroughSeq: from + killIngestBatch - 1, Events: events}
}

// TestKillHubIngestChild is the child half: ingest from the durable cursor on,
// print "C <seq>" after each acknowledged commit.
func TestKillHubIngestChild(t *testing.T) {
	if os.Getenv(killChildEnv) == "" {
		t.Skip("child process of TestKillHubIngest")
	}
	f := reopenKillFixture(t)
	monitorID, _ := strconv.ParseInt(os.Getenv("PHOENIX_KILL_MONITOR"), 10, 64)
	probeID := os.Getenv("PHOENIX_KILL_PROBE")
	repo := killIngestRepo(f)
	ctx := context.Background()
	cursor, err := repo.GetCursor(ctx, probeID, remoteStreamID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.WriteString("READY\n")
	for {
		committed, err := repo.Ingest(ctx, killIngestBatchFrom(monitorID, probeID, cursor+1))
		if err != nil {
			_, _ = fmt.Fprintf(os.Stdout, "ERR %v\n", err)
			select {}
		}
		cursor = committed
		_, _ = fmt.Fprintf(os.Stdout, "C %d\n", committed)
	}
}

func lastAcked(lines []string) int64 {
	var acked int64
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "C "); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			acked = n
		}
		if strings.HasPrefix(l, "ERR ") {
			return -1
		}
	}
	return acked
}

type killIngestCounts struct {
	cursor, observations, distinct, maxSeq, stateSeq int64
}

func readKillIngest(t *testing.T, f probeRegistryFixture, probeID string) killIngestCounts {
	t.Helper()
	ctx := t.Context()
	var c killIngestCounts
	var err error
	if c.cursor, err = killIngestRepo(f).GetCursor(ctx, probeID, remoteStreamID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(ctx, "SELECT COUNT(*), COUNT(DISTINCT seq), COALESCE(MAX(seq),0) FROM probe_observations WHERE probe_id=? AND stream_id=?", probeID, remoteStreamID).Scan(&c.observations, &c.distinct, &c.maxSeq); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM monitor_probe_state WHERE probe_id=? AND stream_id=?", probeID, remoteStreamID).Scan(&c.stateSeq); err != nil {
		t.Fatal(err)
	}
	return c
}

func checkKillIngest(t *testing.T, label string, c killIngestCounts) {
	t.Helper()
	if c.cursor%killIngestBatch != 0 {
		t.Fatalf("%s: cursor %d is inside a batch (partial commit)", label, c.cursor)
	}
	if c.observations != c.cursor || c.distinct != c.cursor || c.maxSeq != c.cursor {
		t.Fatalf("%s: cursor %d but observations=%d distinct=%d max=%d", label, c.cursor, c.observations, c.distinct, c.maxSeq)
	}
	if c.stateSeq != c.cursor {
		t.Fatalf("%s: current state at seq %d, cursor %d", label, c.stateSeq, c.cursor)
	}
}

func TestKillHubIngest(t *testing.T) {
	if os.Getenv(killChildEnv) != "" {
		t.Skip("parent only")
	}
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			if engine == "mariadb" && os.Getenv("TEST_MARIADB_DSN") == "" {
				t.Skip("TEST_MARIADB_DSN is unset")
			}
			f := newProbeRegistryFixture(t, engine)
			ctx := t.Context()
			monitorID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			remote := f.remote(t, probeRegistryID1, "kill-region")
			if _, err := f.assignments.Replace(ctx, monitorID, 1, []string{"local", remote.ID}, domain.HealthPolicyAnyDown); err != nil {
				t.Fatal(err)
			}
			env := []string{"PHOENIX_KILL_MONITOR=" + strconv.FormatInt(monitorID, 10), "PHOENIX_KILL_PROBE=" + remote.ID}
			seed := time.Now().UnixNano()
			rng := rand.New(rand.NewSource(seed))
			t.Logf("random kill seed %d", seed)
			iterations := killIterations()
			var midBatch, killedAfterAck int
			for i := 0; i < iterations; i++ {
				before := readKillIngest(t, f, remote.ID)
				delay := time.Duration(rng.Int63n(int64(150 * time.Millisecond)))
				res := spawnKilledChild(t, "TestKillHubIngestChild", f, "random", delay, env)
				acked := lastAcked(res.lines)
				if acked < 0 {
					t.Fatalf("iter %d: child ingest failed: %v", i, res.lines)
				}
				after := readKillIngest(t, f, remote.ID)
				checkKillIngest(t, fmt.Sprintf("iter %d after kill", i), after)
				if acked > after.cursor {
					t.Fatalf("iter %d: child was told %d committed, durable cursor is %d", i, acked, after.cursor)
				}
				if acked == 0 {
					acked = before.cursor
				}
				switch d := after.cursor - acked; d {
				case 0:
					killedAfterAck++
				case killIngestBatch:
					midBatch++ // committed, killed before the ack reached the probe
				default:
					t.Fatalf("iter %d: cursor %d is %d past the last ack %d (before %d, %d lines, tail %v)", i, after.cursor, d, acked, before.cursor, len(res.lines), res.lines[max(0, len(res.lines)-4):])
				}
				// The probe resends from its last acknowledged sequence.
				replay := killIngestBatchFrom(monitorID, remote.ID, acked+1)
				committed, err := killIngestRepo(f).Ingest(ctx, replay)
				if err != nil || committed != acked+killIngestBatch && committed != after.cursor {
					t.Fatalf("iter %d: replay from %d: %d, %v", i, acked+1, committed, err)
				}
				checkKillIngest(t, fmt.Sprintf("iter %d after replay", i), readKillIngest(t, f, remote.ID))
			}
			final := readKillIngest(t, f, remote.ID)
			t.Logf("%d kills: final cursor %d, %d observations; commit-without-ack %d, other %d", iterations, final.cursor, final.observations, midBatch, killedAfterAck)
		})
	}
}
