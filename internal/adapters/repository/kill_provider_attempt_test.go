package repository_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// M6-F: restart/kill at the provider-attempt boundary, with a real process
// kill instead of a simulated crash. A child copy of this test binary opens the
// parent's database, runs one production aggregate delivery pass and is
// SIGKILLed: either while the provider call is in progress, or at a random
// moment of the pass. No product hook is involved: the pause point is the
// provider port itself. The parent then restarts a cold service over the same
// database and checks that the page went out at most once, and that a page
// whose provider was never called is still delivered.

const killChildEnv = "PHOENIX_KILL_CHILD"

// killIterations is the M6-F floor of 20 random kills per engine, raised with
// PHOENIX_KILL_ITERATIONS for longer local sweeps.
func killIterations() int {
	if n, err := strconv.Atoi(os.Getenv("PHOENIX_KILL_ITERATIONS")); err == nil && n > 20 {
		return n
	}
	return 20
}

// markerSender writes SEND_START to stdout before it does anything else, so the
// parent knows a provider call may have happened. With block set it never
// returns, which holds the child inside the provider attempt.
type markerSender struct{ block bool }

func (markerSender) Type() string                  { return "webhook" }
func (markerSender) Validate(map[string]any) error { return nil }
func (s markerSender) Send(ctx context.Context, _ map[string]any, _ domain.AlertContext) error {
	if _, err := os.Stdout.WriteString("SEND_START\n"); err != nil {
		return err
	}
	if s.block {
		<-ctx.Done()
		select {}
	}
	return nil
}

// TestKillProviderAttemptChild is the child half. It does nothing unless the
// parent started it.
func TestKillProviderAttemptChild(t *testing.T) {
	mode := os.Getenv(killChildEnv)
	if mode == "" {
		t.Skip("child process of TestKillProviderAttempt")
	}
	f := reopenKillFixture(t)
	store := newOperativeStore(t, f)
	sender := markerSender{block: mode == "in-send"}
	svc, err := services.NewOperativeDeliveryService(store, store, store, operativeOwner, true, func(string) (ports.NotificationSender, bool) { return sender, true })
	if err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.WriteString("READY\n")
	for {
		if _, err := svc.ProcessNext(context.Background()); err != nil {
			_, _ = fmt.Fprintf(os.Stdout, "ERR %v\n", err)
		}
		_, _ = os.Stdout.WriteString("PASS_DONE\n")
		time.Sleep(time.Millisecond)
	}
}

// reopenKillFixture opens the parent's database in a child without migrating
// or resetting it.
func reopenKillFixture(t *testing.T) probeRegistryFixture {
	t.Helper()
	engine, dsn := os.Getenv("PHOENIX_KILL_ENGINE"), os.Getenv("PHOENIX_KILL_DSN")
	f := probeRegistryFixture{engine: engine, dsn: dsn}
	var err error
	if engine == "sqlite" {
		f.db, err = sqlite.NewDB(dsn)
	} else {
		f.db, err = mariadb.NewDB(dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type killResult struct {
	sawSend bool
	lines   []string
	// passDone is when the first PASS_DONE arrived, measured from READY.
	passDone time.Duration
}

// runKilledChild starts the child, kills it on the trigger, and returns what
// it printed before it died.
func runKilledChild(t *testing.T, o operativeFixture, mode string, delay time.Duration) killResult {
	t.Helper()
	return spawnKilledChild(t, "TestKillProviderAttemptChild", o.p.f, mode, delay, nil)
}

// spawnKilledChild runs one child test against the fixture's database and
// SIGKILLs it: after the marker for mode "in-send"/"calibrate", otherwise after
// delay from READY. Lines the child wrote before it died are returned.
func spawnKilledChild(t *testing.T, child string, f probeRegistryFixture, mode string, delay time.Duration, env []string) killResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+child+"$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), killChildEnv+"="+mode, "PHOENIX_KILL_ENGINE="+f.engine, "PHOENIX_KILL_DSN="+f.dsn), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var res killResult
	var ready time.Time
	wait := func(want string) {
		deadline := time.After(60 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("child exited before %s: %v / %s", want, res.lines, stderr.String())
				}
				res.lines = append(res.lines, l)
				switch l {
				case "SEND_START":
					res.sawSend = true
				case "READY":
					ready = time.Now()
				case "PASS_DONE":
					if res.passDone == 0 {
						res.passDone = time.Since(ready)
					}
				}
				if l == want {
					return
				}
			case <-deadline:
				_ = cmd.Process.Kill()
				t.Fatalf("child never printed %s: %v / %s", want, res.lines, stderr.String())
			}
		}
	}
	wait("READY")
	switch mode {
	case "in-send":
		wait("SEND_START")
	case "calibrate":
		wait("PASS_DONE")
	case "after-commit":
		wait("COMMITTED")
	default:
		time.Sleep(delay)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Drain before Wait: Wait closes the pipe, which would drop lines the
	// child wrote before SIGKILL that are still in the pipe buffer.
	for l := range lines {
		res.lines = append(res.lines, l)
		if l == "SEND_START" {
			res.sawSend = true
		}
	}
	_ = cmd.Wait()
	return res
}

// recoverAfterKill ages the dead child's lease and provider budget, which is
// what wall-clock time would do, then runs a cold service and reports how many
// pages it sent.
func recoverAfterKill(t *testing.T, o operativeFixture) int {
	t.Helper()
	ctx := t.Context()
	past := time.Now().UTC().Add(-3 * time.Minute)
	if _, err := o.p.f.db.ExecContext(ctx, "UPDATE operative_aggregate_deliveries SET leased_at=?, lease_until=? WHERE lease_until IS NOT NULL", past.Add(-time.Minute), past); err != nil {
		t.Fatal(err)
	}
	if _, err := o.p.f.db.ExecContext(ctx, "UPDATE operative_provider_attempts SET budget_until=? WHERE state=?", past, domain.ProviderAttemptInFlight); err != nil {
		t.Fatal(err)
	}
	cold := newOperativeStore(t, o.p.f)
	sender := &restartRecordingSender{db: o.p.f.db}
	svc, err := services.NewOperativeDeliveryService(cold, cold, cold, operativeOwner, true, func(string) (ports.NotificationSender, bool) { return sender, true })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.ProcessNext(ctx); err != nil {
			t.Fatalf("restart pass %d: %v", i, err)
		}
	}
	if _, err := cold.ClaimAggregateDelivery(ctx, operativeOwner, 30*time.Second); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("terminal queue reclaims work after recovery: %v", err)
	}
	return len(sender.alerts)
}

type killDeliveryState struct {
	Status    string
	ErrorCode string
	Lease     bool
	Attempts  int
	InFlight  int
	Ambiguous int
	Succeeded int
}

func readKillDeliveryState(t *testing.T, o operativeFixture) killDeliveryState {
	t.Helper()
	ctx := t.Context()
	var s killDeliveryState
	var row struct {
		Status     string
		ErrorCode  string
		LeaseUntil *time.Time
	}
	if err := o.p.f.db.NewSelect().Table("operative_aggregate_deliveries").Column("status", "error_code", "lease_until").Limit(1).Scan(ctx, &row); err != nil {
		t.Fatal(err)
	}
	s.Status, s.ErrorCode, s.Lease = row.Status, row.ErrorCode, row.LeaseUntil != nil
	var states []string
	if err := o.p.f.db.NewSelect().Table("operative_provider_attempts").Column("state").Where("scope = ?", "aggregate").Scan(ctx, &states); err != nil {
		t.Fatal(err)
	}
	s.Attempts = len(states)
	for _, st := range states {
		switch domain.ProviderAttemptState(st) {
		case domain.ProviderAttemptInFlight:
			s.InFlight++
		case domain.ProviderAttemptAmbiguous:
			s.Ambiguous++
		case domain.ProviderAttemptSucceeded:
			s.Succeeded++
		}
	}
	return s
}

func newKillDeliveryFixture(t *testing.T, engine string) operativeFixture {
	t.Helper()
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	awaitOperativeObservationClock(t, driveOperativeActive(t, o))
	o.record(t, domain.StatusDown)
	o.run(t, true)
	return o
}

// TestKillProviderAttempt kills a real process inside the provider call, then
// at random points of a delivery pass, on both hub engines.
func TestKillProviderAttempt(t *testing.T) {
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
			t.Run("in-send", func(t *testing.T) {
				o := newKillDeliveryFixture(t, engine)
				res := runKilledChild(t, o, "in-send", 0)
				dead := readKillDeliveryState(t, o)
				if !res.sawSend || dead.InFlight != 1 || dead.Status != domain.DeliveryStatusLeased {
					t.Fatalf("kill inside Send left %+v (saw send %v)", dead, res.sawSend)
				}
				if sent := recoverAfterKill(t, o); sent != 0 {
					t.Fatalf("recovery re-sent %d pages after a provider call of unknown outcome", sent)
				}
				got := readKillDeliveryState(t, o)
				if got.Status != domain.DeliveryStatusFailed || got.ErrorCode != "provider_outcome_unknown" || got.Lease || got.InFlight != 0 || got.Ambiguous != 1 {
					t.Fatalf("after recovery: %+v", got)
				}
			})
			// Calibrate: one full pass, killed only after it finished, gives the
			// window that random kills are spread over.
			cal := newKillDeliveryFixture(t, engine)
			calRes := runKilledChild(t, cal, "calibrate", 0)
			if !calRes.sawSend || recoverAfterKill(t, cal) != 0 || readKillDeliveryState(t, cal).Status != domain.DeliveryStatusSent {
				t.Fatalf("uninterrupted pass did not deliver once: %v", calRes.lines)
			}
			window := calRes.passDone * 3 / 2
			t.Logf("one delivery pass takes %v; random kills spread over %v", calRes.passDone, window)
			seed := time.Now().UnixNano()
			rng := rand.New(rand.NewSource(seed))
			t.Logf("random kill seed %d", seed)
			counts := map[string]int{}
			iterations := killIterations()
			for i := 0; i < iterations; i++ {
				o := newKillDeliveryFixture(t, engine)
				delay := time.Duration(rng.Int63n(int64(window) + 1))
				res := runKilledChild(t, o, "random", delay)
				dead := readKillDeliveryState(t, o)
				sent := recoverAfterKill(t, o)
				got := readKillDeliveryState(t, o)
				childPages := 0
				if res.sawSend {
					childPages = 1
				}
				// Safety: never two pages for one delivery.
				if childPages+sent > 1 {
					t.Fatalf("iter %d delay %v: duplicate page (child %d, recovery %d); dead %+v", i, delay, childPages, sent, dead)
				}
				// Liveness: if no provider attempt was granted before the kill, the
				// page must still go out exactly once.
				if dead.Attempts == 0 && sent != 1 {
					t.Fatalf("iter %d delay %v: ungranted delivery lost (recovery sent %d)", i, delay, sent)
				}
				// A granted attempt whose provider was never called is treated as
				// unknown and never replayed; that is the documented conservative rule.
				if got.Lease || got.InFlight != 0 {
					t.Fatalf("iter %d: recovery left lease/in-flight: %+v", i, got)
				}
				switch {
				case got.Status == domain.DeliveryStatusSent:
					if childPages+sent != 1 {
						t.Fatalf("iter %d: status sent but %d pages", i, childPages+sent)
					}
				case got.Status == domain.DeliveryStatusFailed && got.ErrorCode == "provider_outcome_unknown":
					if got.Ambiguous < 1 {
						t.Fatalf("iter %d: unknown outcome without an ambiguous attempt: %+v", i, got)
					}
				default:
					t.Fatalf("iter %d delay %v: unexpected final state %+v", i, delay, got)
				}
				key := fmt.Sprintf("granted=%v child_send=%v final=%s", dead.Attempts > 0, res.sawSend, got.Status)
				counts[key]++
			}
			t.Logf("%d random kills: %v", iterations, counts)
		})
	}
}
