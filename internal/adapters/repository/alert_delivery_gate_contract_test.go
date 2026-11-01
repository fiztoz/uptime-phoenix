package repository_test

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestAlertDeliveryGateContract proves #119 end to end on both engines,
// through the production router, handlers, services, and real stores. With
// PROBES_AGGREGATE_PAGING_ENABLED off, every write path refuses a new request
// for aggregate or both with 400 aggregate_paging_disabled and persists
// nothing: assignment replace, monitor create, monitor clone (a clone is a new
// request for its source's mode), and config-as-code plan and apply. A legacy
// stored aggregate set stays editable when the mode is omitted or echoed, and
// can always move to regional. With the flag on, every path accepts the modes.
// The phases share one store per engine and run in order; the helpers fail
// the engine subtest, so each phase is a block rather than its own subtest.
func TestAlertDeliveryGateContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := t.Context()
			var repos repositorySet
			var cfgRepos struct {
				keys          ports.ConfigKeyRepository
				tags          ports.TagRepository
				proxies       ports.ProxyRepository
				groupNotifs   ports.GroupNotificationRepository
				monitorTags   ports.MonitorTagRepository
				maintMonitors ports.MaintenanceWindowMonitorRepository
			}
			if engine == "sqlite" {
				rep := sqlite.NewRepository(f.db)
				repos = sqliteRepositorySet(rep)
				cfgRepos.keys, cfgRepos.tags, cfgRepos.proxies = rep.ConfigKeyRepo, rep.TagRepo, rep.ProxyRepo
				cfgRepos.groupNotifs, cfgRepos.monitorTags, cfgRepos.maintMonitors = rep.GroupNotificationRepo, rep.MonitorTagRepo, rep.MaintenanceWindowMonitorRepo
			} else {
				rep := mariadb.NewRepository(f.db)
				repos = mariadbRepositorySet(rep)
				cfgRepos.keys, cfgRepos.tags, cfgRepos.proxies = rep.ConfigKeyRepo, rep.TagRepo, rep.ProxyRepo
				cfgRepos.groupNotifs, cfgRepos.monitorTags, cfgRepos.maintMonitors = rep.GroupNotificationRepo, rep.MonitorTagRepo, rep.MaintenanceWindowMonitorRepo
			}
			admin := f.user(t)
			adminUser, err := repos.users.GetByID(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			adminUser.IsAdmin = true
			if err := repos.users.Update(ctx, adminUser); err != nil {
				t.Fatal(err)
			}
			jwt := auth.NewJWTAuthenticator(strings.Repeat("alert-delivery-gate-key-", 4), 1, repos.users)
			token, err := jwt.IssueSession(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}

			// hub wires one hub process over the shared store, the way
			// bootstrap.Run does, with the given aggregate paging flag.
			hub := func(enabled bool) *echo.Echo {
				bus := eventbus.NewMemoryBus()
				t.Cleanup(func() { bus.Close() })
				access := services.NewAccessService(repos.users, repos.userPermissions, repos.monitorGroups, repos.monitors)
				health := services.NewMonitorHealthService(repos.monitors, f.assignments, f.commits, access)
				health.SetProjections(f.projections)
				reader := services.NewMonitorRegionalService(health, f.registry)
				reader.SetDiagnostics(repository.NewProbeDiagnosticsStore(f.db))
				caps := checker.CapabilityInspector{}
				writer := repository.NewProbeAssignmentStore(f.db)
				assignmentSvc := services.NewProbeAssignmentService(writer, f.assignments, f.registry, repos.monitors, caps, m5AwareFleetGate())
				monitorSvc := services.NewMonitorService(repos.monitors, bus)
				monitorSvc.SetAssignmentProvisioning(writer, f.registry, caps, m5AwareFleetGate())
				monitorSvc.SetAssignmentReader(f.assignments)
				configSvc := services.NewConfigService(cfgRepos.keys, cfgRepos.tags, cfgRepos.proxies, repos.notifications, repos.monitorGroups,
					repos.monitors, cfgRepos.monitorTags, repos.monitorNotifications, cfgRepos.groupNotifs, repos.statusPages,
					repos.statusPageMonitors, repos.maintenance, cfgRepos.maintMonitors, auth.NewPasswordHasher())
				configSvc.SetProbeRegistry(f.registry)
				configSvc.SetProbeAssignments(f.assignments)
				configSvc.SetFleetActivationGate(m5AwareFleetGate())
				assignmentSvc.SetAggregatePagingEnabled(enabled)
				monitorSvc.SetAggregatePagingEnabled(enabled)
				configSvc.SetAggregatePagingEnabled(enabled)
				authSvc := services.NewAuthService(repos.users, nil, jwt, nil)
				regionals := handlers.NewMonitorRegionalHandlers(reader, true)
				regionals.SetAssignments(assignmentSvc)
				return httppkg.NewRouter(
					handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc),
					handlers.NewMonitorHandlers(monitorSvc, access, nil, nil), nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					handlers.NewConfigHandlers(configSvc), nil, nil, nil, nil,
					authSvc, access, nil, nil, embed.FS{}, httppkg.RouterOptions{RegionalMonitors: regionals}, nil, "")
			}
			off, on := hub(false), hub(true)

			do := func(e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				if body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			count := func(table string) int {
				t.Helper()
				n, err := f.db.NewSelect().Table(table).Count(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return int(n)
			}
			type snapshot struct{ monitors, sets, keys int }
			snap := func() snapshot {
				return snapshot{count("monitors"), count("monitor_probe_assignment_sets"), count("config_keys")}
			}
			refused := func(rec *httptest.ResponseRecorder, before snapshot, what string) {
				t.Helper()
				body := rec.Body.String()
				if rec.Code != http.StatusBadRequest || !(strings.Contains(body, `"code":"aggregate_paging_disabled"`) || strings.Contains(body, "aggregate paging is disabled")) {
					t.Fatalf("%s: want 400 aggregate paging refusal, got %d %s", what, rec.Code, rec.Body.String())
				}
				if after := snap(); after != before {
					t.Fatalf("%s: refused request persisted state: %+v -> %+v", what, before, after)
				}
			}
			createBody := func(name, delivery string) string {
				mode := ""
				if delivery != "" {
					mode = fmt.Sprintf(`,"alert_delivery":%q`, delivery)
				}
				return fmt.Sprintf(`{"name":%q,"type":"http","config":{"url":"https://example.com"},"probe_ids":["local"],"health_policy":"any_down"%s}`, name, mode)
			}
			created := func(rec *httptest.ResponseRecorder, what string) int64 {
				t.Helper()
				if rec.Code != http.StatusCreated {
					t.Fatalf("%s: %d %s", what, rec.Code, rec.Body.String())
				}
				var view struct {
					ID int64 `json:"id"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.ID == 0 {
					t.Fatalf("%s: decode id: %v %s", what, err, rec.Body.String())
				}
				return view.ID
			}
			set := func(id int64) *domain.MonitorProbeAssignments {
				t.Helper()
				s, err := f.assignments.GetByMonitorID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			replace := func(e *echo.Echo, id int64, delivery string) *httptest.ResponseRecorder {
				mode := ""
				if delivery != "" {
					mode = fmt.Sprintf(`,"alert_delivery":%q`, delivery)
				}
				body := fmt.Sprintf(`{"expected_revision":"%d","probe_ids":["local"],"health_policy":"all_down"%s}`, set(id).Revision, mode)
				return do(e, http.MethodPut, fmt.Sprintf("/api/monitors/%d/probes", id), body)
			}
			configBody := func(key, delivery string) string {
				return fmt.Sprintf(`{"document":{"apiVersion":%q,"kind":%q,"spec":{"monitors":[{"key":%q,"name":%q,"type":"http","interval":60,"config":{"url":"https://example.com"},"probe_assignments":[{"probe":"local"}],"alert_delivery":%q}]}}}`,
					services.ConfigAPIVersion, services.ConfigKind, key, "Config "+key, delivery)
			}

			var regionalID, legacyID int64
			{ // CreateCannotRequestAggregate
				// POST /api/monitors has no alert_delivery field: an explicit set
				// is always created regional, so a sent mode cannot reach the
				// store on either hub. Aggregate is requested only through the
				// paths gated below (replace, clone, config).
				for _, e := range []*echo.Echo{off, on} {
					id := created(do(e, http.MethodPost, "/api/monitors", createBody("sent aggregate", "aggregate")), "create with a mode")
					if got := set(id).AlertDelivery; got != domain.AlertDeliveryRegional && got != "" {
						t.Fatalf("create stored %q from an unsupported request field", got)
					}
				}
				regionalID = created(do(off, http.MethodPost, "/api/monitors", createBody("regional", "")), "regional create")
			}
			{ // ReplaceRefused
				for _, mode := range []string{"aggregate", "both"} {
					revision := set(regionalID).Revision
					refused(replace(off, regionalID, mode), snap(), "replace to "+mode)
					if after := set(regionalID); after.Revision != revision || after.HealthPolicy != domain.HealthPolicyAnyDown {
						t.Fatalf("refused replace changed the set: %+v", after)
					}
				}
			}
			{ // LegacyEditable
				legacyID = created(do(off, http.MethodPost, "/api/monitors", createBody("legacy", "regional")), "legacy create")
				if _, err := f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET alert_delivery = ? WHERE monitor_id = ?", domain.AlertDeliveryAggregate, legacyID); err != nil {
					t.Fatal(err)
				}
				for _, mode := range []string{"", "aggregate"} {
					if rec := replace(off, legacyID, mode); rec.Code != http.StatusOK {
						t.Fatalf("legacy edit with mode %q: %d %s", mode, rec.Code, rec.Body.String())
					}
					if got := set(legacyID).AlertDelivery; got != domain.AlertDeliveryAggregate {
						t.Fatalf("legacy edit with mode %q rewrote delivery to %q", mode, got)
					}
				}
				revision := set(legacyID).Revision
				refused(replace(off, legacyID, "both"), snap(), "legacy aggregate to both")
				if set(legacyID).Revision != revision {
					t.Fatal("refused legacy replace bumped the revision")
				}
			}
			{ // CloneRefused
				before := snap()
				refused(do(off, http.MethodPost, fmt.Sprintf("/api/monitors/%d/clone", legacyID), ""), before, "clone of a legacy aggregate source")
				// The enabled hub clones the same source with its mode intact.
				clone := created(do(on, http.MethodPost, fmt.Sprintf("/api/monitors/%d/clone", legacyID), ""), "clone with paging enabled")
				if got := set(clone).AlertDelivery; got != domain.AlertDeliveryAggregate {
					t.Fatalf("enabled clone stored %q", got)
				}
				// Moving the source to regional is always allowed, and then the
				// disabled hub clones it.
				if rec := replace(off, legacyID, "regional"); rec.Code != http.StatusOK || set(legacyID).AlertDelivery != domain.AlertDeliveryRegional {
					t.Fatalf("legacy to regional: %d %s", rec.Code, rec.Body.String())
				}
				created(do(off, http.MethodPost, fmt.Sprintf("/api/monitors/%d/clone", legacyID), ""), "clone of a regional source")
			}
			{ // ConfigRefused
				for _, mode := range []string{"aggregate", "both"} {
					before := snap()
					plan := do(off, http.MethodPost, "/api/config/plan", configBody("cfg-"+mode, mode))
					if plan.Code != http.StatusOK || !strings.Contains(plan.Body.String(), `"valid":false`) || !strings.Contains(plan.Body.String(), "aggregate paging is disabled") {
						t.Fatalf("plan %s: %d %s", mode, plan.Code, plan.Body.String())
					}
					refused(do(off, http.MethodPost, "/api/config/apply", configBody("cfg-"+mode, mode)), before, "config apply "+mode)
				}
			}
			{ // EnabledAccepts
				for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
					if rec := replace(on, regionalID, string(mode)); rec.Code != http.StatusOK || set(regionalID).AlertDelivery != mode {
						t.Fatalf("enabled replace to %s: %d %s", mode, rec.Code, rec.Body.String())
					}
				}
				before := count("monitors")
				if rec := do(on, http.MethodPost, "/api/config/apply", configBody("cfg-enabled", "aggregate")); rec.Code != http.StatusOK {
					t.Fatalf("enabled config apply: %d %s", rec.Code, rec.Body.String())
				}
				if count("monitors") != before+1 {
					t.Fatal("enabled config apply did not create its monitor")
				}
			}
		})
	}
}
