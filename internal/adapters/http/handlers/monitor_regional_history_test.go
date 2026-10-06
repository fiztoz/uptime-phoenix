package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func regionalHistoryFixtureRows() []domain.RegionalObservation {
	return []domain.RegionalObservation{
		{ID: 9, MonitorID: 42, ProbeID: "e645246b-b176-4422-8ae5-b79629ee6a29", Status: domain.StatusUp, Ping: 12,
			Message: "OK", Important: true,
			ObservedAt:           time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
			ReceivedAt:           time.Date(2026, 9, 15, 10, 10, 0, 0, time.UTC),
			AssignmentGeneration: 9007199254740993, ConfigRevision: 9007199254740994},
		{ID: 10, MonitorID: 42, ProbeID: domain.LocalProbeID, Status: domain.StatusUnknown, Ping: 0,
			Message: "", Important: false,
			ObservedAt:           time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC),
			ReceivedAt:           time.Date(2026, 9, 15, 11, 0, 1, 0, time.UTC),
			AssignmentGeneration: 1, ConfigRevision: 0},
	}
}

// listHistoryHandler and historyChartHandler adapt the bound methods to the
// echo handler shape the driver expects.
func listHistoryHandler(h *MonitorRegionalHandlers) echo.HandlerFunc  { return h.ListHistory }
func historyChartHandler(h *MonitorRegionalHandlers) echo.HandlerFunc { return h.GetHistoryChart }

func regionalHistoryRequest(h *MonitorRegionalHandlers, pick func(*MonitorRegionalHandlers) echo.HandlerFunc, path, id, probeID string, userID int64) *httptest.ResponseRecorder {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, path, nil), rec)
	c.SetParamNames("id", "probe_id")
	c.SetParamValues(id, probeID)
	if userID != 0 {
		c.Set(ContextUserIDKey, userID)
	}
	if err := pick(h)(c); err != nil {
		e.HTTPErrorHandler(err, c)
	}
	return rec
}

// TestMonitorRegionalHistoryFixtures pins the wire rows to the frozen M0
// shape: the handler output and the checked-in fixture must both decode with
// probe.DecodeRegionalHeartbeat and match each other field for field.
func TestMonitorRegionalHistoryFixtures(t *testing.T) {
	fake := &regionalReadFake{observations: regionalHistoryFixtureRows()}
	h := NewMonitorRegionalHandlers(fake, true)
	rec := regionalHistoryRequest(h, listHistoryHandler, "/api/monitors/42/probes/x/heartbeats?order=asc", "42", "e645246b-b176-4422-8ae5-b79629ee6a29", 1)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	var gotRows []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &gotRows); err != nil {
		t.Fatal(err)
	}
	if len(gotRows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(gotRows))
	}
	for i, raw := range gotRows {
		if _, err := probe.DecodeRegionalHeartbeat(raw); err != nil {
			t.Fatalf("row %d violates the frozen decoder: %v\n%s", i, err, raw)
		}
	}

	data, err := os.ReadFile("testdata/m5/regional_history.json")
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
	if !equalJSON(want, got) {
		t.Fatalf("wire drift from fixture\nfixture: %s\nhandler: %s", data, rec.Body.String())
	}

	// The fixture itself must satisfy the frozen decoder in both directions.
	var fixtureRows []json.RawMessage
	if err = json.Unmarshal(data, &fixtureRows); err != nil {
		t.Fatal(err)
	}
	for i, raw := range fixtureRows {
		if _, err := probe.DecodeRegionalHeartbeat(raw); err != nil {
			t.Fatalf("fixture row %d violates the frozen decoder: %v", i, err)
		}
	}
}

// TestMonitorRegionalHistoryQuerySemantics keeps the existing
// hours/limit/order/important behavior of the unqualified list.
func TestMonitorRegionalHistoryQuerySemantics(t *testing.T) {
	base := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	observations := make([]domain.RegionalObservation, 0, 4)
	for i := 0; i < 4; i++ {
		observations = append(observations, domain.RegionalObservation{
			ID: int64(i + 1), MonitorID: 42, ProbeID: "local", Status: domain.StatusUp,
			Ping: i, Important: i%2 == 0,
			ObservedAt: base.Add(time.Duration(i) * time.Minute), ReceivedAt: base.Add(time.Duration(i) * time.Minute),
			AssignmentGeneration: 1, ConfigRevision: 1,
		})
	}
	h := NewMonitorRegionalHandlers(&regionalReadFake{observations: observations}, true)

	rec := regionalHistoryRequest(h, listHistoryHandler, "/api/monitors/42/probes/local/heartbeats?limit=2&order=asc", "42", "local", 1)
	var rows []RegionalHeartbeatView
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	// The cap applies to the most recent rows, then the requested order is applied.
	if len(rows) != 2 || rows[0].ID != 3 || rows[1].ID != 4 {
		t.Fatalf("desc-cap-then-reorder broken: %+v", rows)
	}

	rec = regionalHistoryRequest(h, listHistoryHandler, "/api/monitors/42/probes/local/heartbeats?important=true&order=asc", "42", "local", 1)
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != 1 || rows[1].ID != 3 {
		t.Fatalf("important filter broken: %+v", rows)
	}
}

// Equal observed times must be ordered by ID before selecting the most recent
// rows, and again when presenting that selection in ascending order.
func TestMonitorRegionalHistoryTimestampTies(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	observations := []domain.RegionalObservation{
		{ID: 1, ObservedAt: at.Add(-time.Minute), Important: true},
		{ID: 2, ObservedAt: at, Important: true},
		{ID: 3, ObservedAt: at, Important: true},
		{ID: 4, ObservedAt: at},
		// A replayed older sample must not outrank a newer timestamp by ID.
		{ID: 5, ObservedAt: at.Add(-time.Minute)},
	}
	for _, tc := range []struct {
		name, query string
		want        []int64
	}{
		{"descending", "order=desc", []int64{4, 3, 2, 5, 1}},
		{"ascending", "order=asc", []int64{1, 5, 2, 3, 4}},
		{"latest_descending", "order=desc&limit=1", []int64{4}},
		{"latest_ascending", "order=asc&limit=1", []int64{4}},
		{"cut_tie_descending", "order=desc&limit=2", []int64{4, 3}},
		{"cut_tie_ascending", "order=asc&limit=2", []int64{3, 4}},
		{"cross_tie_descending", "order=desc&limit=4", []int64{4, 3, 2, 5}},
		{"cross_tie_ascending", "order=asc&limit=4", []int64{5, 2, 3, 4}},
		{"important_latest", "important=true&order=desc&limit=1", []int64{3}},
		{"important_ascending", "important=true&order=asc&limit=2", []int64{2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewMonitorRegionalHandlers(&regionalReadFake{observations: observations}, true)
			rec := regionalHistoryRequest(h, listHistoryHandler, "/api/monitors/42/probes/local/heartbeats?"+tc.query, "42", "local", 1)
			if rec.Code != http.StatusOK {
				t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
			}
			var rows []RegionalHeartbeatView
			if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
			got := make([]int64, len(rows))
			for i, row := range rows {
				got[i] = row.ID
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("IDs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMonitorRegionalHistoryChart(t *testing.T) {
	base := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	observations := []domain.RegionalObservation{
		{ID: 1, MonitorID: 42, ProbeID: "local", Status: domain.StatusUp, Ping: 10, ObservedAt: base, ReceivedAt: base, AssignmentGeneration: 1, ConfigRevision: 1},
		{ID: 2, MonitorID: 42, ProbeID: "local", Status: domain.StatusUp, Ping: 20, ObservedAt: base.Add(time.Second), ReceivedAt: base.Add(time.Second), AssignmentGeneration: 1, ConfigRevision: 1},
		{ID: 3, MonitorID: 42, ProbeID: "local", Status: domain.StatusDown, Ping: 0, ObservedAt: base.Add(30 * time.Minute), ReceivedAt: base.Add(30 * time.Minute), AssignmentGeneration: 1, ConfigRevision: 1},
		{ID: 4, MonitorID: 42, ProbeID: "local", Status: domain.StatusUnknown, Ping: 0, ObservedAt: base.Add(45 * time.Minute), ReceivedAt: base.Add(45 * time.Minute), AssignmentGeneration: 1, ConfigRevision: 1},
	}
	h := NewMonitorRegionalHandlers(&regionalReadFake{observations: observations}, true)
	rec := regionalHistoryRequest(h, historyChartHandler, "/api/monitors/42/probes/local/heartbeats/chart?hours=2", "42", "local", 1)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var chart struct {
		Buckets           []chartBucketView      `json:"buckets"`
		DowntimeIntervals []downtimeIntervalView `json:"downtime_intervals"`
		UnknownIntervals  []downtimeIntervalView `json:"unknown_intervals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Buckets == nil || chart.DowntimeIntervals == nil || chart.UnknownIntervals == nil {
		t.Fatalf("chart arrays must never be null: %s", rec.Body.String())
	}
	if len(chart.Buckets) == 0 || chart.Buckets[0].Min != 10 || chart.Buckets[0].Max != 20 {
		t.Fatalf("latency must come from measured samples only: %+v", chart.Buckets)
	}
	if len(chart.DowntimeIntervals) != 1 || len(chart.UnknownIntervals) != 1 {
		t.Fatalf("UNKNOWN must stay visible as an interval: %s", rec.Body.String())
	}
}

func TestMonitorRegionalHistoryChartTimestampTieAtCap(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	observations := make([]domain.RegionalObservation, 2001)
	for i := range observations {
		observations[i] = domain.RegionalObservation{
			ID: int64(i + 1), ObservedAt: at, Status: domain.StatusUp, Ping: i + 1,
		}
	}
	h := NewMonitorRegionalHandlers(&regionalReadFake{observations: observations}, true)
	rec := regionalHistoryRequest(h, historyChartHandler, "/api/monitors/42/probes/local/heartbeats/chart", "42", "local", 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("chart: %d %s", rec.Code, rec.Body.String())
	}
	var chart chartDataView
	if err := json.Unmarshal(rec.Body.Bytes(), &chart); err != nil {
		t.Fatal(err)
	}
	// The 2000-row chart cap must discard ID 1, retaining the newest tied
	// sample (ID 2001). Ping identifies each sample in the aggregate.
	if len(chart.Buckets) != 1 || chart.Buckets[0].Min != 2 || chart.Buckets[0].Max != 2001 {
		t.Fatalf("chart selected older tied samples: %+v", chart.Buckets)
	}
}

func TestMonitorRegionalHistoryHTTPErrors(t *testing.T) {
	related := &regionalReadFake{observations: regionalHistoryFixtureRows()}
	for name, tc := range map[string]struct {
		h        *MonitorRegionalHandlers
		chart    bool
		userID   int64
		probe    string
		wantCode int
		wantBody string
	}{
		"unauthenticated":       {NewMonitorRegionalHandlers(related, true), false, 0, "local", 401, ""},
		"invalid_monitor_id":    {NewMonitorRegionalHandlers(related, true), false, 1, "local", 400, "invalid_monitor_id"},
		"probes_disabled":       {NewMonitorRegionalHandlers(related, false), false, 1, "local", 503, "probes_disabled"},
		"no_service":            {NewMonitorRegionalHandlers(nil, true), false, 1, "local", 503, "regional_unavailable"},
		"probe_not_found":       {NewMonitorRegionalHandlers(&regionalReadFake{historyErr: services.ErrProbeNotRelated}, true), false, 1, "local", 404, "probe_not_found"},
		"hidden_monitor":        {NewMonitorRegionalHandlers(&regionalReadFake{historyErr: domain.ErrNotFound}, true), false, 1, "local", 404, "monitor_not_found"},
		"chart_probe_not_found": {NewMonitorRegionalHandlers(&regionalReadFake{historyErr: services.ErrProbeNotRelated}, true), true, 1, "local", 404, "probe_not_found"},
		"chart_invalid_monitor": {NewMonitorRegionalHandlers(related, true), true, 1, "local", 400, "invalid_monitor_id"},
	} {
		t.Run(name, func(t *testing.T) {
			id := "42"
			if name == "invalid_monitor_id" || name == "chart_invalid_monitor" {
				id = "bogus"
			}
			handler := listHistoryHandler
			if tc.chart {
				handler = historyChartHandler
			}
			rec := regionalHistoryRequest(tc.h, handler, "/api/monitors/42/probes/x/heartbeats", id, tc.probe, tc.userID)
			if rec.Code != tc.wantCode {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if tc.wantBody != "" && !containsJSONCode(rec.Body.Bytes(), tc.wantBody) {
				t.Fatalf("want code %s in %s", tc.wantBody, rec.Body.String())
			}
		})
	}
}

func containsJSONCode(body []byte, code string) bool {
	var view regionalErrorView
	if err := json.Unmarshal(body, &view); err != nil {
		return false
	}
	return view.Code == code
}

func equalJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}
