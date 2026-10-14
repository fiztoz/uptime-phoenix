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

type observedLocalDeliveryReader struct {
	ports.LocalProbeDeliveryConfigReader
	calls  int
	onRead func(context.Context) error
}

func (r *observedLocalDeliveryReader) ReadAcknowledgedLocalDelivery(ctx context.Context, target domain.ProbeConfigTarget) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	r.calls++
	result, err := r.LocalProbeDeliveryConfigReader.ReadAcknowledgedLocalDelivery(ctx, target)
	if err == nil && r.onRead != nil {
		if err := r.onRead(ctx); err != nil {
			return nil, err
		}
	}
	return result, err
}

func TestLocalDeliveryConfigEvidenceAPI(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			monitor := seedLocalDelivery(t, f)
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
			reader := services.NewMonitorRegionalService(services.NewMonitorHealthService(repos.monitors, f.assignments, f.commits, access), f.registry)
			protector := localDeliveryProtector(t)
			if err := reader.SetDeliveryConfigReader(localDeliveryHubID, repository.NewRemoteProbeConfigSyncStore(f.db, nil, probe.NewHubConfigDecoder(checker.Get, notifier.Get), protector)); err != nil {
				t.Fatal(err)
			}
			local := &observedLocalDeliveryReader{LocalProbeDeliveryConfigReader: newLocalDeliveryStore(f.db, nil, protector)}
			if err := reader.SetLocalDeliveryConfigReader(local); err != nil {
				t.Fatal(err)
			}
			jwt := auth.NewJWTAuthenticator(strings.Repeat("local-document-test-", 4), 1, repos.users)
			authSvc := services.NewAuthService(repos.users, nil, jwt, nil)
			token, err := jwt.IssueSession(ctx, viewer)
			if err != nil {
				t.Fatal(err)
			}
			router := httppkg.NewRouter(
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
				router.ServeHTTP(rec, req)
				return rec
			}
			optIn := path + "?include_delivery_config=true"
			if request(optIn, "").Code != 401 || local.calls != 0 {
				t.Fatal("unauthenticated local document read")
			}
			baseline := request(path, token)
			if baseline.Code != 200 || request(path+"?include_delivery_config=false", token).Body.String() != baseline.Body.String() || local.calls != 0 || strings.Contains(baseline.Body.String(), "delivery_config_evidence") {
				t.Fatal("default local GET changed or read protected documents")
			}
			assertEvidence := func(state string, revision int64, documentMode, desiredMode domain.AlertDelivery) {
				t.Helper()
				rec := request(optIn, token)
				var view handlers.MonitorProbeAssignmentsView
				if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(rec.Body.Bytes(), &view) != nil || len(view.Assignments) != 1 || view.Assignments[0].ProbeID != domain.LocalProbeID {
					t.Fatalf("local diagnostic read failed: %d", rec.Code)
				}
				if view.AlertDelivery != string(desiredMode) || view.AlertDeliveryPending != (desiredMode != domain.AlertDeliveryRegional) {
					t.Fatal("document ACK changed desired state or activated paging")
				}
				e := view.Assignments[0].DeliveryConfigEvidence
				if e == nil || e.State != state {
					t.Fatal("incorrect local evidence state")
				}
				if revision > 0 {
					if e.ConfigRevision == nil || *e.ConfigRevision != strconv.FormatInt(revision, 10) || e.Generation == nil || *e.Generation != strconv.FormatInt(view.Assignments[0].Generation, 10) || e.AlertDelivery == nil || *e.AlertDelivery != string(documentMode) {
						t.Fatal("exact local tuple/mode lost")
					}
				} else if e.ConfigRevision != nil || e.Generation != nil || e.AlertDelivery != nil {
					t.Fatal("unproven local facts exposed")
				}
				for _, forbidden := range []string{"protected_payload", "sha256", "PasswordHash", "config_document", localDeliveryHubID} {
					if strings.Contains(rec.Body.String(), forbidden) {
						t.Fatal("local diagnostic leaked internal document metadata")
					}
				}
			}
			assertEvidence(services.DeliveryConfigUnreported, 0, "", domain.AlertDeliveryRegional)
			first := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC().Truncate(time.Microsecond))
			assertEvidence(services.DeliveryConfigUnreported, 0, "", domain.AlertDeliveryRegional)
			activateLocalDelivery(t, f, f.db, first, 0)
			assertEvidence(services.DeliveryConfigAcknowledged, first.Revision, domain.AlertDeliveryRegional, domain.AlertDeliveryRegional)
			editLocalDeliveryMode(t, f, monitor)
			second := prepareLocalDeliveryRevision(t, f, f.db, first.Revision, time.Now().UTC().Truncate(time.Microsecond))
			assertEvidence(services.DeliveryConfigAcknowledged, first.Revision, domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate)
			activateLocalDelivery(t, f, f.db, second, first.Revision)
			assertEvidence(services.DeliveryConfigAcknowledged, second.Revision, domain.AlertDeliveryAggregate, domain.AlertDeliveryAggregate)
			// A current binary that cannot validate a retained dependency must
			// refuse the complete response, like the existing remote reader.
			originalReader := local.LocalProbeDeliveryConfigReader
			local.LocalProbeDeliveryConfigReader = repository.NewLocalProbeDeliveryConfigStore(f.db, probe.NewLocalDeliveryConfigDecoder(func(string) (ports.Checker, bool) { return nil, false }, notifier.Get), protector)
			rejected := request(optIn, token)
			local.LocalProbeDeliveryConfigReader = originalReader
			if rejected.Code != 503 || strings.Contains(rejected.Body.String(), "delivery_config_evidence") || strings.Contains(rejected.Body.String(), "checker") {
				t.Fatal("semantic decoder failure exposed partial facts or diagnostics")
			}
			local.onRead = func(ctx context.Context) error {
				_, err := repository.NewProbeAssignmentStore(f.db).ReplaceWithBindings(ctx, monitor, 2, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, domain.AlertDeliveryBoth, nil)
				return err
			}
			changed := request(optIn, token)
			local.onRead = nil
			if changed.Code != 409 || !strings.Contains(changed.Body.String(), "assignment_changed") || strings.Contains(changed.Body.String(), "delivery_config_evidence") {
				t.Fatal("local CAS interleaving returned a mixed response")
			}
			assertEvidence(services.DeliveryConfigAcknowledged, second.Revision, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth)
			if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_config_applied_receipts WHERE probe_id = ? AND revision = ?", domain.LocalProbeID, second.Revision); err != nil {
				t.Fatal(err)
			}
			assertEvidence(services.DeliveryConfigUnverified, 0, "", domain.AlertDeliveryBoth)
			if err := access.RevokeMonitor(ctx, viewer, monitor); err != nil {
				t.Fatal(err)
			}
			before := local.calls
			hidden := request(optIn, token)
			missing := request("/api/monitors/9223372036854775807/probes?include_delivery_config=true", token)
			if hidden.Code != 404 || missing.Code != 404 || hidden.Body.String() != missing.Body.String() || local.calls != before {
				t.Fatal("hidden/missing local monitor read confidential documents")
			}
		})
	}
}
