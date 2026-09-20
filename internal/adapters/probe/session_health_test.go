package probe_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
)

func TestSessionHealthProgressesDuringBlockedWork(t *testing.T) {
	client, peer := newLocalWSPair(t)
	session, err := probe.NewSession(client, probe.SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, health := make(chan struct{}), make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- session.RunWithHealth(ctx, func(ctx context.Context, _ probe.Envelope) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}, func(_ context.Context, sample probe.HealthReceipt) error {
			if sample.Generation != 1 || sample.Health.Role != "hub" || sample.ReceivedAt.IsZero() {
				t.Error("lost validated health receipt identity")
			}
			health <- struct{}{}
			return nil
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	if err := peer.Write(ctx, websocket.MessageText, makeValidControlFrame(t, 1, "state.begin")); err != nil {
		t.Fatal(err)
	}
	<-entered
	healthFrame := makeValidHealthFrame(t, 1, "hub")
	go func() { _ = peer.Write(ctx, websocket.MessageText, healthFrame) }()
	select {
	case <-health:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("health receipt waited behind blocked non-health work")
	}
}

func TestSessionHealthRetainsUnhealthySamplesAndReceiptTime(t *testing.T) {
	client, peer := newLocalWSPair(t)
	session, err := probe.NewSession(client, probe.SessionConfig{Generation: 7, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, release, marker := make(chan struct{}), make(chan struct{}), make(chan struct{})
	samples := make(chan probe.HealthReceipt, 3)
	done := make(chan error, 1)
	go func() {
		first := true
		done <- session.RunWithHealth(ctx, func(context.Context, probe.Envelope) error {
			close(marker)
			return nil
		}, func(ctx context.Context, sample probe.HealthReceipt) error {
			if first {
				first = false
				close(entered)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
				}
			}
			samples <- sample
			return nil
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	write := func(data []byte) {
		t.Helper()
		if err := peer.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	good := makeValidHealthFrame(t, 7, "hub")
	write(good)
	<-entered
	var bad map[string]any
	if err := json.Unmarshal(good, &bad); err != nil {
		t.Fatal(err)
	}
	payload := bad["payload"].(map[string]any)
	payload["ready"], payload["db_writable"] = false, false
	data, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	write(data)
	write(good)
	write(makeValidControlFrame(t, 7, "state.begin"))
	select {
	case <-marker: // Proves both queued health frames were already read.
	case <-time.After(time.Second):
		t.Fatal("a blocked health callback also blocked non-health work")
	}
	releasedAt := time.Now()
	close(release)
	var previous time.Time
	for _, want := range []bool{true, false, true} {
		select {
		case got := <-samples:
			if got.Health.Ready != want || got.Generation != 7 || !got.ReceivedAt.Before(releasedAt) || got.ReceivedAt.Before(previous) || !strings.Contains(got.ReceivedAt.String(), "m=") {
				t.Fatalf("health order/receipt clock changed: %+v", got)
			}
			previous = got.ReceivedAt
		case <-time.After(time.Second):
			t.Fatal("health sample disappeared")
		}
	}
}

func TestSessionHealthOverflowClosesAndJoinsWorker(t *testing.T) {
	client, peer := newLocalWSPair(t)
	session, err := probe.NewSession(client, probe.SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	entered, exited := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- session.RunWithHealth(ctx, func(context.Context, probe.Envelope) error { return nil }, func(ctx context.Context, _ probe.HealthReceipt) error {
			close(entered)
			defer close(exited)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	frame := makeValidHealthFrame(t, 1, "hub")
	if err := peer.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	<-entered
	for range 5 { // Four queued samples, then overflow; no coalescing.
		_ = peer.Write(ctx, websocket.MessageText, frame)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "health queue full") || ctx.Err() != nil {
			t.Fatalf("overload did not fail immediately: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("overload did not stop the session")
	}
	select {
	case <-exited:
	default:
		t.Fatal("RunWithHealth returned before its callback stopped")
	}
}

func TestSessionHealthDuplicateRunDoesNotCloseOwner(t *testing.T) {
	client, peer := newLocalWSPair(t)
	session, err := probe.NewSession(client, probe.SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	samples, done := make(chan struct{}, 1), make(chan error, 1)
	frame := func(context.Context, probe.Envelope) error { return nil }
	health := func(ctx context.Context, _ probe.HealthReceipt) error { samples <- struct{}{}; return ctx.Err() }
	go func() { done <- session.RunWithHealth(ctx, frame, health) }()
	t.Cleanup(func() { cancel(); <-done })
	check := func() {
		t.Helper()
		if err := peer.Write(ctx, websocket.MessageText, makeValidHealthFrame(t, 1, "hub")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-samples:
		case <-ctx.Done():
			t.Fatal("original session was closed")
		}
	}
	check()
	if err := session.RunWithHealth(ctx, frame, health); err == nil {
		t.Fatal("duplicate session Run accepted")
	}
	check()
}

func TestSessionHealthRejectsWrongRoleAndGeneration(t *testing.T) {
	for _, tc := range []struct {
		name string
		role string
		gen  probe.Decimal
	}{{"role", "probe", 7}, {"generation", "hub", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := newLocalWSPair(t)
			session, err := probe.NewSession(client, probe.SessionConfig{Generation: 7, PeerRole: "hub"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			called, done := make(chan struct{}, 1), make(chan error, 1)
			go func() {
				done <- session.RunWithHealth(ctx, func(context.Context, probe.Envelope) error { return nil }, func(context.Context, probe.HealthReceipt) error { called <- struct{}{}; return nil })
			}()
			_ = peer.Write(ctx, websocket.MessageText, makeValidHealthFrame(t, tc.gen, tc.role))
			select {
			case err := <-done:
				if err == nil || ctx.Err() != nil {
					t.Fatalf("invalid health did not close session: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("invalid health did not fail")
			}
			if len(called) != 0 {
				t.Fatal("invalid health reached the application callback")
			}
		})
	}
}

func TestSessionHealthUsesValidatedFields(t *testing.T) {
	client, peer := newLocalWSPair(t)
	session, err := probe.NewSession(client, probe.SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	samples, done := make(chan probe.HealthReceipt, 1), make(chan error, 1)
	go func() {
		done <- session.RunWithHealth(ctx, func(context.Context, probe.Envelope) error { return nil }, func(_ context.Context, sample probe.HealthReceipt) error { samples <- sample; return nil })
	}()
	t.Cleanup(func() { cancel(); <-done })
	// Wire validation reads exact field names. encoding/json's case-insensitive
	// struct mapping would let a later unknown alias overwrite the validated value.
	frame := strings.Replace(string(makeValidHealthFrame(t, 1, "hub")), `"ready":true`, `"ready":false,"READY":true`, 1)
	_, validated, err := probe.DecodeHealth([]byte(frame))
	if err != nil || validated.Ready {
		t.Fatalf("expected a valid degraded health frame: %+v %v", validated, err)
	}
	if err := peer.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-samples:
		if got.Health.Ready != validated.Ready {
			t.Fatal("application callback received an unvalidated alias instead of degraded health")
		}
	case <-ctx.Done():
		t.Fatal("health was not delivered")
	}
}

func TestSessionHealthInternalCloseIsFailure(t *testing.T) {
	client, peer := newLocalWSPair(t)
	session, err := probe.NewSession(client, probe.SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- session.RunWithHealth(ctx, func(context.Context, probe.Envelope) error { return nil }, func(context.Context, probe.HealthReceipt) error {
			_ = session.Close() // Like an internal config-transfer deadline.
			return nil
		})
	}()
	_ = peer.Write(ctx, websocket.MessageText, makeValidHealthFrame(t, 1, "hub"))
	select {
	case err := <-done:
		if err == nil || ctx.Err() != nil {
			t.Fatalf("internal closure must not report success: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("internal closure did not join")
	}
}
