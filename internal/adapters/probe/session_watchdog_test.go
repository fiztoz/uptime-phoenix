package probe_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
)

type sessionWatchdogAdmission struct {
	admitted chan bool
	at       time.Time
}

func (a *sessionWatchdogAdmission) Admit(_ int64, decode func(time.Time) (*bool, error)) error {
	value, err := decode(a.at)
	if err != nil {
		return err
	}
	if value != nil {
		a.admitted <- *value
	}
	return nil
}

func TestSessionWatchdogAdmitsValidatedHealthBeforeBlockedWorkers(t *testing.T) {
	for _, role := range []string{"hub", "probe"} {
		t.Run(role, func(t *testing.T) {
			client, peer := newLocalWSPair(t)
			session, err := probe.NewSession(client, probe.SessionConfig{Generation: 7, PeerRole: role})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			admission := &sessionWatchdogAdmission{admitted: make(chan bool, 8), at: time.Now()}
			normalEntered, healthEntered := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- session.RunWithHealthAdmission(ctx, func(ctx context.Context, _ probe.Envelope) error {
					close(normalEntered)
					<-ctx.Done()
					return ctx.Err()
				}, func(ctx context.Context, receipt probe.HealthReceipt) error {
					if receipt.Generation != 7 || !receipt.ReceivedAt.Equal(admission.at) {
						t.Error("worker replaced admitted identity/time")
					}
					close(healthEntered)
					<-ctx.Done()
					return ctx.Err()
				}, admission)
			}()
			t.Cleanup(func() { cancel(); <-done })
			write := func(frame []byte) {
				t.Helper()
				if err := peer.Write(ctx, websocket.MessageText, frame); err != nil {
					t.Fatal(err)
				}
			}
			wait := func(ch <-chan struct{}) {
				t.Helper()
				select {
				case <-ch:
				case <-ctx.Done():
					t.Fatal("worker not reached")
				}
			}
			write(makeValidControlFrame(t, 7, "state.begin"))
			wait(normalEntered)
			good := string(makeValidHealthFrame(t, 7, role))
			write([]byte(good))
			wait(healthEntered)
			degraded := strings.Replace(good, `"ready":true`, `"ready":false`, 1)
			frames := []string{
				strings.Replace(good, `"ready":true`, `"ready":false,"READY":true`, 1),
				strings.Replace(degraded, `"db_writable":true`, `"db_writable":false`, 1),
			}
			if role == "probe" {
				frames = append(frames, strings.Replace(degraded, `"scheduler_healthy":true`, `"scheduler_healthy":false`, 1))
			}
			for _, frame := range frames {
				if frame == good {
					t.Fatal("fixture field was not mutated")
				}
				if _, _, err := probe.DecodeHealth([]byte(frame)); err != nil {
					t.Fatal("invalid degraded fixture", err)
				}
				write([]byte(frame))
			}
			write([]byte(good))
			for i := 0; i < len(frames)+2; i++ {
				select {
				case got := <-admission.admitted:
					if want := i == 0 || i == len(frames)+1; got != want {
						t.Fatalf("role health facts/aliases changed: sample %d got %v", i, got)
					}
				case <-ctx.Done():
					t.Fatal("watchdog admission waited behind storage callback")
				}
			}
		})
	}
}

func TestSessionWatchdogRejectsInvalidHealthBeforeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		generation probe.Decimal
	}{{"role", "probe", 7}, {"generation", "hub", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := newLocalWSPair(t)
			session, err := probe.NewSession(client, probe.SessionConfig{Generation: 7, PeerRole: "hub"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			admission := &sessionWatchdogAdmission{admitted: make(chan bool, 1), at: time.Now()}
			done := make(chan error, 1)
			go func() {
				done <- session.RunWithHealthAdmission(ctx, func(context.Context, probe.Envelope) error { return nil }, func(context.Context, probe.HealthReceipt) error { t.Error("invalid health reached worker"); return nil }, admission)
			}()
			_ = peer.Write(ctx, websocket.MessageText, makeValidHealthFrame(t, tc.generation, tc.role))
			select {
			case err := <-done:
				if err == nil || ctx.Err() != nil {
					t.Fatal("invalid frame did not revoke session", err)
				}
			case <-ctx.Done():
				t.Fatal("invalid frame blocked")
			}
			if len(admission.admitted) != 0 {
				t.Fatal("invalid health renewed watchdog")
			}
		})
	}
}
