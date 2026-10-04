package repository_test

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestM5ReadAPI(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			activateReplayConfig(t, r)
			r.ingest(t, r.batch(r.observation(1)))
			var repos repositorySet
			if engine == "sqlite" {
				repos = sqliteRepositorySet(sqlite.NewRepository(r.f.db))
			} else {
				repos = mariadbRepositorySet(mariadb.NewRepository(r.f.db))
			}
			ctx := t.Context()
			viewer := r.f.user(t)
			admin := r.f.user(t)
			adminUser, err := repos.users.GetByID(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			adminUser.IsAdmin = true
			if err := repos.users.Update(ctx, adminUser); err != nil {
				t.Fatal(err)
			}
			if err := repos.userPermissions.Grant(ctx, &domain.UserPermission{UserID: viewer, MonitorID: &r.monitor}); err != nil {
				t.Fatal(err)
			}
			access := services.NewAccessService(repos.users, repos.userPermissions, repos.monitorGroups, repos.monitors)
			health := services.NewMonitorHealthService(repos.monitors, r.f.assignments, r.f.commits, access)
			health.SetProjections(r.f.projections)
			reader := services.NewMonitorRegionalService(health, r.f.registry)
			jwt := auth.NewJWTAuthenticator(strings.Repeat("m5-read-test-key-", 4), 1, repos.users)
			authSvc := services.NewAuthService(repos.users, nil, jwt, nil)
			viewerToken, err := jwt.IssueSession(ctx, viewer)
			if err != nil {
				t.Fatal(err)
			}
			adminToken, err := jwt.IssueSession(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			router := func(enabled bool) http.Handler {
				return httppkg.NewRouter(
					handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					authSvc, access, nil, nil, embed.FS{}, httppkg.RouterOptions{RegionalMonitors: handlers.NewMonitorRegionalHandlers(reader, enabled)}, nil, "")
			}
			e := router(true)
			request := func(handler http.Handler, path, token string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				return rec
			}
			for _, suffix := range []string{"probes", "health"} {
				path := fmt.Sprintf("/api/monitors/%d/%s", r.monitor, suffix)
				if rec := request(e, path, ""); rec.Code != 401 {
					t.Fatalf("missing authentication: %d %s", rec.Code, rec.Body.String())
				}
				if rec := request(e, path, "invalid"); rec.Code != 401 {
					t.Fatalf("invalid authentication: %d", rec.Code)
				}
				for _, token := range []string{viewerToken, adminToken} {
					rec := request(e, path, token)
					if rec.Code != 200 {
						t.Fatalf("authorized %s: %d %s", suffix, rec.Code, rec.Body.String())
					}
					if rec.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("scoped response can be cached")
					}
					for _, secret := range []string{"endpoint", "fingerprint", "ProtectedCredential", "edge.example", "protected_payload", "config_document"} {
						if strings.Contains(rec.Body.String(), secret) {
							t.Fatalf("leaked %s", secret)
						}
					}
					if suffix == "health" {
						var view handlers.MonitorHealthView
						if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
							t.Fatal(err)
						}
						if view.Status != "up" || len(view.Regions) != 1 || view.Regions[0].ProbeID != r.session.ProbeID || view.Regions[0].ObservedAt == nil || view.Regions[0].ReceivedAt == nil {
							t.Fatalf("persisted replay not visible: %+v", view)
						}
					} else {
						var view handlers.MonitorProbeAssignmentsView
						if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
							t.Fatal(err)
						}
						if len(view.Assignments) != 1 || view.Assignments[0].ProbeID != r.session.ProbeID || view.Assignments[0].SyncStatus != nil {
							t.Fatalf("incorrect assignment projection: %+v", view)
						}
					}
				}
				disabled := request(router(false), path, viewerToken)
				if disabled.Code != 503 || !strings.Contains(disabled.Body.String(), "probes_disabled") {
					t.Fatalf("disabled: %d %s", disabled.Code, disabled.Body.String())
				}
			}
			// A registered but unassigned probe never appears in scoped reads.
			r.f.remote(t, probeRegistryID3, "unrelated-fleet-member")
			if rec := request(e, fmt.Sprintf("/api/monitors/%d/probes", r.monitor), viewerToken); strings.Contains(rec.Body.String(), "unrelated-fleet-member") {
				t.Fatal("fleet enumeration from scoped route")
			}
			if err := access.RevokeMonitor(ctx, viewer, r.monitor); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"probes", "health"} {
				hidden := request(e, fmt.Sprintf("/api/monitors/%d/%s", r.monitor, suffix), viewerToken)
				missing := request(e, "/api/monitors/9223372036854775807/"+suffix, viewerToken)
				if hidden.Code != 404 || missing.Code != 404 || hidden.Body.String() != missing.Body.String() {
					t.Fatalf("revocation/absence differs: %d %s vs %d %s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
				}
			}
		})
	}
}
