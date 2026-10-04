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
	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestM5AlertAPI crosses the production router, authorization, encrypted command
// ledger and fenced source receipt. A 202 must not manufacture an acknowledgement.
func TestM5AlertAPI(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newCommandFixture(t, engine)
			ctx := t.Context()
			var repos repositorySet
			if engine == "sqlite" {
				repos = sqliteRepositorySet(sqlite.NewRepository(f.f.db))
			} else {
				repos = mariadbRepositorySet(mariadb.NewRepository(f.f.db))
			}
			viewer, peer, admin := f.f.user(t), f.f.user(t), f.f.user(t)
			adminUser, err := repos.users.GetByID(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			adminUser.IsAdmin = true
			if err := repos.users.Update(ctx, adminUser); err != nil {
				t.Fatal(err)
			}
			for _, id := range []int64{viewer, peer} {
				if err := repos.userPermissions.Grant(ctx, &domain.UserPermission{UserID: id, MonitorID: &f.monitor}); err != nil {
					t.Fatal(err)
				}
			}
			access := services.NewAccessService(repos.users, repos.userPermissions, repos.monitorGroups, repos.monitors)
			health := services.NewMonitorHealthService(repos.monitors, f.f.assignments, f.f.commits, access)
			reader := services.NewMonitorRegionalService(health, f.f.registry)
			connections := repository.NewProbeConnectorStore(f.f.db)
			commands, err := services.NewProbeCommandService(f.commands, connections, f.protector, probe.AcknowledgementCodec{}, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
			if err != nil {
				t.Fatal(err)
			}
			incidents := repository.NewRegionalCommitStore(f.f.db)
			bus := eventbus.NewMemoryBus()
			defer bus.Close()
			events := bus.Subscribe("probe.command.status")
			commands.SetBrowserEvents(bus, incidents)
			alerts := services.NewProbeAlertService(access, incidents, f.f.registry, repository.NewProbeInstallationStore(f.f.db), commands, f.commands)
			regional := handlers.NewMonitorRegionalHandlers(reader, true)
			regional.SetAlerts(alerts)
			jwt := auth.NewJWTAuthenticator(strings.Repeat("m5-alert-test-key-", 4), 1, repos.users)
			authSvc := services.NewAuthService(repos.users, nil, jwt, nil)
			tokens := map[int64]string{}
			for _, id := range []int64{viewer, peer, admin} {
				tokens[id], err = jwt.IssueSession(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
			}
			e := httppkg.NewRouter(handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				authSvc, access, nil, nil, embed.FS{}, httppkg.RouterOptions{RegionalMonitors: regional}, nil, "")
			request := func(method, path string, userID int64, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if userID > 0 {
					req.Header.Set("Authorization", "Bearer "+tokens[userID])
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			base := fmt.Sprintf("/api/monitors/%d/probe-alerts", f.monitor)
			list := request(http.MethodGet, base, viewer, "")
			var rows []handlers.ProbeAlertView
			if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &rows) != nil || len(rows) != 1 || rows[0].ProbeID != f.session.ProbeID || rows[0].SourceAlertID != f.ack.SourceAlertID {
				t.Fatalf("attribution: %d %s", list.Code, list.Body.String())
			}
			path := base + "/" + f.ack.SourceAlertID + "/ack"
			body := fmt.Sprintf(`{"command_id":%q,"note":"private operator note"}`, f.ack.CommandID)
			for _, bad := range []string{body + ` {}`, `{"actor_display_name":"admin"}`, `{"note":4}`} {
				if rec := request(http.MethodPost, path, viewer, bad); rec.Code != 400 {
					t.Fatalf("invalid input: %d %s", rec.Code, rec.Body.String())
				}
			}
			if rec := request(http.MethodPost, path, 0, body); rec.Code != 401 {
				t.Fatalf("unauthenticated: %d", rec.Code)
			}
			queued := request(http.MethodPost, path, viewer, body)
			var receipt handlers.ProbeCommandView
			if queued.Code != 202 || json.Unmarshal(queued.Body.Bytes(), &receipt) != nil || receipt.Status != "pending" || receipt.RemoteConfirmed {
				t.Fatalf("premature confirmation: %d %s", queued.Code, queued.Body.String())
			}
			stored, err := f.commands.GetCommand(ctx, f.session.HubID, f.session.ProbeID, f.ack.CommandID)
			if err != nil || stored.RequestedBy != viewer {
				t.Fatalf("requester not durable: %+v %v", stored, err)
			}
			if retried := request(http.MethodPost, path, viewer, body); retried.Body.String() != queued.Body.String() {
				t.Fatal("same command retry changed receipt")
			}
			if rec := request(http.MethodPost, path, peer, body); rec.Code != 409 {
				t.Fatalf("peer reused identity: %d", rec.Code)
			}
			if rec := request(http.MethodGet, path+"/"+f.ack.CommandID, peer, ""); rec.Code != 404 {
				t.Fatalf("peer read private receipt: %d", rec.Code)
			}
			incident, err := incidents.GetIncident(ctx, f.ack.SourceAlertID)
			if err != nil || incident.AckedAt != nil {
				t.Fatalf("queue altered source incident: %+v %v", incident, err)
			}
			// A disconnected source leaves a pending durable command. Only a sent
			// command and a result under current session authority confirm it.
			appliedAt := time.Now().UTC().Truncate(time.Microsecond)
			result := domain.ProbeCommandOutcome{CommandID: f.ack.CommandID, Status: "applied", AppliedAt: &appliedAt, Message: "Incident acknowledged"}
			if _, err := commands.RecordCommandResult(ctx, f.session, result); err == nil {
				t.Fatal("unsent command confirmed")
			}
			dispatch, err := commands.NextCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true})
			if err != nil || dispatch == nil {
				t.Fatalf("dispatch: %+v %v", dispatch, err)
			}
			decoded, err := (probe.AcknowledgementCodec{}).DecodeAcknowledgement(ctx, dispatch.Payload)
			if err != nil || decoded.ActorDisplayName != fmt.Sprintf("User %d", viewer) || decoded.Note == nil || *decoded.Note != "private operator note" {
				t.Fatalf("authenticated actor lost: %v", err)
			}
			if _, err := commands.RecordCommandResult(ctx, f.session, result); err != nil {
				t.Fatal(err)
			}
			for _, id := range []int64{viewer, admin} {
				rec := request(http.MethodGet, path+"/"+f.ack.CommandID, id, "")
				if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &receipt) != nil || receipt.Status != "applied" || !receipt.RemoteConfirmed || strings.Contains(rec.Body.String(), "private operator note") {
					t.Fatalf("receipt: %d %s", rec.Code, rec.Body.String())
				}
			}
			var sawApplied bool
			for len(events) > 0 {
				ev := <-events
				raw, _ := json.Marshal(ev.Payload)
				if strings.Contains(string(raw), "private operator note") {
					t.Fatal("receipt event leaked note")
				}
				if strings.Contains(string(raw), `"status":"applied"`) {
					sawApplied = true
				}
			}
			if !sawApplied {
				t.Fatal("committed receipt not published")
			}
			if err := access.RevokeMonitor(ctx, viewer, f.monitor); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{base, path + "/" + f.ack.CommandID} {
				if rec := request(http.MethodGet, path, viewer, ""); rec.Code != 404 {
					t.Fatalf("revoked access: %d", rec.Code)
				}
			}
			if err := runNamedMigration(t, f.f, "073_probe_command_requester", "down"); err == nil {
				t.Fatal("downgrade discarded requester scope")
			}
		})
	}
}
