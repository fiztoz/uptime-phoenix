package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The two drop counters exist so that a lossy install is distinguishable from a
// quiet one. That guarantee is only real if they are actually REGISTERED and
// actually SCRAPEABLE — a counter that increments an unregistered collector is
// exactly as invisible as the silent `default:` branch it replaced.
//
// So this asserts the scrape output, not the method call.
func TestPrometheusExporter_ExportsDropCounters(t *testing.T) {
	exporter := NewPrometheusExporter()

	exporter.IncWSFrameDropped()
	exporter.IncWSFrameDropped()
	exporter.IncBusEventDropped("heartbeat")
	exporter.SetWSConnectionsActive(3)
	exporter.ObserveInsightsStage("transitions", 250*time.Millisecond)
	exporter.IncInsightsCache("hit")

	handler, err := exporter.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	h, ok := handler.(http.Handler)
	if !ok {
		t.Fatalf("Handler returned %T, want http.Handler", handler)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"phoenix_ws_frames_dropped_total 2",
		`phoenix_eventbus_events_dropped_total{event_type="heartbeat"} 1`,
		"phoenix_ws_connections_active 3",
		`phoenix_insights_stage_duration_seconds_sum{stage="transitions"} 0.25`,
		`phoenix_insights_stage_duration_seconds_count{stage="transitions"} 1`,
		`phoenix_insights_cache_total{outcome="hit"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape output does not contain %q.\nDropped events would be invisible on /metrics.", want)
		}
	}
}
