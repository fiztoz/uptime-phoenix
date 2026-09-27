package repository_test

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// recordingEnrollmentTransport captures the operator authorization consumed by
// the enrollment exchange and never returns it anywhere.
type recordingEnrollmentTransport struct {
	ports.ProbeConnectionTransport
	presented string
	runtime   string
	err       error
}

func (t *recordingEnrollmentTransport) Enroll(_ context.Context, _ domain.ProbeCredentialMetadata, enrollmentToken, runtimeToken string) error {
	t.presented, t.runtime = enrollmentToken, runtimeToken
	return t.err
}

// TestM5AdminOperations proves registration writes and durable administrative
// operations end to end on both engines: the M0 request fixtures drive real
// service wraps, receipts are persisted before any 202 and decode with the
// frozen client decoders, and no write-only input ever appears in a response.
func TestM5AdminOperations(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			ctx := t.Context()
			var users ports.UserRepository
			var apiKeys ports.APIKeyRepository
			if engine == "sqlite" {
				users = sqlite.NewRepository(r.f.db).UserRepo
				apiKeys = sqlite.NewAPIKeyRepo(r.f.db)
			} else {
				users = mariadb.NewRepository(r.f.db).UserRepo
				apiKeys = mariadb.NewAPIKeyRepo(r.f.db)
			}
			admin := r.f.user(t)
			adminUser, err := users.GetByID(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			adminUser.IsAdmin = true
			if err := users.Update(ctx, adminUser); err != nil {
				t.Fatal(err)
			}
			viewer := r.f.user(t)

			protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
			if err != nil {
				t.Fatal(err)
			}
			connections := repository.NewProbeConnectorStore(r.f.db)
			transport := &recordingEnrollmentTransport{}
			connector, err := services.NewProbeConnectorService(connections, connections, connections, protector,
				services.NewProbeConfigService(repository.NewProbeConfigStore(r.f.db), probe.ConfigInspector{}, protector),
				transport, probeRegistryID2, uuid.NewString(),
				func(int, time.Duration) time.Duration { return time.Second })
			if err != nil {
				t.Fatal(err)
			}
			commandStore := repository.NewProbeCommandStore(r.f.db, protector, probe.AcknowledgementCodec{}, protector, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
			rotations, err := services.NewProbeCredentialRotationService(commandStore, connections, protector, protector, probe.CredentialCommandCodec{})
			if err != nil {
				t.Fatal(err)
			}
			operations := repository.NewProbeOperationStore(r.f.db)
			resets := repository.NewProbeStreamResetStore(r.f.db, protector, protector, probe.StreamResetCodec{})
			adminSvc := services.NewProbeAdminService(operations, r.f.registry, repository.NewProbeInstallationStore(r.f.db),
				connections, connector, rotations, resets)
			fleetSvc := services.NewProbeFleetService(repository.NewProbeDiagnosticsStore(r.f.db))
			jwt := auth.NewJWTAuthenticator(strings.Repeat("m5-admin-test-key-", 4), 1, users)
			authSvc := services.NewAuthService(users, nil, jwt, nil)
			adminToken, err := jwt.IssueSession(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			viewerToken, err := jwt.IssueSession(ctx, viewer)
			if err != nil {
				t.Fatal(err)
			}
			e := httppkg.NewRouter(
				handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				authSvc, services.NewAccessService(users, nil, nil, nil), apiKeys, nil, embed.FS{}, httppkg.RouterOptions{
					ProbeFleet: handlers.NewProbeFleetHandlers(fleetSvc, true),
					ProbeAdmin: handlers.NewProbeAdminHandlers(adminSvc, fleetSvc, true),
				}, nil, "")

			request := func(method, path, token, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				if body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			authorization := fmt.Sprintf("%s%043d", "phx_probe_enroll_", 7)
			createBody := func(key string) string {
				return `{"key":"` + key + `","name":"Singapore","location":"ap-southeast-1","endpoint":"probe.example.test:443","tls_fingerprint":"` + fmt.Sprintf("%064d", 1) + `"}`
			}
			receipt := func(rec *httptest.ResponseRecorder) probe.OperationReceipt {
				t.Helper()
				out, err := probe.DecodeOperationReceipt(rec.Body.Bytes())
				if err != nil {
					t.Fatalf("receipt breaks the client decoder: %v\n%s", err, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), authorization) || (transport.runtime != "" && strings.Contains(rec.Body.String(), transport.runtime)) {
					t.Fatal("write-only input echoed in the receipt")
				}
				return out
			}
			enroll := func(probeID string) probe.OperationReceipt {
				t.Helper()
				rec := request(http.MethodPost, "/api/probes/"+probeID+"/enroll", adminToken,
					fmt.Sprintf(`{"enrollment_token":%q}`, authorization))
				if rec.Code != http.StatusAccepted {
					t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
				}
				out := receipt(rec)
				if out.Status != "succeeded" || out.Phase != "exchanged" || out.ProbeID != probeID {
					t.Fatalf("enroll receipt: %+v", out)
				}
				return out
			}

			// Authorization precedes every administrative write.
			if rec := request(http.MethodPost, "/api/probes", "", createBody("vm-sg")); rec.Code != 401 {
				t.Fatalf("missing authentication: %d %s", rec.Code, rec.Body.String())
			}
			if rec := request(http.MethodPost, "/api/probes", viewerToken, createBody("vm-sg")); rec.Code != 403 {
				t.Fatalf("non-admin registration write: %d %s", rec.Code, rec.Body.String())
			}

			// Registration create: the frozen network trust lands on the
			// registration and the response satisfies the client decoder.
			rec := request(http.MethodPost, "/api/probes", adminToken, createBody("vm-sg"))
			if rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
			}
			created, err := probe.DecodeProbeView(rec.Body.Bytes())
			if err != nil {
				t.Fatalf("create response breaks the client decoder: %v\n%s", err, rec.Body.String())
			}
			probeA := created.ID
			if created.Endpoint == nil || *created.Endpoint != "probe.example.test:443" || created.TLSFingerprint == nil {
				t.Fatalf("frozen network trust not reported: %s", rec.Body.String())
			}
			stored, err := r.f.registry.GetByID(ctx, probeA)
			if err != nil || stored.Endpoint != "probe.example.test:443" || stored.TLSPin != fmt.Sprintf("%064d", 1) {
				t.Fatalf("registration did not freeze the network trust: %+v %v", stored, err)
			}
			rec = request(http.MethodPost, "/api/probes", adminToken, createBody("vm-sg-2"))
			if rec.Code != http.StatusCreated {
				t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
			}
			created, err = probe.DecodeProbeView(rec.Body.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			probeB := created.ID

			// Patch with optimistic revision; a stale revision changes nothing.
			rec = request(http.MethodPatch, "/api/probes/"+probeA, adminToken, `{"name":"Singapore 2","location":"sg","enabled":true,"revision":"1"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
			}
			if rec = request(http.MethodPatch, "/api/probes/"+probeA, adminToken, `{"name":"x","location":"y","enabled":true,"revision":"1"}`); rec.Code != 409 {
				t.Fatalf("stale patch: %d %s", rec.Code, rec.Body.String())
			}
			if stored, err = r.f.registry.GetByID(ctx, probeA); err != nil || stored.Name != "Singapore 2" || stored.Revision != 2 || stored.Endpoint != "probe.example.test:443" {
				t.Fatalf("patch state: %+v %v", stored, err)
			}

			// Enrollment wraps the real connector: the connection is prepared
			// from the frozen trust and the receipt is durable before the 202.
			enrollA := enroll(probeA)
			enrollB := enroll(probeB)
			connection, err := connections.GetConnection(ctx, probeA)
			if err != nil || connection.State != "prepared" || connection.Endpoint != "wss://probe.example.test:443/ws/probe/v1" || connection.Fingerprint != fmt.Sprintf("%064d", 1) {
				t.Fatalf("prepared connection: %+v %v", connection, err)
			}
			storedOperation, err := operations.GetOperation(ctx, enrollA.OperationID)
			if err != nil || storedOperation.Status != domain.ProbeOperationSucceeded || storedOperation.Kind != domain.ProbeOperationEnroll {
				t.Fatalf("operation not durable: %+v %v", storedOperation, err)
			}
			if rec = request(http.MethodGet, "/api/probe-operations/"+enrollB.OperationID, adminToken, ""); rec.Code != http.StatusOK {
				t.Fatalf("operation read: %d %s", rec.Code, rec.Body.String())
			}

			// A failed exchange persists a failed receipt with a bounded error.
			transport.err = fmt.Errorf("dial failed on the pinned endpoint")
			rec = request(http.MethodPost, "/api/probes/"+probeA+"/enroll", adminToken,
				fmt.Sprintf(`{"enrollment_token":%q}`, authorization))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("failed enroll: %d %s", rec.Code, rec.Body.String())
			}
			failed := receipt(rec)
			if failed.Status != "failed" || failed.Error == nil || failed.Error.Code != "enrollment_failed" {
				t.Fatalf("failed receipt: %+v", failed)
			}
			if strings.Contains(failed.Error.Message, "dial") {
				t.Fatalf("failure leaked a raw error: %+v", failed.Error)
			}
			transport.err = nil

			// The runtime handshake activates the connections (protocol
			// section 6.2 step 6); rotation and reset both require it.
			for _, id := range []string{probeA, probeB} {
				if _, err := r.f.db.ExecContext(ctx, "UPDATE probe_connections SET state = 'active', activated_at = ? WHERE probe_id = ?", time.Now().UTC(), id); err != nil {
					t.Fatal(err)
				}
			}

			// Stream reset on B retains the caller-supplied operation identity
			// and durably prepares the immutable plan.
			resetOperationID := "d1123604-32c5-40aa-89f9-b62f93dceac2"
			newStream := "1395c134-da65-4d86-9b11-c9ce20c61748"
			rec = request(http.MethodPost, "/api/probes/"+probeB+"/reset-stream", adminToken,
				fmt.Sprintf(`{"stream_id":%q,"enrollment_operation_id":%q}`, newStream, resetOperationID))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
			}
			resetReceipt := receipt(rec)
			if resetReceipt.OperationID != resetOperationID || resetReceipt.Status != "succeeded" || resetReceipt.Phase != "prepared" {
				t.Fatalf("reset receipt: %+v", resetReceipt)
			}
			plan, err := resets.GetStreamReset(ctx, probeRegistryID2, probeB, resetOperationID)
			if err != nil || plan == nil || plan.Plan.PreviousStreamID == "" || plan.Plan.StreamID != newStream {
				t.Fatalf("reset plan not durable: %+v %v", plan, err)
			}

			// Credential rotation on A reuses the operation identity as its
			// rotation ID and returns the exact receipt.
			rec = request(http.MethodPost, "/api/probes/"+probeA+"/rotate-credential", adminToken, `{"credential_version":"2"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
			}
			rotationReceipt := receipt(rec)
			if rotationReceipt.Status != "succeeded" || rotationReceipt.Phase != "issued" {
				t.Fatalf("rotation receipt: %+v", rotationReceipt)
			}
			if _, err := commandStore.GetCredentialRotation(ctx, probeRegistryID2, probeA, rotationReceipt.OperationID); err != nil {
				t.Fatalf("rotation not durable under the operation identity: %v", err)
			}

			// The in-flight rotation and the prepared reset exclude each other
			// by contract; each rejected wrap is still a durable failed receipt.
			rec = request(http.MethodPost, "/api/probes/"+probeA+"/reset-stream", adminToken,
				fmt.Sprintf(`{"stream_id":%q,"enrollment_operation_id":%q}`, uuid.NewString(), uuid.NewString()))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("conflicting reset: %d %s", rec.Code, rec.Body.String())
			}
			if conflicting := receipt(rec); conflicting.Status != "failed" || conflicting.Error == nil || conflicting.Error.Code != "reset_conflict" {
				t.Fatalf("conflicting reset receipt: %+v", conflicting)
			}
			rec = request(http.MethodPost, "/api/probes/"+probeB+"/rotate-credential", adminToken, `{"credential_version":"2"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("conflicting rotation: %d %s", rec.Code, rec.Body.String())
			}
			if conflicting := receipt(rec); conflicting.Status != "failed" || conflicting.Error == nil || conflicting.Error.Code != "rotation_conflict" {
				t.Fatalf("conflicting rotation receipt: %+v", conflicting)
			}

			// The fleet detail reports the frozen trust and the diagnostics
			// without any protected material.
			detail := request(http.MethodGet, "/api/probes/"+probeA, adminToken, "").Body.String()
			for _, secret := range []string{"protected_credential", "key_hash", "phx_probe_", "enrollment_id", "stream_id"} {
				if strings.Contains(detail, secret) {
					t.Fatalf("fleet detail leaked %s: %s", secret, detail)
				}
			}
			if !strings.Contains(detail, "probe.example.test:443") {
				t.Fatalf("frozen network trust missing from detail: %s", detail)
			}
		})
	}
}
