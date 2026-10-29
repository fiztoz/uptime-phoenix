package handlers_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
)

// GitHub #122: a garbage or forged 2FA ticket is a client error (401), not a
// 500 that masks real server faults in monitoring.
func TestVerify2FARejectsBadTicketsAsUnauthorized(t *testing.T) {
	h := newHarness(t)
	otherKey := auth.NewJWTAuthenticator(strings.Repeat("some-other-signing-key-", 2), 1, memory.NewUserRepo())
	forged, err := otherKey.IssuePending2FATicket(context.Background(), 1)
	if err != nil {
		t.Fatalf("mint forged ticket: %v", err)
	}
	for name, ticket := range map[string]string{
		"garbage":           "garbage",
		"wrongly signed":    forged,
		"structurally junk": "a.b.c",
	} {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, http.MethodPost, "/api/auth/verify-2fa", map[string]string{
				"ticket": ticket,
				"token":  "123456",
			}, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
