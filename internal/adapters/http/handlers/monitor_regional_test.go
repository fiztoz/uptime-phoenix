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

type regionalReadFake struct {
	health      *services.MonitorRegionalHealth
	assignments *services.MonitorRegionalAssignments
	err         error
	calls       int
	hours       int
	at          time.Time
}

func (r *regionalReadFake) Assignments(context.Context, int64, int64) (*services.MonitorRegionalAssignments, error) {
	r.calls++
	return r.assignments, r.err
}
func (r *regionalReadFake) Health(_ context.Context, _, _ int64, hours int, at time.Time) (*services.MonitorRegionalHealth, error) {
	r.calls++
	r.hours = hours
	r.at = at
	return r.health, r.err
}

func regionalFixture() *regionalReadFake {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	remote := "11111111-1111-4111-8111-111111111111"
	probes := map[string]domain.Probe{remote: {ID: remote, Name: "Singapore", Location: "SG"}, domain.LocalProbeID: {ID: domain.LocalProbeID, Name: "Hub"}}
	fifty := float64(50)
	return &regionalReadFake{
		assignments: &services.MonitorRegionalAssignments{Set: domain.MonitorProbeAssignments{Revision: 9007199254740993, HealthPolicy: domain.HealthPolicyAnyDown, Assignments: []domain.ProbeAssignment{{ProbeID: remote, Generation: 9007199254740993, ResourceBinding: &domain.ProbeResourceBinding{Kind: "docker_socket", BindingKey: "docker-main"}}, {ProbeID: domain.LocalProbeID, Generation: 1}}}, Probes: probes},
		health:      &services.MonitorRegionalHealth{Current: domain.CurrentMonitorHealth{MonitorID: 7, Policy: domain.HealthPolicyAnyDown, AsOf: at, Health: domain.MonitorHealth{Status: domain.StatusUnknown, Counts: domain.ProbeHealthCounts{Assigned: 2, Up: 1, Unknown: 1}}, Regions: []domain.RegionalHealthEvidence{{ProbeID: remote, Status: domain.StatusUp, ObservedAt: at.Add(-3 * time.Minute), ReceivedAt: at, FreshFor: 125 * time.Second}, {ProbeID: domain.LocalProbeID, Status: domain.StatusUp, ObservedAt: at, ReceivedAt: at, FreshFor: 125 * time.Second}}}, Probes: probes, ProjectionVersion: 9007199254740993, Coverage: domain.HealthCoverage{Known: 30 * time.Minute, Unknown: 30 * time.Minute, UptimePercent: &fifty, CoveragePercent: &fifty}},
	}
}

func regionalRequest(h *MonitorRegionalHandlers, path, id string, userID int64) *httptest.ResponseRecorder {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, path, nil), rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	if userID != 0 {
		c.Set(ContextUserIDKey, userID)
	}
	var err error
	if strings.Contains(path, "/health") {
		err = h.Health(c)
	} else {
		err = h.Assignments(c)
	}
	if err != nil {
		e.HTTPErrorHandler(err, c)
	}
	return rec
}

func TestMonitorRegionalHTTPFixtures(t *testing.T) {
	fake := regionalFixture()
	h := NewMonitorRegionalHandlers(fake, true)
	h.now = func() time.Time { return fake.health.Current.AsOf.In(time.FixedZone("UTC+7", 7*3600)) }
	for _, tc := range []struct{ path, file string }{{"/api/monitors/7/health", "health.json"}, {"/api/monitors/7/probes", "assignments.json"}} {
		t.Run(tc.file, func(t *testing.T) {
			rec := regionalRequest(h, tc.path, "7", 1)
			if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
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
	if fake.hours != 24 || fake.at.Location() != time.UTC {
		t.Fatalf("coverage defaults/UTC: %d %v", fake.hours, fake.at)
	}
}

func TestMonitorRegionalHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		name, path, id string
		user           int64
		enabled        bool
		err            error
		status         int
		code           string
		calls          int
	}{
		{"unauthenticated", "/health", "7", 0, true, nil, 401, "unauthenticated", 0},
		{"invalid id", "/health", "0", 1, true, nil, 400, "invalid_monitor_id", 0},
		{"overflow id", "/probes", "9223372036854775808", 1, true, nil, 400, "invalid_monitor_id", 0},
		{"disabled", "/health", "7", 1, false, nil, 503, "probes_disabled", 0},
		{"invalid hours", "/health?hours=0", "7", 1, true, nil, 400, "invalid_hours", 0},
		{"too large", "/health?hours=721", "7", 1, true, nil, 400, "invalid_hours", 0},
		{"empty hours", "/health?hours=", "7", 1, true, nil, 400, "invalid_hours", 0},
		{"duplicate hours", "/health?hours=1&hours=2", "7", 1, true, nil, 400, "invalid_hours", 0},
		{"hidden", "/health", "7", 1, true, ports.ErrNotFound, 404, "monitor_not_found", 1},
		{"storage", "/probes", "7", 1, true, errors.New("secret DSN and endpoint"), 503, "regional_unavailable", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &regionalReadFake{err: tc.err}
			rec := regionalRequest(NewMonitorRegionalHandlers(fake, tc.enabled), tc.path, tc.id, tc.user)
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

func TestMonitorRegionalHTTPUnknownAndEmpty(t *testing.T) {
	fake := regionalFixture()
	fake.health.Current.Regions = nil
	fake.health.Coverage = domain.HealthCoverage{}
	rec := regionalRequest(NewMonitorRegionalHandlers(fake, true), "/health?hours=1", "7", 1)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if string(body["regions"]) != "[]" || string(body["uptime_percent"]) != "null" || string(body["coverage_percent"]) != "null" {
		t.Fatal(rec.Body.String())
	}
}
