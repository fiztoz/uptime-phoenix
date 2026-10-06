package repository_test

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// m5AwareFleetReadiness is the permissive ports.HubWorkerReadiness double for
// this suite: the T34 mixed-version gate is exercised by its own tests, and these
// assignment-write cases need remote activation to be allowed.
type m5AwareFleetReadiness struct{}

func (m5AwareFleetReadiness) DeclareWorker(context.Context, string, int, time.Duration) error {
	return nil
}

func (m5AwareFleetReadiness) UnawareWorkers(context.Context, int, time.Duration) ([]string, error) {
	return nil, nil
}

func m5AwareFleetGate() services.FleetActivationGate {
	return services.NewFleetActivationGate(m5AwareFleetReadiness{}, time.Minute)
}

// TestM5AssignmentWrites proves the revisioned desired-set write surface end
// to end on both engines: atomic complete-set replacement with optimistic
// revisions, preserved versus cleared bindings, legacy initialization inside
// the write transaction, atomic create-with-assignments, and the clone rule.
func TestM5AssignmentWrites(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			activateReplayConfig(t, r)
			ctx := t.Context()
			var repos repositorySet
			if engine == "sqlite" {
				repos = sqliteRepositorySet(sqlite.NewRepository(r.f.db))
			} else {
				repos = mariadbRepositorySet(mariadb.NewRepository(r.f.db))
			}
			admin := r.f.user(t)
			adminUser, err := repos.users.GetByID(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			adminUser.IsAdmin = true
			if err := repos.users.Update(ctx, adminUser); err != nil {
				t.Fatal(err)
			}
			creator := r.f.user(t)
			creatorUser, err := repos.users.GetByID(ctx, creator)
			if err != nil {
				t.Fatal(err)
			}
			creatorUser.CanCreateMonitors = true
			creatorUser.CanCreateTopLevelMonitors = true
			if err := repos.users.Update(ctx, creatorUser); err != nil {
				t.Fatal(err)
			}
			if err := repos.userPermissions.Grant(ctx, &domain.UserPermission{UserID: creator, MonitorID: &r.monitor}); err != nil {
				t.Fatal(err)
			}
			// The fixture monitor row is created by raw SQL without an owner; a
			// full-row update writes every column, so give it a real user first.
			if _, err := r.f.db.ExecContext(ctx, "UPDATE monitors SET user_id = ? WHERE id = ?", admin, r.monitor); err != nil {
				t.Fatal(err)
			}
			access := services.NewAccessService(repos.users, repos.userPermissions, repos.monitorGroups, repos.monitors)
			health := services.NewMonitorHealthService(repos.monitors, r.f.assignments, r.f.commits, access)
			health.SetProjections(r.f.projections)
			diagnostics := repository.NewProbeDiagnosticsStore(r.f.db)
			reader := services.NewMonitorRegionalService(health, r.f.registry)
			reader.SetDiagnostics(diagnostics)
			caps := checker.CapabilityInspector{}
			writer := repository.NewProbeAssignmentStore(r.f.db)
			assignmentSvc := services.NewProbeAssignmentService(writer, r.f.assignments, r.f.registry, repos.monitors, caps, m5AwareFleetGate())
			monitorSvc := services.NewMonitorService(repos.monitors, eventbus.NewMemoryBus())
			monitorSvc.SetAssignmentProvisioning(writer, r.f.registry, caps, m5AwareFleetGate())
			monitorSvc.SetAssignmentReader(r.f.assignments)
			jwt := auth.NewJWTAuthenticator(strings.Repeat("m5-assignment-test-key-", 4), 1, repos.users)
			authSvc := services.NewAuthService(repos.users, nil, jwt, nil)
			viewerToken, err := jwt.IssueSession(ctx, creator)
			if err != nil {
				t.Fatal(err)
			}
			adminToken, err := jwt.IssueSession(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			regionals := handlers.NewMonitorRegionalHandlers(reader, true)
			regionals.SetAssignments(assignmentSvc)
			e := httppkg.NewRouter(
				handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc),
				handlers.NewMonitorHandlers(monitorSvc, access, nil, nil), nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				authSvc, access, nil, nil, embed.FS{}, httppkg.RouterOptions{RegionalMonitors: regionals}, nil, "")

			type authed struct{ bearer, method, path, body string }
			request := func(a authed) *httptest.ResponseRecorder {
				var payload *strings.Reader
				if a.body != "" {
					payload = strings.NewReader(a.body)
				} else {
					payload = strings.NewReader("")
				}
				req := httptest.NewRequest(a.method, a.path, payload)
				if a.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				if a.bearer != "" {
					req.Header.Set("Authorization", "Bearer "+a.bearer)
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			put := func(body string, token string) *httptest.ResponseRecorder {
				return request(authed{bearer: token, method: http.MethodPut, path: fmt.Sprintf("/api/monitors/%d/probes", r.monitor), body: body})
			}
			setView := func(rec *httptest.ResponseRecorder) handlers.MonitorProbeAssignmentsView {
				t.Helper()
				var view handlers.MonitorProbeAssignmentsView
				if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
					t.Fatal(err)
				}
				return view
			}
			getSet := func(monitorID int64, token string) handlers.MonitorProbeAssignmentsView {
				t.Helper()
				rec := request(authed{bearer: token, method: http.MethodGet, path: fmt.Sprintf("/api/monitors/%d/probes", monitorID)})
				if rec.Code != 200 {
					t.Fatalf("read set: %d %s", rec.Code, rec.Body.String())
				}
				return setView(rec)
			}

			// Authorization precedes any write.
			if rec := put(`{"expected_revision":"1","probe_ids":["local"],"health_policy":"any_down"}`, ""); rec.Code != 401 {
				t.Fatalf("missing authentication: %d %s", rec.Code, rec.Body.String())
			}
			if rec := put(`{"expected_revision":"1","probe_ids":["local"],"health_policy":"any_down"}`, viewerToken); rec.Code != 403 {
				t.Fatalf("non-admin write: %d %s", rec.Code, rec.Body.String())
			}

			// The fixture set is at revision two (local init + remote replace).
			current := getSet(r.monitor, adminToken)
			if current.Revision != 2 || current.Assignments[0].ProbeID != r.session.ProbeID {
				t.Fatalf("fixture set: %+v", current)
			}
			// A changed write reports its unproven members pending even though
			// an applied receipt proves an earlier document.
			rec := put(`{"expected_revision":"2","probe_ids":["local"],"health_policy":"any_down","alert_delivery":"regional"}`, adminToken)
			view := setView(rec)
			if rec.Code != 200 || view.Revision != 3 || len(view.Assignments) != 1 || view.Assignments[0].ProbeID != domain.LocalProbeID {
				t.Fatalf("replacement: %d %s", rec.Code, rec.Body.String())
			}
			// Stale precondition: nothing changes and the client gets 409.
			rec = put(`{"expected_revision":"2","probe_ids":["local"],"health_policy":"all_down"}`, adminToken)
			if rec.Code != 409 || !strings.Contains(rec.Body.String(), "stale_revision") {
				t.Fatalf("stale revision: %d %s", rec.Code, rec.Body.String())
			}
			if again := getSet(r.monitor, adminToken); again.Revision != 3 {
				t.Fatalf("stale write mutated state: %+v", again)
			}
			// Revision zero is never a valid precondition.
			if rec = put(`{"expected_revision":"0","probe_ids":["local"],"health_policy":"any_down"}`, adminToken); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_expected_revision") {
				t.Fatalf("revision zero: %d %s", rec.Code, rec.Body.String())
			}

			// Identity and configuration rejections.
			r.f.remote(t, probeRegistryID3, "fleet-member")
			for _, tc := range []struct {
				name, body, code string
				status           int
			}{
				{"unknown probe", `{"expected_revision":"3","probe_ids":["99999999-9999-4999-8999-999999999999"],"health_policy":"any_down"}`, "unknown_probe", 409},
				{"disabled probe", `{"expected_revision":"3","probe_ids":["` + probeRegistryID3 + `"],"health_policy":"any_down"}`, "probe_unavailable", 409},
				{"push remote", `{"expected_revision":"3","probe_ids":["` + probeRegistryID1 + `"],"health_policy":"any_down"}`, "unsupported_assignment", 422},
				{"duplicate members", `{"expected_revision":"3","probe_ids":["local","local"],"health_policy":"any_down"}`, "invalid_probe_ids", 400},
				{"empty members", `{"expected_revision":"3","probe_ids":[],"health_policy":"any_down"}`, "invalid_probe_ids", 400},
				{"bad policy", `{"expected_revision":"3","probe_ids":["local"],"health_policy":"quorum"}`, "invalid_health_policy", 400},
				{"bad delivery", `{"expected_revision":"3","probe_ids":["local"],"health_policy":"any_down","alert_delivery":"both"}`, "invalid_alert_delivery", 400},
				{"bad binding", `{"expected_revision":"3","probe_ids":["local"],"health_policy":"any_down","bindings":[{"probe_id":"local","kind":"docker_socket","binding_key":"docker-main"}]}`, "invalid_bindings", 400},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// "push remote" runs before the disabled probe is armed.
					if tc.name == "disabled probe" {
						if _, err := r.f.db.ExecContext(ctx, "UPDATE probes SET enabled = 0 WHERE id = ?", probeRegistryID3); err != nil {
							t.Fatal(err)
						}
					}
					if tc.name == "push remote" {
						if _, err := r.f.db.ExecContext(ctx, "UPDATE monitors SET type = 'push' WHERE id = ?", r.monitor); err != nil {
							t.Fatal(err)
						}
						defer func() {
							if _, err := r.f.db.ExecContext(ctx, "UPDATE monitors SET type = 'http' WHERE id = ?", r.monitor); err != nil {
								t.Fatal(err)
							}
						}()
					}
					rec := put(tc.body, adminToken)
					if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
						t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
					}
				})
			}

			// Resource bindings on a docker monitor: explicit replaces, omitted
			// preserves, explicit empty clears (and then no remote member may
			// remain unbound).
			docker := r.f.monitor(t)
			if _, err := r.f.db.ExecContext(ctx, "UPDATE monitors SET type = 'docker' WHERE id = ?", docker); err != nil {
				t.Fatal(err)
			}
			bound := fmt.Sprintf(`{"expected_revision":"1","probe_ids":["%s"],"health_policy":"any_down","bindings":[{"probe_id":"%s","kind":"docker_socket","binding_key":"docker-main"}]}`, probeRegistryID1, probeRegistryID1)
			if rec = put(`{"expected_revision":"1","probe_ids":["`+probeRegistryID1+`"],"health_policy":"any_down"}`, adminToken); rec.Code == 200 {
				t.Fatalf("unbound remote docker assignment accepted: %d %s", rec.Code, rec.Body.String())
			}
			rec = request(authed{bearer: adminToken, method: http.MethodPut, path: fmt.Sprintf("/api/monitors/%d/probes", docker), body: bound})
			view = setView(rec)
			if rec.Code != 200 || view.Assignments[0].Bindings[0].BindingKey != "docker-main" {
				t.Fatalf("bound docker assignment: %d %s", rec.Code, rec.Body.String())
			}
			rec = request(authed{bearer: adminToken, method: http.MethodPut, path: fmt.Sprintf("/api/monitors/%d/probes", docker),
				body: fmt.Sprintf(`{"expected_revision":"2","probe_ids":["%s"],"health_policy":"any_down","bindings":[{"probe_id":"%s","kind":"docker_api","binding_key":"docker-api"}]}`, probeRegistryID1, probeRegistryID1)})
			view = setView(rec)
			if rec.Code != 200 || view.Assignments[0].Bindings[0].Kind != "docker_api" || view.Assignments[0].SyncStatus == nil || *view.Assignments[0].SyncStatus != "pending" {
				t.Fatalf("explicit binding replacement: %d %s", rec.Code, rec.Body.String())
			}
			rec = request(authed{bearer: adminToken, method: http.MethodPut, path: fmt.Sprintf("/api/monitors/%d/probes", docker),
				body: fmt.Sprintf(`{"expected_revision":"3","probe_ids":["%s"],"health_policy":"any_down"}`, probeRegistryID1)})
			view = setView(rec)
			if rec.Code != 200 || view.Assignments[0].Bindings[0].Kind != "docker_api" {
				t.Fatalf("omitted bindings must preserve: %d %s", rec.Code, rec.Body.String())
			}
			rec = request(authed{bearer: adminToken, method: http.MethodPut, path: fmt.Sprintf("/api/monitors/%d/probes", docker),
				body: `{"expected_revision":"3","probe_ids":["local"],"health_policy":"any_down","bindings":[]}`})
			view = setView(rec)
			if rec.Code != 200 || len(view.Assignments) != 1 || view.Assignments[0].ProbeID != domain.LocalProbeID || len(view.Assignments[0].Bindings) != 0 {
				t.Fatalf("explicit empty bindings must clear: %d %s", rec.Code, rec.Body.String())
			}

			// A legacy monitor without an assignment set is initialized inside
			// the write transaction; revision zero stays invalid.
			legacy := r.f.monitor(t)
			if _, err := r.f.db.ExecContext(ctx, "DELETE FROM monitor_probe_assignments WHERE monitor_id = ?", legacy); err != nil {
				t.Fatal(err)
			}
			if _, err := r.f.db.ExecContext(ctx, "DELETE FROM monitor_probe_assignment_sets WHERE monitor_id = ?", legacy); err != nil {
				t.Fatal(err)
			}
			rec = request(authed{bearer: adminToken, method: http.MethodPut, path: fmt.Sprintf("/api/monitors/%d/probes", legacy),
				body: `{"expected_revision":"1","probe_ids":["local"],"health_policy":"any_down"}`})
			view = setView(rec)
			if rec.Code != 200 || view.Revision != 1 || len(view.Assignments) != 1 {
				t.Fatalf("legacy initialization: %d %s", rec.Code, rec.Body.String())
			}
			if rec = request(authed{bearer: adminToken, method: http.MethodPut, path: fmt.Sprintf("/api/monitors/%d/probes", legacy),
				body: `{"expected_revision":"0","probe_ids":["local"],"health_policy":"any_down"}`}); rec.Code != 400 {
				t.Fatalf("legacy revision zero accepted: %d %s", rec.Code, rec.Body.String())
			}

			// Create with an explicit set commits monitor and set atomically.
			created := request(authed{bearer: adminToken, method: http.MethodPost, path: "/api/monitors",
				body: `{"name":"Regional create","type":"http","config":{"url":"https://example.com"},"probe_ids":["` + probeRegistryID1 + `"],"health_policy":"all_down"}`})
			if created.Code != 201 {
				t.Fatalf("admin create with set: %d %s", created.Code, created.Body.String())
			}
			var createdView struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &createdView); err != nil {
				t.Fatal(err)
			}
			remoteID := createdView.ID
			set, err := r.f.assignments.GetByMonitorID(ctx, remoteID)
			if err != nil || set.Revision != 1 || set.HealthPolicy != domain.HealthPolicyAllDown || len(set.Assignments) != 1 || set.Assignments[0].ProbeID != probeRegistryID1 {
				t.Fatalf("atomic create set: %+v %v", set, err)
			}
			// A non-admin creator keeps local behavior and is rejected when
			// explicitly requesting remote membership — with no monitor row.
			before := monitorCount(t, r)
			forbidden := request(authed{bearer: viewerToken, method: http.MethodPost, path: "/api/monitors",
				body: `{"name":"Forbidden remote","type":"http","config":{"url":"https://example.com"},"probe_ids":["` + probeRegistryID1 + `"]}`})
			if forbidden.Code != 403 {
				t.Fatalf("non-admin remote create: %d %s", forbidden.Code, forbidden.Body.String())
			}
			if after := monitorCount(t, r); after != before {
				t.Fatalf("forbidden create left a monitor behind: %d -> %d", before, after)
			}
			local := request(authed{bearer: viewerToken, method: http.MethodPost, path: "/api/monitors",
				body: `{"name":"Local create","type":"http","config":{"url":"https://example.com"},"probe_ids":["local"]}`})
			if local.Code != 201 {
				t.Fatalf("non-admin local create: %d %s", local.Code, local.Body.String())
			}
			if err := json.Unmarshal(local.Body.Bytes(), &createdView); err != nil {
				t.Fatal(err)
			}
			localID := createdView.ID
			set, err = r.f.assignments.GetByMonitorID(ctx, localID)
			if err != nil || len(set.Assignments) != 1 || set.Assignments[0].ProbeID != domain.LocalProbeID {
				t.Fatalf("local create set: %+v %v", set, err)
			}

			// Clone of a remote monitor: rejected for a non-admin, reproduced
			// atomically for an admin.
			if _, err := r.f.db.ExecContext(ctx, "UPDATE monitors SET user_id = ? WHERE id = ?", creator, remoteID); err != nil {
				t.Fatal(err)
			}
			if err := repos.userPermissions.Grant(ctx, &domain.UserPermission{UserID: creator, MonitorID: &remoteID}); err != nil {
				t.Fatal(err)
			}
			cloneReq := func(token string) *httptest.ResponseRecorder {
				return request(authed{bearer: token, method: http.MethodPost, path: fmt.Sprintf("/api/monitors/%d/clone", remoteID)})
			}
			if rec = cloneReq(viewerToken); rec.Code != 403 {
				t.Fatalf("non-admin remote clone: %d %s", rec.Code, rec.Body.String())
			}
			// Clone preserves today's ownership rule: only the creator (here the
			// admin re-owning the row) may clone at all.
			if _, err := r.f.db.ExecContext(ctx, "UPDATE monitors SET user_id = ? WHERE id = ?", admin, remoteID); err != nil {
				t.Fatal(err)
			}
			rec = cloneReq(adminToken)
			if rec.Code != 201 {
				t.Fatalf("admin remote clone: %d %s", rec.Code, rec.Body.String())
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &createdView); err != nil {
				t.Fatal(err)
			}
			set, err = r.f.assignments.GetByMonitorID(ctx, createdView.ID)
			if err != nil || len(set.Assignments) != 1 || set.Assignments[0].ProbeID != probeRegistryID1 || set.HealthPolicy != domain.HealthPolicyAllDown {
				t.Fatalf("clone set: %+v %v", set, err)
			}

			// Monitor updates without assignment fields preserve the set.
			beforeSet := getSet(r.monitor, adminToken)
			upd := request(authed{bearer: adminToken, method: http.MethodPut, path: fmt.Sprintf("/api/monitors/%d", r.monitor),
				body: `{"name":"Renamed monitor"}`})
			if upd.Code != 200 {
				t.Fatalf("update: %d %s", upd.Code, upd.Body.String())
			}
			if afterSet := getSet(r.monitor, adminToken); afterSet.Revision != beforeSet.Revision || len(afterSet.Assignments) != len(beforeSet.Assignments) {
				t.Fatalf("update disturbed assignments: %+v -> %+v", beforeSet, afterSet)
			}
		})
	}
}

func monitorCount(t *testing.T, r replayFixture) int {
	t.Helper()
	count, err := r.f.db.NewSelect().Table("monitors").Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return int(count)
}
