package mariadb

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// fakeClock records the waits the retry loop asks for without sleeping.
type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return nil
}

var refused = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}

// #163: MariaDB not accepting connections yet at boot is retried with
// backoff, and each failed attempt is reported once, instead of the first
// refused ping ending the process.
func TestPingWithRetry_RecoversWhenDatabaseComesUp(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	calls := 0
	ping := func(context.Context) error {
		calls++
		if calls < 5 {
			return refused
		}
		return nil
	}
	var reported []int
	r := ConnectRetry{Timeout: time.Minute, InitialBackoff: 250 * time.Millisecond, MaxBackoff: 2 * time.Second,
		OnRetry: func(attempt int, wait time.Duration, err error) { reported = append(reported, attempt) }}
	if err := pingWithRetry(context.Background(), ping, r, clock); err != nil {
		t.Fatalf("ping should succeed once the DB is up, got %v", err)
	}
	if calls != 5 {
		t.Fatalf("calls = %d, want 5", calls)
	}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}
	if len(clock.waits) != len(want) {
		t.Fatalf("waits = %v, want %v", clock.waits, want)
	}
	for i := range want {
		if clock.waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v (exponential, capped)", clock.waits, want)
		}
	}
	if len(reported) != 4 || reported[0] != 1 || reported[3] != 4 {
		t.Fatalf("OnRetry attempts = %v, want [1 2 3 4]", reported)
	}
}

func TestPingWithRetry_GivesUpAfterTimeout(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	calls := 0
	ping := func(context.Context) error { calls++; return refused }
	r := ConnectRetry{Timeout: 10 * time.Second, InitialBackoff: time.Second, MaxBackoff: 4 * time.Second}
	err := pingWithRetry(context.Background(), ping, r, clock)
	if !errors.Is(err, refused) {
		t.Fatalf("want the last ping error after giving up, got %v", err)
	}
	var total time.Duration
	for _, w := range clock.waits {
		total += w
	}
	if total > 10*time.Second {
		t.Fatalf("waited %s, longer than the 10s budget (waits %v)", total, clock.waits)
	}
	if calls < 3 {
		t.Fatalf("calls = %d, want several attempts within the budget", calls)
	}
}

// A server that answered (bad password, unknown database) will not fix
// itself by waiting; fail fast so misconfiguration is visible immediately.
func TestPingWithRetry_ServerErrorsAreNotRetried(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	calls := 0
	denied := &mysql.MySQLError{Number: 1045, Message: "Access denied"}
	ping := func(context.Context) error { calls++; return denied }
	err := pingWithRetry(context.Background(), ping, ConnectRetry{Timeout: time.Minute, InitialBackoff: time.Second, MaxBackoff: time.Second}, clock)
	if !errors.Is(err, denied) || calls != 1 || len(clock.waits) != 0 {
		t.Fatalf("server error: err=%v calls=%d waits=%v, want one attempt and no wait", err, calls, clock.waits)
	}
}

func TestPingWithRetry_ZeroTimeoutIsSingleAttempt(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	calls := 0
	ping := func(context.Context) error { calls++; return refused }
	if err := pingWithRetry(context.Background(), ping, ConnectRetry{}, clock); !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("zero timeout: err=%v calls=%d, want one attempt", err, calls)
	}
}

func TestPingWithRetry_StopsOnContextCancel(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	ping := func(context.Context) error { calls++; cancel(); return refused }
	err := pingWithRetry(ctx, ping, ConnectRetry{Timeout: time.Minute, InitialBackoff: time.Second, MaxBackoff: time.Second}, clock)
	if err == nil || calls != 1 {
		t.Fatalf("canceled: err=%v calls=%d, want stop after one attempt", err, calls)
	}
}

// End to end through the driver: nothing listens on the port, so the pool
// must be retried and then closed when the budget runs out.
func TestNewDBWithRetry_UnreachableServerFailsAfterBudget(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	attempts := 0
	start := time.Now()
	_, err = NewDBWithRetry(context.Background(), "phoenix:phoenix@tcp("+addr+")/phoenix?timeout=200ms", DefaultPool(),
		ConnectRetry{Timeout: 1500 * time.Millisecond, InitialBackoff: 100 * time.Millisecond, MaxBackoff: 400 * time.Millisecond,
			OnRetry: func(int, time.Duration, error) { attempts++ }})
	if err == nil {
		t.Fatal("unreachable server must fail after the budget")
	}
	if attempts < 2 {
		t.Fatalf("OnRetry called %d times, want retries before giving up", attempts)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s, budget was 1.5s", elapsed)
	}
}
