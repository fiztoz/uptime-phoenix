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
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// fleetSecrets are values no fleet read may ever disclose. The endpoint string
// is checked separately: it belongs to admin detail only, never to list.
var fleetSecrets = []string{"protected_credential", "key_hash", "protected_payload", "config_document", "private-sync-fixture", "phx_fleet_", "enrollment_id", "stream_id"}

// TestM5FleetAPI proves the administrative fleet read surface end to end on
// both engines: persisted evidence -> diagnostics read port -> service ->
// production Echo router with signed credentials. It checks admin session and
// write-scope API key admission, non-admin denial, pagination, null fields,
// expired owners, the sync proof and secret exclusion.
func TestM5FleetAPI(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			activateReplayConfig(t, r)
			ctx := t.Context()
			var repos repositorySet
			var apiKeys ports.APIKeyRepository
			if engine == "sqlite" {
				repos = sqliteRepositorySet(sqlite.NewRepository(r.f.db))
				apiKeys = sqlite.NewAPIKeyRepo(r.f.db)
			} else {
				repos = mariadbRepositorySet(mariadb.NewRepository(r.f.db))
				apiKeys = mariadb.NewAPIKeyRepo(r.f.db)
			}
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
			diagnostics := repository.NewProbeDiagnosticsStore(r.f.db)
			reader := services.NewMonitorRegionalService(health, r.f.registry)
			reader.SetDiagnostics(diagnostics)
			jwt := auth.NewJWTAuthenticator(strings.Repeat("m5-fleet-test-key-", 4), 1, repos.users)
			authSvc := services.NewAuthService(repos.users, nil, jwt, nil)
			viewerToken, err := jwt.IssueSession(ctx, viewer)
			if err != nil {
				t.Fatal(err)
			}
			adminToken, err := jwt.IssueSession(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			writeKey, readKey := "phx_fleet_write_0000000000000000000000", "phx_fleet_read_00000000000000000000000"
			fleetAPIKey(t, r, engine, admin, `["write"]`, writeKey)
			fleetAPIKey(t, r, engine, admin, `["read"]`, readKey)
			e := httppkg.NewRouter(
				handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				authSvc, access, apiKeys, nil, embed.FS{}, httppkg.RouterOptions{
					RegionalMonitors: handlers.NewMonitorRegionalHandlers(reader, true),
					ProbeFleet:       handlers.NewProbeFleetHandlers(services.NewProbeFleetService(diagnostics), true),
				}, nil, "")

			type authedRequest struct{ bearer, apiKey string }
			request := func(path string, cred authedRequest) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if cred.bearer != "" {
					req.Header.Set("Authorization", "Bearer "+cred.bearer)
				}
				if cred.apiKey != "" {
					req.Header.Set("Authorization", "ApiKey "+cred.apiKey)
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			adminCred, viewerCred := authedRequest{bearer: adminToken}, authedRequest{bearer: viewerToken}

			// Authentication and administrative authority precede any read.
			if rec := request("/api/probes", authedRequest{}); rec.Code != 401 {
				t.Fatalf("missing authentication: %d %s", rec.Code, rec.Body.String())
			}
			if rec := request("/api/probes", authedRequest{bearer: "invalid"}); rec.Code != 401 {
				t.Fatalf("invalid authentication: %d", rec.Code)
			}
			if rec := request("/api/probes", viewerCred); rec.Code != 403 {
				t.Fatalf("non-admin fleet read: %d %s", rec.Code, rec.Body.String())
			}
			if rec := request("/api/probes/"+r.session.ProbeID, viewerCred); rec.Code != 403 {
				t.Fatalf("non-admin detail read: %d %s", rec.Code, rec.Body.String())
			}
			if rec := request("/api/probes", authedRequest{apiKey: readKey}); rec.Code != 401 {
				t.Fatalf("read-scope key admitted: %d %s", rec.Code, rec.Body.String())
			}
			for _, path := range []string{"/api/probes", "/api/probes/" + r.session.ProbeID} {
				if rec := request(path, adminCred); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("admin session: %d %s", rec.Code, rec.Body.String())
				}
				if rec := request(path, authedRequest{apiKey: writeKey}); rec.Code != 200 {
					t.Fatalf("write-scope API key: %d %s", rec.Code, rec.Body.String())
				}
			}

			// The prepared-but-unconnected session with an applied receipt is
			// degraded execution and a disconnect, never an inferred online.
			detail := func() map[string]any {
				rec := request("/api/probes/"+r.session.ProbeID, adminCred)
				if rec.Code != 200 {
					t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				return body
			}
			if got := detail(); got["enrollment_state"] != "pending" || got["connection_status"] != "disconnected" || got["execution_status"] != "degraded" {
				t.Fatalf("initial evidence projection: %+v", got)
			}
			connector := repository.NewProbeConnectorStore(r.f.db)
			lease := domain.ProbeConnectorLease{ProbeID: r.session.ProbeID, OwnerID: r.session.OwnerID, Generation: r.session.ConnectionGeneration}
			if err := connector.SetConnectorConnected(ctx, lease, true); err != nil {
				t.Fatal(err)
			}
			if _, err := r.f.db.ExecContext(ctx, "UPDATE probe_connections SET state = 'active', activated_at = ? WHERE probe_id = ?", time.Now().UTC(), r.session.ProbeID); err != nil {
				t.Fatal(err)
			}
			got := detail()
			if got["enrollment_state"] != "active" || got["connection_status"] != "online" || got["execution_status"] != "degraded" {
				t.Fatalf("connected without runtime lease: %+v", got)
			}

			// Monitor-level reads share the same vocabulary and proof.
			rec := request(fmt.Sprintf("/api/monitors/%d/probes", r.monitor), viewerCred)
			var assignments handlers.MonitorProbeAssignmentsView
			if err := json.Unmarshal(rec.Body.Bytes(), &assignments); err != nil {
				t.Fatal(err)
			}
			if rec.Code != 200 || len(assignments.Assignments) != 1 || assignments.Assignments[0].SyncStatus == nil || *assignments.Assignments[0].SyncStatus != "applied" ||
				assignments.Assignments[0].DesiredConfigRevision == nil || *assignments.Assignments[0].DesiredConfigRevision != "1" || assignments.Assignments[0].AppliedConfigRevision == nil {
				t.Fatalf("assignment diagnostics: %d %s", rec.Code, rec.Body.String())
			}
			rec = request(fmt.Sprintf("/api/monitors/%d/health", r.monitor), viewerCred)
			var healthView handlers.MonitorHealthView
			if err := json.Unmarshal(rec.Body.Bytes(), &healthView); err != nil {
				t.Fatal(err)
			}
			if rec.Code != 200 || len(healthView.Regions) != 1 || healthView.Regions[0].ConnectionStatus == nil || *healthView.Regions[0].ConnectionStatus != "online" ||
				healthView.Regions[0].ConfigSyncStatus == nil || *healthView.Regions[0].ConfigSyncStatus != "applied" {
				t.Fatalf("region diagnostics: %d %s", rec.Code, rec.Body.String())
			}

			// A held runtime lease proves execution readiness; expiry revokes
			// it observationally without touching connection state.
			if _, err := r.f.db.ExecContext(ctx, "INSERT INTO probe_runtime_owners (probe_id, owner_id, epoch, lease_until) VALUES (?, ?, 1, ?)", r.session.ProbeID, r.session.OwnerID, time.Now().Add(time.Minute).Unix()); err != nil {
				t.Fatal(err)
			}
			if got = detail(); got["execution_status"] != "ready" {
				t.Fatalf("held runtime lease: %+v", got)
			}
			if _, err := r.f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until = ? WHERE probe_id = ?", time.Now().Add(-time.Minute).Unix(), r.session.ProbeID); err != nil {
				t.Fatal(err)
			}
			got = detail()
			if got["execution_status"] != "degraded" || got["connection_status"] != "online" {
				t.Fatalf("expired owner must not fake readiness: %+v", got)
			}
			if _, err := r.f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = 0 WHERE probe_id = ?", r.session.ProbeID); err != nil {
				t.Fatal(err)
			}
			if got = detail(); got["connection_status"] != "disconnected" {
				t.Fatalf("expired session lease: %+v", got)
			}

			// A registered but never enrolled probe reports unreported state as
			// unconfigured/never_connected, and everything else stays null.
			r.f.remote(t, probeRegistryID3, "unrelated-fleet-member")
			rec = request("/api/probes/"+probeRegistryID3, adminCred)
			var bare map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &bare); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]any{
				"enrollment_state": "unconfigured", "connection_status": "never_connected", "execution_status": "unconfigured",
				"desired_config_revision": "0", "applied_config_revision": "0", "last_seen_at": nil,
				"agent_version": nil, "protocol_version": nil, "queue_bytes": nil, "oldest_queued_at": nil,
				"endpoint": nil, "tls_fingerprint": nil, "certificate_expires_at": nil, "credential_version": nil,
			} {
				if bare[field] != want {
					t.Fatalf("bare probe %s = %v, want %v", field, bare[field], want)
				}
			}
			if caps, ok := bare["capabilities"].([]any); !ok || len(caps) != 0 {
				t.Fatalf("bare probe capabilities must render an empty list: %v", bare["capabilities"])
			}
			bareDiag := bare["diagnostics"].(map[string]any)
			for _, section := range []string{"enrollment", "connection", "runtime", "watchdog"} {
				if bareDiag[section] != nil {
					t.Fatalf("bare probe invented %s evidence: %v", section, bareDiag[section])
				}
			}
			if config := bareDiag["config"].(map[string]any); config["desired"] != nil || config["applied"] != nil || config["sync_status"] != nil {
				t.Fatalf("bare probe invented config evidence: %v", config)
			}

			// Exclusive-cursor pagination covers the whole fleet in ID order.
			var paged []string
			cursor := ""
			for page := 0; ; page++ {
				if page > 5 {
					t.Fatal("pagination does not terminate")
				}
				path := "/api/probes?limit=1"
				if cursor != "" {
					path += "&cursor=" + cursor
				}
				rec := request(path, adminCred)
				if rec.Code != 200 {
					t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
				}
				var body struct {
					Items      []map[string]any `json:"items"`
					NextCursor *string          `json:"next_cursor"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				for _, item := range body.Items {
					paged = append(paged, item["id"].(string))
				}
				if body.NextCursor == nil {
					break
				}
				cursor = *body.NextCursor
			}
			if strings.Join(paged, ",") != r.session.ProbeID+","+probeRegistryID3+",local" {
				t.Fatalf("fleet order/pages: %v", paged)
			}

			// Secret exclusion: list never carries endpoint or pin material,
			// detail carries only the protocol-authorized admin fields.
			list := request("/api/probes", adminCred).Body.String()
			for _, secret := range append(fleetSecrets, "edge.example", "fingerprint", "endpoint") {
				if strings.Contains(list, secret) {
					t.Fatalf("list leaked %s: %s", secret, list)
				}
			}
			full := request("/api/probes/"+r.session.ProbeID, adminCred).Body.String()
			for _, secret := range fleetSecrets {
				if strings.Contains(full, secret) {
					t.Fatalf("detail leaked %s: %s", secret, full)
				}
			}
			for _, authorized := range []string{"wss://edge.example/ws/probe/v1", "tls_fingerprint", "certificate_expires_at", "credential_version"} {
				if !strings.Contains(full, authorized) {
					t.Fatalf("admin detail lost %s: %s", authorized, full)
				}
			}
		})
	}
}

// fleetAPIKey stores one active API key of the given scope for the user.
func fleetAPIKey(t *testing.T, r replayFixture, engine string, userID int64, scopes, token string) {
	t.Helper()
	_, err := r.f.db.ExecContext(t.Context(),
		"INSERT INTO api_keys (user_id, name, key_hash, active, scopes) VALUES (?, ?, ?, ?, ?)",
		userID, "fleet-"+engine, services.FingerprintAPIKey(token), true, scopes)
	if err != nil {
		t.Fatal(err)
	}
}
