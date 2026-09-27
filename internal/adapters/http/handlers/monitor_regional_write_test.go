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

type assignmentWriteFake struct {
	result *services.ProbeAssignmentWriteResult
	err    error
	req    services.ProbeAssignmentRequest
	id     int64
	calls  int
}

func (f *assignmentWriteFake) Replace(_ context.Context, monitorID int64, req services.ProbeAssignmentRequest) (*services.ProbeAssignmentWriteResult, error) {
	f.calls++
	f.id, f.req = monitorID, req
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func regionalWriteRequest(h *MonitorRegionalHandlers, body string, userID int64) *httptest.ResponseRecorder {
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/monitors/7/probes", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("7")
	if userID != 0 {
		c.Set(ContextUserIDKey, userID)
	}
	if err := h.Replace(c); err != nil {
		e.HTTPErrorHandler(err, c)
	}
	return rec
}

// TestMonitorRegionalHTTPReplace freezes the write response contract: the same
// desired-set view as GET, with this write's unproven members marked pending
// even when a receipt proves an EARLIER document.
func TestMonitorRegionalHTTPReplace(t *testing.T) {
	const remote = "11111111-1111-4111-8111-111111111111"
	fake := regionalFixture()
	fake.assignments.Diag = map[string]services.ProbeDiagnosticSummary{
		remote: {ConfigSyncStatus: services.ProbeConfigSyncApplied, DesiredConfigRevision: 9007199254740993, AppliedConfigRevision: 9007199254740993},
	}
	writer := &assignmentWriteFake{result: &services.ProbeAssignmentWriteResult{
		Set:           fake.assignments.Set,
		PendingProbes: map[string]bool{remote: true},
	}}
	h := NewMonitorRegionalHandlers(fake, true)
	h.SetAssignments(writer)
	h.now = func() time.Time { return fake.health.Current.AsOf }

	rec := regionalWriteRequest(h, `{"expected_revision":"9007199254740993","probe_ids":["11111111-1111-4111-8111-111111111111","local"],"health_policy":"any_down","alert_delivery":"regional","bindings":[{"probe_id":"11111111-1111-4111-8111-111111111111","kind":"docker_socket","binding_key":"docker-main"}]}`, 1)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if writer.id != 7 || writer.req.ExpectedRevision != 9007199254740993 || writer.req.HealthPolicy != domain.HealthPolicyAnyDown || writer.req.AlertDelivery != "regional" ||
		writer.req.Bindings == nil || len(*writer.req.Bindings) != 1 || (*writer.req.Bindings)[0].BindingKey != "docker-main" {
		t.Fatalf("request not mapped: %+v", writer.req)
	}
	data, err := os.ReadFile("testdata/m5/assignment_replace.json")
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

	// Omitted bindings stay omitted (preserve), and revision parsing is strict.
	writer.calls = 0
	rec = regionalWriteRequest(h, `{"expected_revision":"1","probe_ids":["local"],"health_policy":"any_down"}`, 1)
	if rec.Code != 200 || writer.req.Bindings != nil {
		t.Fatalf("omitted bindings must stay nil: %d %s", rec.Code, rec.Body.String())
	}
	empty := regionalWriteRequest(h, `{"expected_revision":"1","probe_ids":["local"],"health_policy":"any_down","bindings":[]}`, 1)
	if empty.Code != 200 || writer.req.Bindings == nil || len(*writer.req.Bindings) != 0 {
		t.Fatalf("explicit empty bindings must clear: %d %s", empty.Code, empty.Body.String())
	}
}

func TestMonitorRegionalHTTPReplaceErrors(t *testing.T) {
	const body = `{"expected_revision":"4","probe_ids":["local"],"health_policy":"any_down"}`
	for _, tc := range []struct {
		name, body string
		user       int64
		enabled    bool
		writer     *assignmentWriteFake
		status     int
		code       string
		noCall     bool
	}{
		{"unauthenticated", body, 0, true, &assignmentWriteFake{}, 401, "unauthenticated", true},
		{"disabled", body, 1, false, &assignmentWriteFake{}, 503, "probes_disabled", true},
		{"no writer", body, 1, true, nil, 503, "assignment_unavailable", true},
		{"malformed body", `{"expected_revision":`, 1, true, &assignmentWriteFake{}, 400, "invalid_request", true},
		{"missing revision", `{"probe_ids":["local"],"health_policy":"any_down"}`, 1, true, &assignmentWriteFake{}, 400, "invalid_expected_revision", true},
		{"zero revision", `{"expected_revision":"0","probe_ids":["local"],"health_policy":"any_down"}`, 1, true, &assignmentWriteFake{}, 400, "invalid_expected_revision", true},
		{"non-decimal revision", `{"expected_revision":"1x","probe_ids":["local"],"health_policy":"any_down"}`, 1, true, &assignmentWriteFake{}, 400, "invalid_expected_revision", true},
		{"overflow revision", `{"expected_revision":"9223372036854775808","probe_ids":["local"],"health_policy":"any_down"}`, 1, true, &assignmentWriteFake{}, 400, "invalid_expected_revision", true},
		{"invalid members", body, 1, true, &assignmentWriteFake{err: services.ErrInvalidProbeIDs}, 400, "invalid_probe_ids", false},
		{"invalid policy", body, 1, true, &assignmentWriteFake{err: services.ErrInvalidPolicy}, 400, "invalid_health_policy", false},
		{"invalid delivery", body, 1, true, &assignmentWriteFake{err: services.ErrInvalidDelivery}, 400, "invalid_alert_delivery", false},
		{"invalid bindings", body, 1, true, &assignmentWriteFake{err: services.ErrInvalidBindings}, 400, "invalid_bindings", false},
		{"stale revision", body, 1, true, &assignmentWriteFake{err: services.ErrStaleRevision}, 409, "stale_revision", false},
		{"unknown probe", body, 1, true, &assignmentWriteFake{err: services.ErrUnknownProbe}, 409, "unknown_probe", false},
		{"disabled probe", body, 1, true, &assignmentWriteFake{err: services.ErrProbeUnavailable}, 409, "probe_unavailable", false},
		{"unsupported assignment", body, 1, true, &assignmentWriteFake{err: services.ErrUnsupportedAssignment}, 422, "unsupported_assignment", false},
		{"missing monitor", body, 1, true, &assignmentWriteFake{err: ports.ErrNotFound}, 404, "monitor_not_found", false},
		{"bad request", body, 1, true, &assignmentWriteFake{err: domain.ErrValidation}, 400, "invalid_request", false},
		{"storage", body, 1, true, &assignmentWriteFake{err: errors.New("secret DSN and endpoint")}, 503, "assignment_unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewMonitorRegionalHandlers(regionalFixture(), tc.enabled)
			var writer assignmentWriter
			if tc.writer != nil {
				writer = tc.writer
			}
			h.SetAssignments(writer)
			rec := regionalWriteRequest(h, tc.body, tc.user)
			var bodyView regionalErrorView
			if err := json.Unmarshal(rec.Body.Bytes(), &bodyView); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.status || bodyView.Code != tc.code || bodyView.Error == "" || strings.Contains(rec.Body.String(), "secret") {
				t.Fatalf("response %d %s", rec.Code, rec.Body.String())
			}
			if tc.writer != nil && tc.noCall && tc.writer.calls != 0 {
				t.Fatalf("rejected input reached the write: %d", tc.writer.calls)
			}
			if tc.writer != nil && !tc.noCall && tc.writer.calls != 1 {
				t.Fatalf("missing service call: %d", tc.writer.calls)
			}
		})
	}
}
