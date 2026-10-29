package ws

import (
	"context"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestHandleWebSocket_RevokedSessionClosesUnauthorized(t *testing.T) {
	for _, state := range []string{"deactivated", "deleted"} {
		t.Run(state, func(t *testing.T) {
			users := memory.NewUserRepo()
			user := &domain.User{Username: "session-owner", Active: true}
			if err := users.Create(t.Context(), user); err != nil {
				t.Fatal(err)
			}
			jwt := auth.NewJWTAuthenticator(strings.Repeat("fixture", 8), 1, users)
			svc := services.NewAuthService(users, nil, jwt, nil)
			token, err := jwt.IssueSession(t.Context(), user.ID)
			if err != nil {
				t.Fatal(err)
			}
			if state == "deactivated" {
				user.Active = false
				err = users.Update(t.Context(), user)
			} else {
				err = users.Delete(t.Context(), user.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			server := serveHandleWebSocket(t, newAuthCloseHub(t), svc, token)
			t.Cleanup(server.Close)
			conn, cancel := dialWS(t, server)
			defer cancel()
			_, _, err = conn.Read(context.Background())
			if websocket.CloseStatus(err) != StatusUnauthorized {
				t.Fatalf("revoked session close status=%d, want unauthorized", websocket.CloseStatus(err))
			}
		})
	}
}
