package repository_test

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestProbeWatchdogSettingsAPIContract proves #120 end to end on both engines
// through the production router, auth middleware, handler and the existing
// ProbeWatchdogSettingsStore. Watchdog settings stay off until an explicit
// valid PUT; only an admin session or an admin write-scope API key may read or
// write them; every refused request leaves settings and channel links
// unchanged; a saved setting enters the probe config pipeline as a new
// prepared revision and is not reported as applied until the probe's receipt.
// The phases share one store per engine, so each is a block, not a subtest.
func TestProbeWatchdogSettingsAPIContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			h := newHubWatchdogFixture(t, engine)
			f := h.f
			ctx := t.Context()
			var users ports.UserRepository
			var apiKeys ports.APIKeyRepository
			if engine == "sqlite" {
				users, apiKeys = sqlite.NewRepository(f.db).UserRepo, sqlite.NewAPIKeyRepo(f.db)
			} else {
				users, apiKeys = mariadb.NewRepository(f.db).UserRepo, mariadb.NewAPIKeyRepo(f.db)
			}
			admin, viewer := f.user(t), f.user(t)
			adminUser, err := users.GetByID(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			adminUser.IsAdmin = true
			if err := users.Update(ctx, adminUser); err != nil {
				t.Fatal(err)
			}
			jwt := auth.NewJWTAuthenticator(strings.Repeat("watchdog-api-signing-", 4), 1, users)
			authSvc := services.NewAuthService(users, apiKeys, jwt, nil)
			session := func(id int64) string {
				token, err := jwt.IssueSession(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return "Bearer " + token
			}
			key := func(owner int64, scope string) string {
				k, _, err := authSvc.CreateAPIKey(ctx, owner, "watchdog-"+scope, []string{scope}, nil)
				if err != nil {
					t.Fatal(err)
				}
				return "ApiKey " + k
			}
			adminSession, viewerSession := session(admin), session(viewer)
			adminWrite, adminRead, viewerWrite := key(admin, "write"), key(admin, "read"), key(viewer, "write")

			settingsStore := repository.NewProbeWatchdogSettingsStore(f.db)
			// router builds the production router; opts mirrors bootstrap.Run,
			// which passes NewProbeWatchdogHandlers(store, cfg.ProbesEnabled).
			router := func(opts httppkg.RouterOptions) http.Handler {
				return httppkg.NewRouter(
					handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					authSvc, services.NewAccessService(users, nil, nil, nil), apiKeys, nil, embed.FS{}, opts, nil, "")
			}
			live := router(httppkg.RouterOptions{ProbeWatchdog: handlers.NewProbeWatchdogHandlers(settingsStore, true)})
			disabled := router(httppkg.RouterOptions{ProbeWatchdog: handlers.NewProbeWatchdogHandlers(settingsStore, false)})
			unwired := router(httppkg.RouterOptions{})
			do := func(e http.Handler, method, path, authorization, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if authorization != "" {
					req.Header.Set("Authorization", authorization)
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			path := "/api/probes/" + h.authority.ProbeID + "/watchdog"
			body := func(revision string, enabled bool, lost, recover, resend int, ids ...int64) string {
				raw, err := json.Marshal(map[string]any{"expected_revision": revision, "enabled": enabled,
					"lost_after_seconds": lost, "recover_after_seconds": recover, "resend_interval": resend, "notification_ids": append([]int64{}, ids...)})
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			type state struct {
				settings domain.ProbeWatchdogSettings
				rows     int
				links    int
				prepared int64
				applied  int64
			}
			configs := repository.NewProbeConfigStore(f.db)
			observe := func() state {
				t.Helper()
				s, err := settingsStore.Get(ctx, h.authority.ProbeID)
				if err != nil {
					t.Fatal(err)
				}
				latest, err := configs.Latest(ctx, h.authority.ProbeID)
				if err != nil {
					t.Fatal(err)
				}
				var applied int64
				if err := f.db.NewSelect().Table("probe_active_configs").Column("revision").Where("probe_id = ?", h.authority.ProbeID).Scan(ctx, &applied); err != nil {
					t.Fatal(err)
				}
				return state{s, replayCount(t, f, "probe_watchdog_settings"), replayCount(t, f, "probe_watchdog_notifications"), latest.Revision, applied}
			}
			unchanged := func(before state, what string) {
				t.Helper()
				after := observe()
				if after.settings.Revision != before.settings.Revision || after.settings.Enabled != before.settings.Enabled ||
					after.settings.LostAfterSeconds != before.settings.LostAfterSeconds || after.settings.RecoverAfterSeconds != before.settings.RecoverAfterSeconds ||
					after.settings.ResendInterval != before.settings.ResendInterval || !slices.Equal(after.settings.NotificationIDs, before.settings.NotificationIDs) ||
					after.rows != before.rows || after.links != before.links || after.prepared != before.prepared || after.applied != before.applied {
					t.Fatalf("%s changed state: before %+v after %+v", what, before, after)
				}
			}
			expect := func(rec *httptest.ResponseRecorder, status int, code, what string) {
				t.Helper()
				if rec.Code != status || (code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`)) {
					t.Fatalf("%s: %d %s; want %d %s", what, rec.Code, rec.Body.String(), status, code)
				}
			}

			// OffUntilExplicitPut: an enrolled probe has no saved row and reads
			// as revision-zero disabled defaults with no channels.
			initial := observe()
			if initial.rows != 0 || initial.links != 0 || initial.settings.Revision != 0 || initial.settings.Enabled || len(initial.settings.NotificationIDs) != 0 {
				t.Fatalf("watchdog not off by default: %+v", initial)
			}
			rec := do(live, "GET", path, adminSession, "")
			expect(rec, 200, "", "default GET")
			var view handlers.ProbeWatchdogSettingsView
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.Revision != "0" || view.Enabled || view.LostAfterSeconds != 90 || view.RecoverAfterSeconds != 30 || len(view.NotificationIDs) != 0 {
				t.Fatalf("default view: %+v %v", view, err)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("watchdog GET is cacheable")
			}
			unchanged(initial, "default GET")

			// AuthGate: anonymous and read-scope keys are unauthenticated for
			// this group; non-admin sessions and keys are forbidden.
			valid := body("0", true, 120, 45, 15, h.channel)
			for _, route := range []struct{ method, body string }{{"GET", ""}, {"PUT", valid}} {
				for _, principal := range []struct {
					header string
					status int
				}{{"", 401}, {"Bearer not-a-token", 401}, {adminRead, 401}, {viewerSession, 403}, {viewerWrite, 403}} {
					expect(do(live, route.method, path, principal.header, route.body), principal.status, "", fmt.Sprintf("%s as %q", route.method, strings.SplitN(principal.header, " ", 2)[0]))
				}
			}
			unchanged(initial, "denied callers")

			// Unavailable versus disabled, through the router fallback too.
			expect(do(unwired, "GET", path, adminSession, ""), 501, "watchdog_unavailable", "router fallback GET")
			expect(do(unwired, "PUT", path, adminWrite, valid), 501, "watchdog_unavailable", "router fallback PUT")
			expect(do(disabled, "GET", path, adminSession, ""), 503, "probes_disabled", "disabled GET")
			expect(do(disabled, "PUT", path, adminWrite, valid), 503, "probes_disabled", "disabled PUT")
			unchanged(initial, "unavailable/disabled")

			// Validation: nothing reaches storage.
			for _, bad := range []struct {
				name, path, body string
				status           int
				code             string
			}{
				{"malformed", path, `{"expected_revision":`, 400, "invalid_watchdog_settings"},
				{"truncated", path, valid[:len(valid)-1], 400, "invalid_watchdog_settings"},
				{"trailing value", path, valid + `{}`, 400, "invalid_watchdog_settings"},
				{"trailing garbage", path, valid + ` x`, 400, "invalid_watchdog_settings"},
				{"oversized", path, valid + strings.Repeat(" ", 64<<10), 413, "watchdog_settings_too_large"},
				{"unknown field", path, valid[:len(valid)-1] + `,"applied":true}`, 400, "invalid_watchdog_settings"},
				{"omitted enabled", path, strings.Replace(valid, `"enabled":true,`, ``, 1), 400, "invalid_watchdog_settings"},
				{"omitted channels", path, `{"enabled":true,"expected_revision":"0","lost_after_seconds":120,"recover_after_seconds":45,"resend_interval":15}`, 400, "invalid_watchdog_settings"},
				{"null timing", path, `{"enabled":true,"expected_revision":"0","notification_ids":[],"lost_after_seconds":null}`, 400, "invalid_watchdog_settings"},
				{"unknown field with omitted timings", path, `{"enabled":true,"expected_revision":"0","notification_ids":[],"lost_after":90}`, 400, "invalid_watchdog_settings"},
				{"numeric revision", path, strings.Replace(valid, `"expected_revision":"0"`, `"expected_revision":0`, 1), 400, "invalid_watchdog_settings"},
				{"duplicate channels", path, body("0", true, 120, 45, 15, h.channel, h.channel), 400, "invalid_watchdog_settings"},
				{"unknown channel", path, body("0", true, 120, 45, 15, h.channel, 987654321), 400, "invalid_watchdog_settings"},
				{"zero lost", path, body("0", true, 0, 45, 15, h.channel), 400, "invalid_watchdog_settings"},
				{"unknown probe", "/api/probes/00000000-0000-4000-8000-000000000000/watchdog", valid, 404, "probe_not_found"},
			} {
				expect(do(live, "PUT", bad.path, adminWrite, bad.body), bad.status, bad.code, bad.name)
				unchanged(initial, bad.name)
			}

			// PutThenGet: a write-scope admin key saves the exact settings and
			// an admin session reads them back.
			rec = do(live, "PUT", path, adminWrite, valid)
			expect(rec, 200, "", "valid PUT")
			if strings.Contains(rec.Body.String(), "applied") {
				t.Fatalf("PUT response claims application: %s", rec.Body.String())
			}
			want := handlers.ProbeWatchdogSettingsView{Revision: "1", Enabled: true, LostAfterSeconds: 120, RecoverAfterSeconds: 45, ResendInterval: 15, NotificationIDs: []int64{h.channel}}
			for _, r := range []*httptest.ResponseRecorder{rec, do(live, "GET", path, adminSession, ""), do(live, "GET", path, adminWrite, "")} {
				var got handlers.ProbeWatchdogSettingsView
				if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil || got.Revision != want.Revision || got.Enabled != want.Enabled ||
					got.LostAfterSeconds != want.LostAfterSeconds || got.RecoverAfterSeconds != want.RecoverAfterSeconds ||
					got.ResendInterval != want.ResendInterval || !slices.Equal(got.NotificationIDs, want.NotificationIDs) {
					t.Fatalf("round trip: %d %+v %v; want %+v", r.Code, got, err, want)
				}
			}
			saved := observe()
			if saved.rows != 1 || saved.links != 1 || saved.settings.Revision != 1 {
				t.Fatalf("persisted: %+v", saved)
			}
			// An identical retry at the new revision is a no-op.
			expect(do(live, "PUT", path, adminSession, body("1", true, 120, 45, 15, h.channel)), 200, "", "same-value retry")
			unchanged(saved, "same-value retry")

			// StaleRevision: 409 and the settings and channel links stay.
			expect(do(live, "PUT", path, adminWrite, body("0", false, 300, 60, 0)), 409, "watchdog_revision_conflict", "stale revision")
			unchanged(saved, "stale revision")

			// ConfigPipeline: the saved setting prepares a new probe config
			// revision; the probe's applied config stays the old one until its
			// own receipt, so saving fabricates no receipt.
			target := domain.ProbeConfigTarget{HubID: h.authority.HubID, ProbeID: h.authority.ProbeID}
			next, err := h.syncer.RefreshRemote(ctx, target, h.at)
			if err != nil || next.Revision <= saved.prepared {
				t.Fatalf("saved settings did not prepare a new revision: %+v %v (was %d)", next, err, saved.prepared)
			}
			reader := repository.NewProbeWatchdogConfigReader(h.store, h.authority, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get))
			loaded, err := reader.Load(ctx)
			if err != nil || loaded.Metadata.Revision != saved.applied || loaded.Watchdog.LostAfterSeconds != 90 || loaded.Watchdog.Enabled {
				t.Fatalf("saved settings reported as applied before a receipt: %+v %v", loaded.Watchdog, err)
			}
			lease := domain.ProbeConnectorLease{ProbeID: h.authority.ProbeID, OwnerID: h.authority.RuntimeOwner.OwnerID, Generation: h.authority.HealthGeneration}
			if err := h.syncer.RecordRemoteApplied(ctx, lease, domain.ProbeActiveConfig{ProbeConfigTarget: next.ProbeConfigTarget, Revision: next.Revision, SHA256: next.SHA256, AppliedAt: h.at, AssignmentCount: 1}); err != nil {
				t.Fatal(err)
			}
			loaded, err = reader.Load(ctx)
			if err != nil || loaded.Metadata.Revision != next.Revision || !loaded.Watchdog.Enabled || loaded.Watchdog.LostAfterSeconds != 120 ||
				loaded.Watchdog.RecoverAfterSeconds != 45 || loaded.Watchdog.ResendInterval != 15 || !slices.Equal(loaded.Watchdog.NotificationIDs, []int64{h.channel}) {
				t.Fatalf("applied graph after receipt: %+v %v", loaded.Watchdog, err)
			}

			// OmittedTimings: timing fields may be left out; they take the
			// phoenix-probe-admin defaults from the shared domain source.
			defaults := domain.DefaultProbeWatchdogSettings()
			rec = do(live, "PUT", path, adminWrite, fmt.Sprintf(`{"expected_revision":"1","enabled":true,"notification_ids":[%d]}`, h.channel))
			expect(rec, 200, "", "omitted timings")
			var omitted handlers.ProbeWatchdogSettingsView
			if err := json.Unmarshal(rec.Body.Bytes(), &omitted); err != nil || omitted.Revision != "2" || omitted.LostAfterSeconds != 90 ||
				omitted.RecoverAfterSeconds != 30 || omitted.ResendInterval != 0 {
				t.Fatalf("omitted timings: %+v %v; want revision 2 with 90/30/0", omitted, err)
			}
			if got := observe().settings; got.Revision != 2 || got.LostAfterSeconds != defaults.LostAfterSeconds ||
				got.RecoverAfterSeconds != defaults.RecoverAfterSeconds || got.ResendInterval != defaults.ResendInterval {
				t.Fatalf("omitted timings persisted: %+v; want the shared defaults", got)
			}

			// DisabledProbe: a disabled registration refuses the write with a
			// distinct 409 and changes nothing.
			if _, err := f.db.NewUpdate().Table("probes").Set("enabled = ?", false).Where("id = ?", h.authority.ProbeID).Exec(ctx); err != nil {
				t.Fatal(err)
			}
			before := observe()
			expect(do(live, "PUT", path, adminWrite, body("2", false, 120, 45, 15)), 409, "probe_disabled", "disabled probe")
			unchanged(before, "disabled probe")
		})
	}
}
