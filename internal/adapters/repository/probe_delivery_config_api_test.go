package repository_test

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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

type countingDeliveryConfigReader struct {
	ports.RemoteProbeDeliveryConfigReader
	reads  int
	onRead func(context.Context) error
}

func (r *countingDeliveryConfigReader) ReadAcknowledgedRemoteDelivery(ctx context.Context, target domain.ProbeConfigTarget) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	r.reads++
	document, err := r.RemoteProbeDeliveryConfigReader.ReadAcknowledgedRemoteDelivery(ctx, target)
	if err == nil && r.onRead != nil {
		if err := r.onRead(ctx); err != nil {
			return nil, err
		}
	}
	return document, err
}

// TestDeliveryConfigEvidenceAPI traces encrypted retained documents through the
// nonsecret port, AccessService, JWT middleware, real router and explicit View.
// MariaDB runs only against an explicitly configured disposable test database.
func TestDeliveryConfigEvidenceAPI(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			writer, protector, monitor, _ := remoteSyncFixture(t, f)
			ctx := t.Context()
			var repos repositorySet
			if engine == "sqlite" {
				repos = sqliteRepositorySet(sqlite.NewRepository(f.db))
			} else {
				repos = mariadbRepositorySet(mariadb.NewRepository(f.db))
			}
			viewer := f.user(t)
			if err := repos.userPermissions.Grant(ctx, &domain.UserPermission{UserID: viewer, MonitorID: &monitor}); err != nil {
				t.Fatal(err)
			}
			access := services.NewAccessService(repos.users, repos.userPermissions, repos.monitorGroups, repos.monitors)
			health := services.NewMonitorHealthService(repos.monitors, f.assignments, f.commits, access)
			reader := services.NewMonitorRegionalService(health, f.registry)
			reader.SetDiagnostics(repository.NewProbeDiagnosticsStore(f.db))
			// Mirror bootstrap's fresh, read-only use of the hub semantic decoder.
			documents := &countingDeliveryConfigReader{RemoteProbeDeliveryConfigReader: repository.NewRemoteProbeConfigSyncStore(f.db, nil, probe.NewHubConfigDecoder(checker.Get, notifier.Get), protector)}
			if err := reader.SetDeliveryConfigReader(syncTarget().HubID, documents); err != nil {
				t.Fatal(err)
			}
			if err := reader.SetLocalDeliveryConfigReader(repository.NewLocalProbeDeliveryConfigStore(f.db, probe.NewLocalDeliveryConfigDecoder(checker.Get, notifier.Get), protector)); err != nil {
				t.Fatal(err)
			}
			jwt := auth.NewJWTAuthenticator(strings.Repeat("delivery-evidence-test-", 4), 1, repos.users)
			authSvc := services.NewAuthService(repos.users, nil, jwt, nil)
			token, err := jwt.IssueSession(ctx, viewer)
			if err != nil {
				t.Fatal(err)
			}
			e := httppkg.NewRouter(
				handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
				authSvc, access, nil, nil, embed.FS{}, httppkg.RouterOptions{RegionalMonitors: handlers.NewMonitorRegionalHandlers(reader, true)}, nil, "")
			path := fmt.Sprintf("/api/monitors/%d/probes", monitor)
			request := func(path, bearer string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if bearer != "" {
					req.Header.Set("Authorization", "Bearer "+bearer)
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			optIn := path + "?include_delivery_config=true"
			if request(optIn, "").Code != 401 || request(optIn, "invalid").Code != 401 || documents.reads != 0 {
				t.Fatal("unauthenticated document read")
			}
			baseline := request(path, token)
			if baseline.Code != 200 || request(path+"?include_delivery_config=false", token).Body.String() != baseline.Body.String() || documents.reads != 0 || strings.Contains(baseline.Body.String(), "delivery_config_evidence") {
				t.Fatal("default GET changed or read confidential documents")
			}
			assertEvidence := func(state string, revision int64, mode domain.AlertDelivery, desired domain.AlertDelivery) {
				t.Helper()
				rec := request(optIn, token)
				if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("diagnostic read status %d", rec.Code)
				}
				for _, forbidden := range []string{"private-sync-fixture", "example.test/hook", "sha256", "protected_payload", "config_document", "ProtectedCredential", "PasswordHash", "paging.aggregate.v1"} {
					if strings.Contains(rec.Body.String(), forbidden) {
						t.Fatalf("diagnostic view leaked forbidden field/setting %s", forbidden)
					}
				}
				var view handlers.MonitorProbeAssignmentsView
				if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
					t.Fatal(err)
				}
				if len(view.Assignments) != 1 || view.Assignments[0].ProbeID != syncTarget().ProbeID || view.AlertDelivery != string(desired) || view.AlertDeliveryPending != (desired != domain.AlertDeliveryRegional) {
					t.Fatal("desired state changed or document ACK activated paging")
				}
				evidence := view.Assignments[0].DeliveryConfigEvidence
				if evidence == nil || evidence.State != state {
					t.Fatalf("wrong evidence state: %+v", evidence)
				}
				if revision > 0 {
					if evidence.ConfigRevision == nil || *evidence.ConfigRevision != strconv.FormatInt(revision, 10) || evidence.Generation == nil || *evidence.Generation != strconv.FormatInt(view.Assignments[0].Generation, 10) || evidence.AlertDelivery == nil || *evidence.AlertDelivery != string(mode) {
						t.Fatal("exact acknowledged tuple/mode lost")
					}
				} else if evidence.ConfigRevision != nil || evidence.Generation != nil || evidence.AlertDelivery != nil {
					t.Fatal("unproven evidence invented tuple/mode")
				}
			}
			first, err := writer.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			assertEvidence(services.DeliveryConfigUnreported, 0, "", domain.AlertDeliveryRegional)
			lease, _ := remoteAppliedReceipt(t, f, writer, first)
			assertEvidence(services.DeliveryConfigAcknowledged, first.Revision, domain.AlertDeliveryRegional, domain.AlertDeliveryRegional)
			assignments := repository.NewProbeAssignmentStore(f.db)
			set, err := assignments.ReplaceWithBindings(ctx, monitor, 2, []string{probeRegistryID1}, domain.HealthPolicyAnyDown, domain.AlertDeliveryBoth, nil)
			if err != nil {
				t.Fatal(err)
			}
			second, err := writer.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			assertEvidence(services.DeliveryConfigAcknowledged, first.Revision, domain.AlertDeliveryRegional, domain.AlertDeliveryBoth)
			if err := writer.RecordRemoteApplied(ctx, lease, domain.ProbeActiveConfig{ProbeConfigTarget: syncTarget(), Revision: second.Revision, SHA256: second.SHA256, AssignmentCount: 1, AppliedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			assertEvidence(services.DeliveryConfigAcknowledged, second.Revision, domain.AlertDeliveryBoth, domain.AlertDeliveryBoth)
			// Commit a real desired-mode CAS edit after the document read but
			// before service revalidation: the router must refuse the mixed view.
			documents.onRead = func(ctx context.Context) error {
				set, err = assignments.ReplaceWithBindings(ctx, monitor, set.Revision, []string{probeRegistryID1}, domain.HealthPolicyAnyDown, domain.AlertDeliveryRegional, nil)
				return err
			}
			changed := request(optIn, token)
			documents.onRead = nil
			if changed.Code != 409 || !strings.Contains(changed.Body.String(), "assignment_changed") || strings.Contains(changed.Body.String(), "delivery_config_evidence") {
				t.Fatal("real CAS interleaving returned a mixed evidence view")
			}
			set, err = assignments.ReplaceWithBindings(ctx, monitor, set.Revision, []string{probeRegistryID1}, domain.HealthPolicyAnyDown, domain.AlertDeliveryBoth, nil)
			if err != nil {
				t.Fatal(err)
			}
			set, err = assignments.ReplaceWithBindings(ctx, monitor, set.Revision, []string{probeRegistryID1, domain.LocalProbeID}, domain.HealthPolicyAnyDown, domain.AlertDeliveryBoth, nil)
			if err != nil {
				t.Fatal(err)
			}
			builder, _ := sourceConfigBuilder(t, f, f.db)
			at := time.Now().UTC().Truncate(time.Microsecond)
			localMetadata, err := builder.Prepare(ctx, syncTarget().HubID, 0, at, at)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := activationRepo(f, f.db).ActivateLocal(ctx, ports.LocalActivationParams{Target: localMetadata.ProbeConfigTarget, Revision: localMetadata.Revision, SHA256: localMetadata.SHA256, ExpectedActiveRevision: 0, AppliedAt: at, AssignmentCount: -1}); err != nil {
				t.Fatal(err)
			}
			mixed := request(optIn, token)
			var mixedView handlers.MonitorProbeAssignmentsView
			if mixed.Code != 200 || json.Unmarshal(mixed.Body.Bytes(), &mixedView) != nil || len(mixedView.Assignments) != 2 {
				t.Fatal("mixed local/remote evidence unavailable")
			}
			for _, row := range mixedView.Assignments {
				evidence := row.DeliveryConfigEvidence
				if evidence == nil {
					t.Fatal("mixed-set evidence missing")
				}
				if row.ProbeID == domain.LocalProbeID {
					if evidence.State != services.DeliveryConfigAcknowledged || evidence.ConfigRevision == nil || *evidence.ConfigRevision != strconv.FormatInt(localMetadata.Revision, 10) || evidence.Generation == nil || *evidence.Generation != strconv.FormatInt(row.Generation, 10) || evidence.AlertDelivery == nil || *evidence.AlertDelivery != "both" {
						t.Fatal("mixed-set exact local document evidence lost")
					}
				} else if evidence.State != services.DeliveryConfigAcknowledged || evidence.AlertDelivery == nil || *evidence.AlertDelivery != "both" {
					t.Fatal("mixed-set remote configuration evidence lost")
				}
			}
			set, err = assignments.ReplaceWithBindings(ctx, monitor, set.Revision, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, domain.AlertDeliveryBoth, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = assignments.ReplaceWithBindings(ctx, monitor, set.Revision, []string{probeRegistryID1}, domain.HealthPolicyAnyDown, domain.AlertDeliveryBoth, nil); err != nil {
				t.Fatal(err)
			}
			assertEvidence(services.DeliveryConfigGenerationMismatch, 0, "", domain.AlertDeliveryBoth)
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_config_applied_receipts SET sha256 = ? WHERE probe_id = ? AND revision = ?", strings.Repeat("0", 64), syncTarget().ProbeID, second.Revision); err != nil {
				t.Fatal(err)
			}
			assertEvidence(services.DeliveryConfigUnverified, 0, "", domain.AlertDeliveryBoth)
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_config_applied_receipts SET sha256 = ? WHERE probe_id = ? AND revision = ?", second.SHA256, syncTarget().ProbeID, second.Revision); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ? AND revision = ?", append([]byte{1}, make([]byte, 31)...), syncTarget().ProbeID, second.Revision); err != nil {
				t.Fatal(err)
			}
			if rec := request(optIn, token); rec.Code != 503 || strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "delivery_config_evidence") {
				t.Fatal("authentication failure did not redact/refuse the whole response")
			}
			if err := access.RevokeMonitor(ctx, viewer, monitor); err != nil {
				t.Fatal(err)
			}
			before := documents.reads
			hidden := request(optIn, token)
			missing := request("/api/monitors/9223372036854775807/probes?include_delivery_config=true", token)
			if hidden.Code != 404 || missing.Code != 404 || hidden.Body.String() != missing.Body.String() || documents.reads != before {
				t.Fatal("revoked/missing monitor leaked existence or performed a document read")
			}
		})
	}
}
