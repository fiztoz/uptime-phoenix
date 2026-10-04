package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const adminProbeID = "e645246b-b176-4422-8ae5-b79629ee6a29"
const adminOperationID = "d1123604-32c5-40aa-89f9-b62f93dceac2"

// adminWireFixture returns bytes from the frozen M0 contract fixtures so the
// accepted request shapes cannot drift from the client decoders.
func adminWireFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "probe", "testdata", "v1", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// testOperatorAuthorization builds a fabricated write-only request value.
func testOperatorAuthorization() string {
	return fmt.Sprintf("%s%043d", "phx_probe_enroll_", 7)
}

type adminFake struct {
	createReq  services.ProbeRegistrationRequest
	patchReq   services.ProbeRegistrationPatch
	patchID    string
	enrollID   string
	enrollAuth string
	rotateID   string
	rotateVer  int64
	resetID    string
	resetNew   string
	resetOp    string
	operation  *domain.ProbeOperation
	err        error
	calls      int
}

func (f *adminFake) CreateRegistration(_ context.Context, req services.ProbeRegistrationRequest) (*domain.Probe, error) {
	f.calls++
	f.createReq = req
	if f.err != nil {
		return nil, f.err
	}
	return &domain.Probe{ID: adminProbeID, Key: req.Key, Name: req.Name, Location: req.Location, Kind: domain.ProbeKindRemote, Enabled: true, Endpoint: req.Endpoint, TLSPin: req.TLSPin, Revision: 1}, nil
}

func (f *adminFake) UpdateRegistration(_ context.Context, probeID string, patch services.ProbeRegistrationPatch) (*domain.Probe, error) {
	f.calls++
	f.patchID, f.patchReq = probeID, patch
	if f.err != nil {
		return nil, f.err
	}
	return &domain.Probe{ID: probeID, Key: "vm-sg", Name: patch.Name, Location: patch.Location, Kind: domain.ProbeKindRemote, Enabled: patch.Enabled, Revision: patch.ExpectedRevision + 1}, nil
}

func (f *adminFake) Enroll(_ context.Context, probeID, authorization string) (*domain.ProbeOperation, error) {
	f.calls++
	f.enrollID, f.enrollAuth = probeID, authorization
	if f.err != nil {
		return nil, f.err
	}
	return f.operation, nil
}

func (f *adminFake) RotateCredential(_ context.Context, probeID string, version int64) (*domain.ProbeOperation, error) {
	f.calls++
	f.rotateID, f.rotateVer = probeID, version
	if f.err != nil {
		return nil, f.err
	}
	return f.operation, nil
}

func (f *adminFake) ResetStream(_ context.Context, probeID, streamID, operationID string) (*domain.ProbeOperation, error) {
	f.calls++
	f.resetID, f.resetNew, f.resetOp = probeID, streamID, operationID
	if f.err != nil {
		return nil, f.err
	}
	return f.operation, nil
}

func (f *adminFake) Operation(_ context.Context, operationID string) (*domain.ProbeOperation, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.operation == nil || f.operation.OperationID != operationID {
		return nil, ports.ErrNotFound
	}
	return f.operation, nil
}

func adminFleetFake() *fleetReadFake {
	remote := services.ProbeFleetEntry{
		Registration: domain.Probe{ID: adminProbeID, Key: "vm-sg", Name: "Singapore", Location: "ap-southeast-1", Kind: domain.ProbeKindRemote, Enabled: true,
			Endpoint: "probe.example.test:443", TLSPin: fmt.Sprintf("%064d", 1), Revision: 1,
			CreatedAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)},
		Summary: services.ProbeDiagnosticSummary{EnrollmentState: services.ProbeEnrollmentUnconfigured, ConnectionStatus: services.ProbeConnectionNeverConnected, ExecutionStatus: services.ProbeExecutionUnconfigured},
	}
	return &fleetReadFake{entry: &remote}
}

// methodFor selects the handler by route shape under test.
func (h *ProbeAdminHandlers) methodFor(method, path string) func(echo.Context) error {
	switch {
	case strings.Contains(path, "/enroll"):
		return h.Enroll
	case strings.Contains(path, "/rotate-credential"):
		return h.RotateCredential
	case strings.Contains(path, "/reset-stream"):
		return h.ResetStream
	case strings.Contains(path, "/api/probe-operations"):
		return h.Operation
	case method == http.MethodPatch:
		return h.Update
	default:
		return h.Create
	}
}

func adminRequest(h *ProbeAdminHandlers, method, path, id, body string) *httptest.ResponseRecorder {
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c := e.NewContext(req, rec)
	if id != "" {
		c.SetParamNames("probe_id", "operation_id")
		c.SetParamValues(id, id)
	}
	c.Set(ContextUserIDKey, 1)
	if err := h.methodFor(method, path)(c); err != nil {
		e.HTTPErrorHandler(err, c)
	}
	return rec
}

func adminUnauthenticatedRequest(h *ProbeAdminHandlers, method, path string) *httptest.ResponseRecorder {
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	c := e.NewContext(req, rec)
	if err := h.methodFor(method, path)(c); err != nil {
		e.HTTPErrorHandler(err, c)
	}
	return rec
}

// TestProbeAdminContractParity feeds the frozen M0 request fixtures into the
// handlers and validates every response with the frozen client decoders, so
// the registered routes cannot drift from the wire contract.
func TestProbeAdminContractParity(t *testing.T) {
	fake := &adminFake{operation: &domain.ProbeOperation{
		OperationID: adminOperationID, ProbeID: adminProbeID, Kind: domain.ProbeOperationEnroll,
		Status: domain.ProbeOperationSucceeded, Phase: "exchanged",
		CreatedAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 15, 10, 10, 0, 0, time.UTC),
	}}
	h := NewProbeAdminHandlers(fake, adminFleetFake(), true)

	rec := adminRequest(h, http.MethodPost, "/api/probes", "", string(adminWireFixture(t, "http-probe-create.json")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := probe.DecodeProbeView(rec.Body.Bytes()); err != nil {
		t.Fatalf("create response breaks the client decoder: %v\n%s", err, rec.Body.String())
	}
	if fake.createReq.Endpoint != "probe.example.test:443" || len(fake.createReq.TLSPin) != 64 {
		t.Fatalf("create request mapping: %+v", fake.createReq)
	}
	var fixture map[string]string
	if err := json.Unmarshal(adminWireFixture(t, "http-probe-create.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	if fake.createReq.TLSPin != fixture["tls_fingerprint"] {
		t.Fatalf("create pin drifted from the fixture: %q", fake.createReq.TLSPin)
	}

	rec = adminRequest(h, http.MethodPatch, "/api/probes/"+adminProbeID, adminProbeID, string(adminWireFixture(t, "http-probe-patch.json")))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := probe.DecodeProbeView(rec.Body.Bytes()); err != nil {
		t.Fatalf("patch response breaks the client decoder: %v", err)
	}
	if fake.patchReq.ExpectedRevision != 1 || !fake.patchReq.Enabled {
		t.Fatalf("patch request mapping: %+v", fake.patchReq)
	}

	authorization := testOperatorAuthorization()
	rec = adminRequest(h, http.MethodPost, "/api/probes/"+adminProbeID+"/enroll", adminProbeID,
		fmt.Sprintf(`{"enrollment_token":%q}`, authorization))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := probe.DecodeOperationReceipt(rec.Body.Bytes()); err != nil {
		t.Fatalf("enroll receipt breaks the client decoder: %v\n%s", err, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), authorization) {
		t.Fatal("write-only authorization echoed in the receipt")
	}

	rec = adminRequest(h, http.MethodPost, "/api/probes/"+adminProbeID+"/rotate-credential", adminProbeID,
		string(adminWireFixture(t, "http-rotate-credential-request.json")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := probe.DecodeOperationReceipt(rec.Body.Bytes()); err != nil {
		t.Fatalf("rotation receipt breaks the client decoder: %v", err)
	}
	if fake.rotateVer != 2 {
		t.Fatalf("rotation version mapping: %d", fake.rotateVer)
	}

	rec = adminRequest(h, http.MethodPost, "/api/probes/"+adminProbeID+"/reset-stream", adminProbeID,
		string(adminWireFixture(t, "http-reset-stream-request.json")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := probe.DecodeOperationReceipt(rec.Body.Bytes()); err != nil {
		t.Fatalf("reset receipt breaks the client decoder: %v", err)
	}
	if fake.resetOp != adminOperationID || fake.resetNew != "1395c134-da65-4d86-9b11-c9ce20c61748" {
		t.Fatalf("reset request mapping: %+v", fake)
	}

	// A failed receipt keeps the exact shape and a bounded redacted error.
	fake.operation = &domain.ProbeOperation{
		OperationID: adminOperationID, ProbeID: adminProbeID, Kind: domain.ProbeOperationEnroll,
		Status: domain.ProbeOperationFailed, Phase: "exchanging",
		Error:     &domain.ProbeOperationError{Code: "enrollment_failed", Message: "enrollment exchange failed"},
		CreatedAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 15, 10, 10, 0, 0, time.UTC),
	}
	rec = adminRequest(h, http.MethodGet, "/api/probe-operations/"+adminOperationID, adminOperationID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("operation read: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := probe.DecodeOperationReceipt(rec.Body.Bytes()); err != nil {
		t.Fatalf("failed receipt breaks the client decoder: %v\n%s", err, rec.Body.String())
	}
}

func TestProbeAdminHTTPErrors(t *testing.T) {
	authorization := testOperatorAuthorization()
	enrollBody := fmt.Sprintf(`{"enrollment_token":%q}`, authorization)
	createBody := `{"key":"k","name":"n","location":"","endpoint":"h:1","tls_fingerprint":"` + fmt.Sprintf("%064d", 1) + `"}`
	for _, tc := range []struct {
		name, method, path, id, body string
		unauthenticated              bool
		enabled, noSvc               bool
		fakeErr                      error
		status                       int
		code                         string
	}{
		{"unauthenticated", http.MethodPost, "/api/probes", "", "{}", true, true, false, nil, 401, "unauthenticated"},
		{"disabled", http.MethodPost, "/api/probes", "", "{}", false, false, false, nil, 503, "probes_disabled"},
		{"no service", http.MethodPost, "/api/probes", "", "{}", false, true, true, nil, 503, "fleet_unavailable"},
		{"malformed body", http.MethodPost, "/api/probes", "", `{"key":`, false, true, false, nil, 400, "invalid_registration"},
		{"unknown field", http.MethodPost, "/api/probes", "", `{"key":"k","name":"n","location":"","endpoint":"h:1","tls_fingerprint":"x","extra":1}`, false, true, false, nil, 400, "invalid_registration"},
		{"short fingerprint", http.MethodPost, "/api/probes", "", `{"key":"k","name":"n","location":"","endpoint":"h:1","tls_fingerprint":"x"}`, false, true, false, nil, 400, "invalid_registration"},
		{"create conflict", http.MethodPost, "/api/probes", "", createBody, false, true, false, ports.ErrConflict, 409, "registration_conflict"},
		{"patch stale", http.MethodPatch, "/api/probes/" + adminProbeID, adminProbeID, `{"name":"n","location":"","enabled":true,"revision":"1"}`, false, true, false, ports.ErrConflict, 409, "stale_revision"},
		{"enroll missing authorization", http.MethodPost, "/api/probes/" + adminProbeID + "/enroll", adminProbeID, "{}", false, true, false, nil, 400, "invalid_operation"},
		{"enroll conflict", http.MethodPost, "/api/probes/" + adminProbeID + "/enroll", adminProbeID, enrollBody, false, true, false, ports.ErrConflict, 409, "operation_conflict"},
		{"rotate malformed", http.MethodPost, "/api/probes/" + adminProbeID + "/rotate-credential", adminProbeID, `{"credential_version":"0"}`, false, true, false, nil, 400, "invalid_operation"},
		{"reset malformed", http.MethodPost, "/api/probes/" + adminProbeID + "/reset-stream", adminProbeID, `{"stream_id":"x","enrollment_operation_id":"y"}`, false, true, false, nil, 400, "invalid_operation"},
		{"operation missing", http.MethodGet, "/api/probe-operations/" + adminOperationID, adminOperationID, "", false, true, false, ports.ErrNotFound, 404, "operation_not_found"},
		{"operation malformed", http.MethodGet, "/api/probe-operations/not-a-uuid", "not-a-uuid", "", false, true, false, services.ErrInvalidOperation, 400, "invalid_operation_id"},
		{"storage", http.MethodPost, "/api/probes/" + adminProbeID + "/enroll", adminProbeID, enrollBody, false, true, false, errors.New("secret DSN and endpoint"), 503, "operation_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &adminFake{err: tc.fakeErr}
			var runner probeAdminRunner
			if !tc.noSvc {
				runner = fake
			}
			h := NewProbeAdminHandlers(runner, adminFleetFake(), tc.enabled)
			rec := adminRequest(h, tc.method, tc.path, tc.id, tc.body)
			if tc.unauthenticated {
				rec = adminUnauthenticatedRequest(h, tc.method, tc.path)
			}
			var bodyView regionalErrorView
			if err := json.Unmarshal(rec.Body.Bytes(), &bodyView); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.status || bodyView.Code != tc.code || bodyView.Error == "" || strings.Contains(rec.Body.String(), "secret") {
				t.Fatalf("response %d %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), authorization) {
				t.Fatal("write-only authorization echoed in an error")
			}
		})
	}
}
