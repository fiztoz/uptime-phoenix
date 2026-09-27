package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type fleetReadFake struct {
	page   *services.ProbeFleetPage
	entry  *services.ProbeFleetEntry
	err    error
	limit  int
	cursor string
	probe  string
	at     time.Time
	calls  int
}

func (f *fleetReadFake) List(_ context.Context, cursor string, limit int, at time.Time) (*services.ProbeFleetPage, error) {
	f.calls++
	f.cursor, f.limit, f.at = cursor, limit, at
	if f.err != nil {
		return nil, f.err
	}
	return f.page, nil
}

func (f *fleetReadFake) Detail(_ context.Context, probeID string, at time.Time) (*services.ProbeFleetEntry, error) {
	f.calls++
	f.probe, f.at = probeID, at
	if f.err != nil {
		return nil, f.err
	}
	return f.entry, nil
}

func fleetFixtureEntries() (services.ProbeFleetEntry, services.ProbeFleetEntry) {
	remote := services.ProbeFleetEntry{
		Registration: domain.Probe{ID: "11111111-1111-4111-8111-111111111111", Key: "singapore", Name: "Singapore", Location: "SG", Kind: domain.ProbeKindRemote, Enabled: true, Revision: 9007199254740993,
			CreatedAt: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 27, 11, 0, 0, 0, time.UTC)},
		Summary: services.ProbeDiagnosticSummary{EnrollmentState: services.ProbeEnrollmentActive, ConnectionStatus: services.ProbeConnectionOnline, ExecutionStatus: services.ProbeExecutionReady,
			ConfigSyncStatus: services.ProbeConfigSyncApplied, DesiredConfigRevision: 9007199254740993, AppliedConfigRevision: 9007199254740993,
			LastSeenAt: timePtr(time.Date(2026, 9, 27, 11, 58, 0, 0, time.UTC))},
		Diagnostics: domain.ProbeDiagnostics{
			Enrollment: &domain.ProbeEnrollmentFacts{Endpoint: "wss://edge.example/ws/probe/v1", TLSPin: strings.Repeat("a", 64), CredentialVersion: 3, CertificateVersion: 2,
				CertificateNotAfter: timePtr(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)), State: "active",
				PreparedAt: time.Date(2026, 9, 27, 10, 30, 0, 0, time.UTC), ActivatedAt: timePtr(time.Date(2026, 9, 27, 10, 31, 0, 0, time.UTC))},
			Session: &domain.ProbeSessionFacts{OwnerID: "44444444-4444-4444-8444-444444444444", Generation: 5, LeaseUntil: time.Date(2026, 9, 27, 12, 1, 0, 0, time.UTC), Connected: true},
			Runtime: &domain.ProbeRuntimeFacts{OwnerID: "44444444-4444-4444-8444-444444444444", Epoch: 9007199254740993, LeaseUntil: time.Date(2026, 9, 27, 12, 1, 0, 0, time.UTC)},
			Watchdog: &domain.ProbeWatchdogFacts{Status: "healthy", Version: 12, ConfigRevision: 9007199254740993, Armed: true,
				IncidentOpen: true, UpdatedAt: time.Date(2026, 9, 27, 11, 58, 0, 0, time.UTC)},
			Publication: &domain.ProbeConfigPublicationFacts{Revision: 9007199254740993, SchemaVersion: 1, SHA256: strings.Repeat("d", 64),
				SourceCreatedAt: time.Date(2026, 9, 27, 10, 45, 0, 0, time.UTC), EffectiveAt: time.Date(2026, 9, 27, 10, 45, 0, 0, time.UTC), StoredAt: time.Date(2026, 9, 27, 10, 45, 1, 0, time.UTC)},
			Applied: &domain.ProbeConfigAppliedFacts{Revision: 9007199254740993, SHA256: strings.Repeat("d", 64),
				AppliedAt: time.Date(2026, 9, 27, 11, 57, 0, 0, time.UTC), AssignmentCount: 3},
		},
	}
	local := services.ProbeFleetEntry{
		Registration: domain.Probe{ID: domain.LocalProbeID, Key: "local", Name: "Local", Location: "", Kind: domain.ProbeKindLocal, Enabled: true, Revision: 1,
			CreatedAt: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)},
		Summary: services.ProbeDiagnosticSummary{ExecutionStatus: services.ProbeExecutionReady},
	}
	return remote, local
}

func timePtr(at time.Time) *time.Time { return &at }

func fleetRequest(h *ProbeFleetHandlers, path, probeID string, userID int64) *httptest.ResponseRecorder {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, path, nil), rec)
	if probeID != "" {
		c.SetParamNames("probe_id")
		c.SetParamValues(probeID)
	}
	if userID != 0 {
		c.Set(ContextUserIDKey, userID)
	}
	var err error
	if probeID != "" {
		err = h.Detail(c)
	} else {
		err = h.List(c)
	}
	if err != nil {
		e.HTTPErrorHandler(err, c)
	}
	return rec
}

// TestProbeFleetHTTPFixtures freezes the fleet wire contract: the list keeps
// ProbeView exactly and the detail adds only the protocol-authorized fields
// and the safe diagnostics sections.
func TestProbeFleetHTTPFixtures(t *testing.T) {
	remote, local := fleetFixtureEntries()
	fake := &fleetReadFake{page: &services.ProbeFleetPage{Items: []services.ProbeFleetEntry{remote, local}}, entry: &remote}
	h := NewProbeFleetHandlers(fake, true)
	h.now = func() time.Time { return time.Date(2026, 9, 27, 19, 0, 0, 0, time.FixedZone("UTC+7", 7*3600)) }
	for _, tc := range []struct{ path, probe, file string }{
		{"/api/probes", "", "fleet_list.json"},
		{"/api/probes/11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111", "fleet_detail.json"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			rec := fleetRequest(h, tc.path, tc.probe, 1)
			if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if fake.at.Location() != time.UTC {
				t.Fatalf("diagnostic instant not UTC: %v", fake.at)
			}
			data, err := os.ReadFile("testdata/m5/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err = json.Unmarshal(data, &want); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("wire fixture drift:\n%s", rec.Body.String())
			}
		})
	}
	if fake.limit != services.DefaultProbeFleetPageLimit {
		t.Fatalf("list default limit: %d", fake.limit)
	}
}

func TestProbeFleetHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		name, path, probe string
		user              int64
		enabled, noSvc    bool
		err               error
		status            int
		code              string
		calls             int
	}{
		{"unauthenticated", "/api/probes", "", 0, true, false, nil, 401, "unauthenticated", 0},
		{"disabled", "/api/probes", "", 1, false, false, nil, 503, "probes_disabled", 0},
		{"unavailable service", "/api/probes", "", 1, true, true, nil, 503, "fleet_unavailable", 0},
		{"zero limit", "/api/probes?limit=0", "", 1, true, false, nil, 400, "invalid_limit", 0},
		{"huge limit", "/api/probes?limit=101", "", 1, true, false, nil, 400, "invalid_limit", 0},
		{"malformed limit", "/api/probes?limit=x", "", 1, true, false, nil, 400, "invalid_limit", 0},
		{"duplicate limit", "/api/probes?limit=1&limit=2", "", 1, true, false, nil, 400, "invalid_limit", 0},
		{"duplicate cursor", "/api/probes?cursor=a&cursor=b", "", 1, true, false, nil, 400, "invalid_cursor", 0},
		{"malformed cursor", "/api/probes?cursor=not-a-probe", "", 1, true, false, domain.ErrValidation, 400, "invalid_cursor", 1},
		{"invalid probe id", "/api/probes/x", "x", 1, true, false, domain.ErrValidation, 400, "invalid_probe_id", 1},
		{"missing probe", "/api/probes/11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111", 1, true, false, ports.ErrNotFound, 404, "probe_not_found", 1},
		{"storage", "/api/probes", "", 1, true, false, errors.New("secret DSN and endpoint"), 503, "fleet_unavailable", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fleetReadFake{err: tc.err}
			var h *ProbeFleetHandlers
			if tc.noSvc {
				h = NewProbeFleetHandlers(nil, tc.enabled)
			} else {
				h = NewProbeFleetHandlers(fake, tc.enabled)
			}
			rec := fleetRequest(h, tc.path, tc.probe, tc.user)
			var body regionalErrorView
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.status || body.Code != tc.code || body.Error == "" || strings.Contains(rec.Body.String(), "secret") || fake.calls != tc.calls {
				t.Fatalf("response %d %s calls=%d", rec.Code, rec.Body.String(), fake.calls)
			}
		})
	}
}

// TestProbeFleetHTTPSecretExclusion proves the read model cannot disclose
// protected material. Endpoint and TLS pin are the protocol-authorized admin
// detail fields; list keeps them out entirely.
func TestProbeFleetHTTPSecretExclusion(t *testing.T) {
	remote, local := fleetFixtureEntries()
	fake := &fleetReadFake{page: &services.ProbeFleetPage{Items: []services.ProbeFleetEntry{remote, local}}, entry: &remote}
	h := NewProbeFleetHandlers(fake, true)
	list := fleetRequest(h, "/api/probes", "", 1).Body.String()
	for _, secret := range []string{"protected_credential", "key_hash", "phx_", "enrollment_id", "stream_id", "protected_payload", "config_document", strings.Repeat("d", 64), "endpoint", "fingerprint", "owner_id"} {
		if strings.Contains(list, secret) {
			t.Fatalf("list leaked %s: %s", secret, list)
		}
	}
	detail := fleetRequest(h, "/api/probes/11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111", 1).Body.String()
	for _, secret := range []string{"protected_credential", "key_hash", "phx_", "enrollment_id", "stream_id", "protected_payload", "config_document", strings.Repeat("d", 64)} {
		if strings.Contains(detail, secret) {
			t.Fatalf("detail leaked %s: %s", secret, detail)
		}
	}
	for _, authorized := range []string{"wss://edge.example/ws/probe/v1", "tls_fingerprint", "certificate_expires_at", "credential_version", "diagnostics"} {
		if !strings.Contains(detail, authorized) {
			t.Fatalf("admin detail lost %s: %s", authorized, detail)
		}
	}
}
