package http_test

import (
	"embed"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func healthRouter(t *testing.T, ready bool) http.Handler {
	t.Helper()
	users := memory.NewUserRepo()
	jwt := auth.NewJWTAuthenticator(strings.Repeat("health-alias-test-key-", 2), 1, users)
	authSvc := services.NewAuthService(users, nil, jwt, nil)
	return httppkg.NewRouter(
		handlers.NewHealthHandlers(func() bool { return ready }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		authSvc, nil, nil, nil, embed.FS{}, httppkg.RouterOptions{}, nil, "")
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// GitHub #127: bare /api/health is the first path operators and external
// uptime checkers try; it answers exactly like /api/health/ready.
func TestBareHealthPathIsTheReadinessProbe(t *testing.T) {
	for _, ready := range []bool{true, false} {
		h := healthRouter(t, ready)
		want := get(h, "/api/health/ready")
		got := get(h, "/api/health")
		if got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("ready=%v: GET /api/health = %d %q; want /api/health/ready's %d %q",
				ready, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
	}
}
