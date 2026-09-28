package repository_test

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestM5HistoryAPI drives the relationship-checked regional history and chart
// routes through the production Echo router on both engines. Every row must
// satisfy the frozen M0 regional heartbeat decoder and every unrelated probe
// must read exactly like a missing one.
func TestM5HistoryAPI(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			activateReplayConfig(t, r)
			events := make([]domain.ProbeReplayEvent, 0, 3)
			for i := 1; i <= 3; i++ {
				ev := r.observation(int64(i))
				at := r.at.Add(time.Duration(i) * time.Second)
				ev.ObservedAt = at
				ev.Observation.ObservedAt = at
				ev.Observation.ReceivedAt = at.Add(2 * time.Second)
				switch i {
				case 2:
					ev.Observation.Status, ev.Observation.RawStatus = domain.StatusDown, domain.StatusDown
					ev.Observation.DownCount = 1
					ev.Observation.Message = "timeout"
					ev.Observation.Important = true
				case 3:
					ev.Observation.Status, ev.Observation.RawStatus = domain.StatusMaintenance, domain.StatusMaintenance
					ev.Observation.Message = "maintenance"
				default:
					ev.Observation.Message = "OK"
				}
				events = append(events, ev)
			}
			r.ingest(t, r.batch(events...))

			var repos repositorySet
			if engine == "sqlite" {
				repos = sqliteRepositorySet(sqlite.NewRepository(r.f.db))
			} else {
				repos = mariadbRepositorySet(mariadb.NewRepository(r.f.db))
			}
			ctx := t.Context()
			viewer := r.f.user(t)
			outsider := r.f.user(t)
			if err := repos.userPermissions.Grant(ctx, &domain.UserPermission{UserID: viewer, MonitorID: &r.monitor}); err != nil {
				t.Fatal(err)
			}
			access := services.NewAccessService(repos.users, repos.userPermissions, repos.monitorGroups, repos.monitors)
			health := services.NewMonitorHealthService(repos.monitors, r.f.assignments, r.f.commits, access)
			health.SetProjections(r.f.projections)
			reader := services.NewMonitorRegionalService(health, r.f.registry)
			reader.SetHistory(r.f.commits, r.f.assignments)
			hbHandlers := handlers.NewHeartbeatHandlers(services.NewHeartbeatService(repos.heartbeats, nil), access)
			hbHandlers.SetOverall(reader)
			jwt := auth.NewJWTAuthenticator(strings.Repeat("m5-history-test-key", 4), 1, repos.users)
			authSvc := services.NewAuthService(repos.users, nil, jwt, nil)
			viewerToken, err := jwt.IssueSession(ctx, viewer)
			if err != nil {
				t.Fatal(err)
			}
			outsiderToken, err := jwt.IssueSession(ctx, outsider)
			if err != nil {
				t.Fatal(err)
			}
			router := func(enabled bool) http.Handler {
				return httppkg.NewRouter(
					handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, hbHandlers,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					authSvc, access, nil, nil, embed.FS{}, httppkg.RouterOptions{RegionalMonitors: handlers.NewMonitorRegionalHandlers(reader, enabled)}, nil, "")
			}
			e := router(true)
			related := r.session.ProbeID
			request := func(handler http.Handler, path, token string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				return rec
			}

			historyPath := fmt.Sprintf("/api/monitors/%d/probes/%s/heartbeats?order=asc", r.monitor, related)
			for _, path := range []string{historyPath, fmt.Sprintf("/api/monitors/%d/probes/%s/heartbeats/chart", r.monitor, related)} {
				if rec := request(e, path, ""); rec.Code != 401 {
					t.Fatalf("missing authentication: %d %s", rec.Code, rec.Body.String())
				}
				if rec := request(e, path, "invalid"); rec.Code != 401 {
					t.Fatalf("invalid authentication: %d", rec.Code)
				}
				if rec := request(e, path, outsiderToken); rec.Code != 404 {
					t.Fatalf("ungranted monitor must read missing: %d %s", rec.Code, rec.Body.String())
				} else if !strings.Contains(rec.Body.String(), "monitor_not_found") {
					t.Fatalf("ungranted monitor leaked a different answer: %s", rec.Body.String())
				}
			}

			rec := request(e, historyPath, viewerToken)
			if rec.Code != 200 {
				t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
			}
			var rows []json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 {
				t.Fatalf("want 3 rows, got %d: %s", len(rows), rec.Body.String())
			}
			statuses := make([]string, 0, 3)
			for i, raw := range rows {
				row, err := probe.DecodeRegionalHeartbeat(raw)
				if err != nil {
					t.Fatalf("row %d violates the frozen decoder: %v\n%s", i, err, raw)
				}
				if row.ProbeID != related || row.MonitorID != r.monitor {
					t.Fatalf("row %d identity: %+v", i, row)
				}
				statuses = append(statuses, row.Status)
			}
			if statuses[0] != "up" || statuses[1] != "down" || statuses[2] != "maintenance" {
				t.Fatalf("lowercase regional status vocabulary broken: %v", statuses)
			}

			// A probe that never belonged to this monitor reads exactly like a
			// missing one, with no evidence read behind it.
			unknownPath := fmt.Sprintf("/api/monitors/%d/probes/%s/heartbeats", r.monitor, probeRegistryID2)
			rec = request(e, unknownPath, viewerToken)
			if rec.Code != 404 || !strings.Contains(rec.Body.String(), "probe_not_found") {
				t.Fatalf("unrelated probe: %d %s", rec.Code, rec.Body.String())
			}

			rec = request(e, fmt.Sprintf("/api/monitors/%d/probes/%s/heartbeats/chart", r.monitor, related), viewerToken)
			if rec.Code != 200 {
				t.Fatalf("chart: %d %s", rec.Code, rec.Body.String())
			}

			// Section 7.2 activation: this monitor has a remote member, so the
			// unqualified endpoints serve the overall stream with the unmeasured
			// zero ping and no synthetic latency buckets.
			overallPath := fmt.Sprintf("/api/monitors/%d/heartbeats", r.monitor)
			rec = request(e, overallPath, viewerToken)
			if rec.Code != 200 {
				t.Fatalf("overall list: %d %s", rec.Code, rec.Body.String())
			}
			var overallRows []map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &overallRows); err != nil {
				t.Fatal(err)
			}
			if len(overallRows) == 0 {
				t.Fatalf("overall timeline must not be empty for a multi-probe monitor: %s", rec.Body.String())
			}
			for i, row := range overallRows {
				if row["scope"] != "overall" || row["latency_available"] != false {
					t.Fatalf("row %d lost its section-7.2 markers: %v", i, row)
				}
				if row["ping"] != float64(0) {
					t.Fatalf("row %d must carry the unmeasured zero sentinel: %v", i, row)
				}
			}
			rec = request(e, overallPath+"/chart", viewerToken)
			if rec.Code != 200 {
				t.Fatalf("overall chart: %d %s", rec.Code, rec.Body.String())
			}
			var overallChart struct {
				Buckets           []json.RawMessage `json:"buckets"`
				DowntimeIntervals []json.RawMessage `json:"downtime_intervals"`
				UnknownIntervals  []json.RawMessage `json:"unknown_intervals"`
				Scope             string            `json:"scope"`
				LatencyAvailable  bool              `json:"latency_available"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &overallChart); err != nil {
				t.Fatal(err)
			}
			if overallChart.Scope != "overall" || overallChart.LatencyAvailable || len(overallChart.Buckets) != 0 {
				t.Fatalf("overall chart must carry no synthetic latency: %s", rec.Body.String())
			}
			if overallChart.DowntimeIntervals == nil || overallChart.UnknownIntervals == nil {
				t.Fatalf("chart arrays must never be null: %s", rec.Body.String())
			}

			// Relationship is historical: unassigning the probe must not erase
			// access to the evidence it produced while assigned.
			set, err := r.f.assignments.GetByMonitorID(ctx, r.monitor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.f.assignments.Replace(ctx, r.monitor, set.Revision, []string{domain.LocalProbeID}, set.HealthPolicy); err != nil {
				t.Fatal(err)
			}
			rec = request(e, historyPath, viewerToken)
			if rec.Code != 200 || len(rec.Body.Bytes()) == 0 {
				t.Fatalf("historical relationship lost: %d %s", rec.Code, rec.Body.String())
			}
			rec = request(e, unknownPath, viewerToken)
			if rec.Code != 404 {
				t.Fatalf("unrelated probe after unassign: %d %s", rec.Code, rec.Body.String())
			}

			if rec := request(router(false), historyPath, viewerToken); rec.Code != 503 || !strings.Contains(rec.Body.String(), "probes_disabled") {
				t.Fatalf("disabled routes: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
