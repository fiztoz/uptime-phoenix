package ws

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// closeAuthn is a ports.Authenticator that only implements VerifyToken.
// Every other method is unused by HandleWebSocket.
type closeAuthn struct {
	userID int64
	err    error
}

func (a closeAuthn) Login(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}
func (a closeAuthn) VerifyToken(context.Context, string) (int64, error) {
	return a.userID, a.err
}
func (a closeAuthn) HashPassword(string) (string, error) { return "", errors.New("unused") }
func (a closeAuthn) VerifyPassword(string, string) error { return errors.New("unused") }
func (a closeAuthn) IssueSession(context.Context, int64) (string, error) {
	return "", errors.New("unused")
}
func (a closeAuthn) IssuePending2FATicket(context.Context, int64) (string, error) {
	return "", errors.New("unused")
}
func (a closeAuthn) VerifyPending2FATicket(context.Context, string) (int64, error) {
	return 0, errors.New("unused")
}

func newAuthCloseHub(t *testing.T) *Hub {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHub(eventbus.NewMemoryBus(), nil, nil, nil, nil, log)
}

func serveHandleWebSocket(t *testing.T, h *Hub, authSvc *services.AuthService, jwt string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		h.HandleWebSocket(WithJWT(r.Context(), jwt), conn, authSvc)
	}))
}

func dialWS(t *testing.T, srv *httptest.Server) (*websocket.Conn, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn, cancel
}

func TestHandleWebSocket_InvalidJWTClosesUnauthorized(t *testing.T) {
	h := newAuthCloseHub(t)
	authSvc := services.NewAuthService(nil, nil, closeAuthn{err: errors.New("token expired")}, nil)
	srv := serveHandleWebSocket(t, h, authSvc, "dead-token")
	t.Cleanup(srv.Close)

	conn, cancel := dialWS(t, srv)
	defer cancel()

	_, _, err := conn.Read(context.Background())
	if got := websocket.CloseStatus(err); got != StatusUnauthorized {
		t.Fatalf("close status = %d (%v); want %d so the frontend logs the user out", got, err, StatusUnauthorized)
	}
}

func TestHandleWebSocket_MissingJWTClosesUnauthorized(t *testing.T) {
	h := newAuthCloseHub(t)
	authSvc := services.NewAuthService(nil, nil, closeAuthn{userID: 1}, nil)
	srv := serveHandleWebSocket(t, h, authSvc, "")
	t.Cleanup(srv.Close)

	conn, cancel := dialWS(t, srv)
	defer cancel()

	_, _, err := conn.Read(context.Background())
	if got := websocket.CloseStatus(err); got != StatusUnauthorized {
		t.Fatalf("close status = %d (%v); want %d", got, err, StatusUnauthorized)
	}
}

func TestHandleWebSocket_ValidJWTDoesNotCloseUnauthorized(t *testing.T) {
	h := newAuthCloseHub(t)
	authSvc := services.NewAuthService(nil, nil, closeAuthn{userID: 1}, nil)
	srv := serveHandleWebSocket(t, h, authSvc, "good-token")
	t.Cleanup(srv.Close)

	conn, cancel := dialWS(t, srv)
	defer cancel()

	ctx, readCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer readCancel()
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) == StatusUnauthorized {
		t.Fatalf("valid JWT was closed as unauthorized: %v", err)
	}
}

func TestStatusUnauthorizedIsInFrontendAuthRange(t *testing.T) {
	// The Svelte store treats 4001–4003 as "wipe JWT, go to /login".
	// 1008 is the old code that caused the reconnect loop — this constant
	// must stay inside the range the client already understood.
	if StatusUnauthorized < 4001 || StatusUnauthorized > 4003 {
		t.Fatalf("StatusUnauthorized = %d; want 4001–4003", StatusUnauthorized)
	}
	if StatusUnauthorized == websocket.StatusPolicyViolation {
		t.Fatal("StatusUnauthorized must not be 1008; that is the bug this test guards")
	}
}
