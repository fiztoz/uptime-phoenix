package repository_test

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

type m8APIInspector struct {
	calls    atomic.Int32
	started  chan struct{}
	returned chan struct{}
}

func (i *m8APIInspector) InspectHost(ctx context.Context, _ domain.ProbeSSHHost) (domain.ProbeHostFacts, error) {
	i.calls.Add(1)
	close(i.started)
	<-ctx.Done()
	close(i.returned)
	return domain.ProbeHostFacts{}, ctx.Err()
}

type m8APIFactory struct{ inspector *m8APIInspector }

func (f m8APIFactory) NewInspector(ctx context.Context, _ []byte, _ string) (ports.ProbeHostInspector, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return f.inspector, m8PreflightFingerprint(2), nil
}

// TestM8PreflightAPI exercises the actual router/auth, service and database
// lifecycle on each engine. Only the discovery port is controlled; no external
// host or credential is contacted. Production SSH factory effects are covered
// separately by loopback tests in provisioning.
func TestM8PreflightAPI(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := t.Context()
			var users ports.UserRepository
			var apiKeys ports.APIKeyRepository
			if engine == "sqlite" {
				users = sqlite.NewRepository(f.db).UserRepo
				apiKeys = sqlite.NewAPIKeyRepo(f.db)
			} else {
				users = mariadb.NewRepository(f.db).UserRepo
				apiKeys = mariadb.NewAPIKeyRepo(f.db)
			}
			admin, viewer := f.user(t), f.user(t)
			user, err := users.GetByID(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			user.IsAdmin = true
			if err := users.Update(ctx, user); err != nil {
				t.Fatal(err)
			}
			jwt := auth.NewJWTAuthenticator(strings.Repeat("m8-test-signing-", 4), 1, users)
			authSvc := services.NewAuthService(users, apiKeys, jwt, nil)
			adminToken, err := jwt.IssueSession(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			viewerToken, err := jwt.IssueSession(ctx, viewer)
			if err != nil {
				t.Fatal(err)
			}
			writeKey, _, err := authSvc.CreateAPIKey(ctx, admin, "preflight-write", []string{"write"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			readKey, _, err := authSvc.CreateAPIKey(ctx, admin, "preflight-read", []string{"read"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			viewerKey, _, err := authSvc.CreateAPIKey(ctx, viewer, "viewer-write", []string{"write"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			inspector := &m8APIInspector{started: make(chan struct{}), returned: make(chan struct{})}
			store := repository.NewProbePreflightStore(f.db)
			svc, err := services.NewProbePreflightOperationService(ctx, store, m8APIFactory{inspector}, m8PreflightOwnerA, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := svc.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			router := func(enabled bool) http.Handler {
				return httppkg.NewRouter(
					handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					nil, nil, nil, nil, nil,
					authSvc, services.NewAccessService(users, nil, nil, nil), apiKeys, nil, embed.FS{}, httppkg.RouterOptions{ProbePreflights: handlers.NewProbePreflightHandlers(svc, enabled)}, nil, "")
			}
			e := router(true)
			request := func(method, path, authorization, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if authorization != "" {
					req.Header.Set("Authorization", authorization)
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			path := "/api/probe-host-preflights"
			privateInput := "ephemeral-ssh-input-not-a-real-credential"
			bodyBytes, err := json.Marshal(map[string]string{"operation_id": m8PreflightOp1, "ssh_address": "192.0.2.10:22", "ssh_user": "phoenix", "host_key_fingerprint": m8PreflightFingerprint(1), "private_key": privateInput})
			if err != nil {
				t.Fatal(err)
			}
			body := string(bodyBytes)
			for _, route := range []struct{ method, path, body string }{{"POST", path, body}, {"GET", path + "/" + m8PreflightOp1, ""}, {"POST", path + "/" + m8PreflightOp1 + "/cancel", ""}} {
				for _, principal := range []struct {
					header string
					code   int
				}{{"", 401}, {"Bearer " + viewerToken, 403}, {"ApiKey " + readKey, 401}, {"ApiKey " + viewerKey, 403}} {
					if rec := request(route.method, route.path, principal.header, route.body); rec.Code != principal.code {
						t.Fatalf("%s auth gate: %d, want %d", route.method, rec.Code, principal.code)
					}
				}
			}
			if m8PreflightCount(t, f.db) != 0 || inspector.calls.Load() != 0 {
				t.Fatal("unauthorized requests created/dispatched work")
			}
			for _, malformed := range []string{body[:len(body)-1] + `,"operation_id":"` + m8PreflightOp1 + `"}`, body[:len(body)-1] + `,"requested_by":"1"}`, strings.Repeat("x", (128<<10)+1)} {
				rec := request("POST", path, "Bearer "+adminToken, malformed)
				if rec.Code != 400 && rec.Code != 413 {
					t.Fatalf("strict input accepted: %d", rec.Code)
				}
			}
			rec := request("POST", path, "ApiKey "+writeKey, body)
			if rec.Code != 202 || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("submission: %d %s", rec.Code, rec.Body.String())
			}
			stored, err := store.ObservePreflight(ctx, m8PreflightOp1)
			if err != nil || stored.Identity.RequestedBy != admin || m8PreflightCount(t, f.db) != 1 {
				t.Fatalf("202 preceded durable actor-bound creation: %v", err)
			}
			select {
			case <-inspector.started:
			case <-time.After(5 * time.Second):
				t.Fatal("committed job never dispatched")
			}
			if rec = request("POST", path, "Bearer "+adminToken, body); rec.Code != 202 || inspector.calls.Load() != 1 {
				t.Fatalf("saturated retry dispatched or refused: %d", rec.Code)
			}
			changed := strings.Replace(body, "192.0.2.10:22", "192.0.2.11:22", 1)
			if rec = request("POST", path, "Bearer "+adminToken, changed); rec.Code != 409 {
				t.Fatalf("changed identity: %d", rec.Code)
			}
			if rec = request("POST", path+"/"+m8PreflightOp1+"/cancel", "Bearer "+adminToken, ""); rec.Code != 202 {
				t.Fatalf("cancellation intent: %d", rec.Code)
			}
			deadline := time.Now().Add(8 * time.Second)
			for {
				rec = request("GET", path+"/"+m8PreflightOp1, "Bearer "+adminToken, "")
				var view handlers.ProbePreflightView
				if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &view) != nil {
					t.Fatalf("observation: %d", rec.Code)
				}
				if view.Status == domain.ProbePreflightCanceled {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("cancellation never joined: %s", view.Status)
				}
				time.Sleep(20 * time.Millisecond)
			}
			select {
			case <-inspector.returned:
			default:
				t.Fatal("canceled persisted before inspector joined")
			}
			for _, forbidden := range []string{privateInput, "ssh_address", "host_key_fingerprint", "client_key_fingerprint", "requested_by", "owner_id", writeKey} {
				if strings.Contains(rec.Body.String(), forbidden) {
					t.Fatal("receipt disclosed private/internal input")
				}
			}
			// any is needed to inspect every actual SQL column for accidental secret persistence.
			var rows []map[string]any
			if err := f.db.NewSelect().TableExpr("probe_host_preflights").ColumnExpr("*").Scan(ctx, &rows); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), privateInput) || strings.Contains(string(encoded), writeKey) {
				t.Fatal("secret persisted in operation storage")
			}
			if rec = request("POST", path, "Bearer "+adminToken, body); rec.Code != 200 || inspector.calls.Load() != 1 {
				t.Fatalf("terminal retry changed outcome/dispatched: %d", rec.Code)
			}
			if rec = request("GET", path+"/"+m8PreflightOp2, "Bearer "+adminToken, ""); rec.Code != 404 {
				t.Fatalf("missing receipt: %d", rec.Code)
			}
			// Corruption is unavailable server state, never malformed input,
			// and neither reads, cancellation nor exact retries may repair it.
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_host_preflights SET deadline_at = ? WHERE operation_id = ?", stored.CreatedAt.Add(domain.ProbePreflightLifetime+time.Second), m8PreflightOp1); err != nil {
				t.Fatal(err)
			}
			for _, route := range []struct{ method, path, body string }{{"GET", path + "/" + m8PreflightOp1, ""}, {"POST", path + "/" + m8PreflightOp1 + "/cancel", ""}, {"POST", path, body}} {
				if rec := request(route.method, route.path, "Bearer "+adminToken, route.body); rec.Code != 503 || strings.Contains(rec.Body.String(), privateInput) {
					t.Fatalf("corrupt receipt did not refuse safely: %d", rec.Code)
				}
			}
			if inspector.calls.Load() != 1 {
				t.Fatal("corrupt retry dispatched SSH")
			}
			disabled := router(false)
			req := httptest.NewRequest("GET", path+"/"+m8PreflightOp1, nil)
			req.Header.Set("Authorization", "Bearer "+adminToken)
			rec = httptest.NewRecorder()
			disabled.ServeHTTP(rec, req)
			if rec.Code != 503 || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("disabled route returned fake success/SPA: %d", rec.Code)
			}
		})
	}
}
