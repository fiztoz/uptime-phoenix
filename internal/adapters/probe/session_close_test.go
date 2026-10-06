package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newCloseTestPair establishes a loopback websocket pair through an httptest
// server, matching the practice of the other internal runtime tests.
func newCloseTestPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	serverConn := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	var peer *websocket.Conn
	select {
	case peer = <-serverConn:
	case <-ctx.Done():
		t.Fatal("timeout waiting for server connection")
	}
	t.Cleanup(func() {
		_ = client.CloseNow()
		_ = peer.CloseNow()
	})
	return client, peer
}

// closeTestFrame encodes a valid generation-1 session frame with the
// canonical encoder.
func closeTestFrame(t *testing.T, kind string) []byte {
	t.Helper()
	var payload any = map[string]any{"dummy": "value"}
	if kind == "health" {
		payload = Health{
			Role:           "hub",
			Ready:          true,
			DBWritable:     true,
			ConfigRevision: 1,
			ClockTime:      Timestamp(time.Now().UTC()),
			Errors:         []string{},
		}
	}
	data, err := encodeFrame(kind, Decimal(1), payload)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestSessionInternalCloseIsAlwaysFailure covers the reported race: a callback
// closes the session and returns no error while the caller's context is still
// live. Every repetition must report ErrSessionClosed, never success.
func TestSessionInternalCloseIsAlwaysFailure(t *testing.T) {
	t.Run("health callback", func(t *testing.T) {
		for i := 0; i < 25; i++ {
			client, peer := newCloseTestPair(t)
			session, err := NewSession(client, SessionConfig{Generation: 1, PeerRole: "hub"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- session.RunWithHealth(ctx, func(context.Context, Envelope) error { return nil }, func(context.Context, HealthReceipt) error {
					_ = session.Close() // Like an internal config-transfer deadline.
					return nil
				})
			}()
			if err := peer.Write(ctx, websocket.MessageText, closeTestFrame(t, "health")); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrSessionClosed) || ctx.Err() != nil {
					t.Fatalf("internal closure must report %v: %v", ErrSessionClosed, err)
				}
			case <-ctx.Done():
				t.Fatal("internal closure did not join")
			}
			cancel()
		}
	})

	t.Run("frame handler", func(t *testing.T) {
		for i := 0; i < 25; i++ {
			client, peer := newCloseTestPair(t)
			session, err := NewSession(client, SessionConfig{Generation: 1, PeerRole: "hub"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- session.Run(ctx, func(context.Context, Envelope) error {
					_ = session.Close()
					return nil
				})
			}()
			if err := peer.Write(ctx, websocket.MessageText, closeTestFrame(t, "state.begin")); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrSessionClosed) || ctx.Err() != nil {
					t.Fatalf("internal closure must report %v: %v", ErrSessionClosed, err)
				}
			case <-ctx.Done():
				t.Fatal("internal closure did not join")
			}
			cancel()
		}
	})
}

// TestSessionCallerCancellationIsContextError keeps caller-requested
// cancellation distinguishable: the result must be the caller's own context
// error in every interleaving, never an internal-close shape.
func TestSessionCallerCancellationIsContextError(t *testing.T) {
	for i := 0; i < 50; i++ {
		client, _ := newCloseTestPair(t)
		session, err := NewSession(client, SessionConfig{Generation: 1, PeerRole: "hub"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- session.Run(ctx, func(context.Context, Envelope) error { return nil })
		}()
		time.Sleep(2 * time.Millisecond) // Let the loops reach their blocking points.
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation must report context.Canceled: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("caller cancellation did not join")
		}
	}
}

// TestSessionHandlerFailureRetainsCause asserts a handler failure keeps its
// exact information instead of collapsing into a generic close or cancellation.
func TestSessionHandlerFailureRetainsCause(t *testing.T) {
	client, peer := newCloseTestPair(t)
	session, err := NewSession(client, SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- session.Run(ctx, func(context.Context, Envelope) error { return errors.New("boom") })
	}()
	if err := peer.Write(ctx, websocket.MessageText, closeTestFrame(t, "state.begin")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "handler failed") || !strings.Contains(err.Error(), "boom") || ctx.Err() != nil {
			t.Fatalf("handler failure lost its cause: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("handler failure did not end the session")
	}
}

// TestSessionReadDeadlineIsFailure asserts read-deadline closure retains
// non-nil deadline failure information while the caller's context is live.
func TestSessionReadDeadlineIsFailure(t *testing.T) {
	client, _ := newCloseTestPair(t)
	session, err := NewSession(client, SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	session.readTimeout = 25 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runErr := session.Run(ctx, func(context.Context, Envelope) error { return nil })
	if !errors.Is(runErr, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("read deadline must retain failure information: %v", runErr)
	}
}

// TestSessionHandlerDeadlineIsFailure asserts handler-deadline closure retains
// non-nil deadline failure information while the caller's context is live.
func TestSessionHandlerDeadlineIsFailure(t *testing.T) {
	client, peer := newCloseTestPair(t)
	session, err := NewSession(client, SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	session.handlerTimeout = 25 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- session.Run(ctx, func(handleCtx context.Context, _ Envelope) error {
			<-handleCtx.Done()
			return handleCtx.Err()
		})
	}()
	if err := peer.Write(ctx, websocket.MessageText, closeTestFrame(t, "state.begin")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "handler failed") || ctx.Err() != nil {
			t.Fatalf("handler deadline must retain failure information: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("handler deadline did not end the session")
	}
}

// TestSessionConcurrentCloseIsFailure asserts concurrent internal Close calls
// end a live session with the internal-close failure and stay idempotent.
func TestSessionConcurrentCloseIsFailure(t *testing.T) {
	for i := 0; i < 25; i++ {
		client, peer := newCloseTestPair(t)
		session, err := NewSession(client, SessionConfig{Generation: 1, PeerRole: "hub"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- session.Run(ctx, func(context.Context, Envelope) error {
				select {
				case <-started:
				default:
					close(started)
				}
				return nil
			})
		}()
		if err := peer.Write(ctx, websocket.MessageText, closeTestFrame(t, "state.begin")); err != nil {
			t.Fatal(err)
		}
		<-started // Run is past its close check before the concurrent closes race it.
		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = session.Close()
			}()
		}
		wg.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, ErrSessionClosed) || ctx.Err() != nil {
				t.Fatalf("concurrent internal close must report %v: %v", ErrSessionClosed, err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent internal close did not join")
		}
		cancel()
	}
}
