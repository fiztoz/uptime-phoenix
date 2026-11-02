package notifier

import (
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Receivers must be able to route and dedupe a hub aggregate page apart from the
// per-region pages without parsing message text (#174).
func TestAggregateAlertIdentifiesItsScope(t *testing.T) {
	regional := domain.AlertContext{
		MonitorID: 3, MonitorName: "api", Status: domain.StatusDown, PreviousStatus: domain.StatusUp,
		DeliveryScope: domain.IncidentScopeRegional, SourceAlertID: "regional-1",
		ProbeID: "p-sg", ProbeName: "Singapore", ProbeLocation: "ap-southeast-1", Message: "timeout",
	}
	aggregate := domain.AlertContext{
		MonitorID: 3, MonitorName: "api", Status: domain.StatusDown, PreviousStatus: domain.StatusUp,
		AlertScope: domain.AlertScopeMonitor, DeliveryScope: domain.IncidentScopeAggregate, SourceAlertID: "aggregate-1",
		Message: "AGGREGATE api (any_down): Singapore: DOWN, Frankfurt: UP",
	}

	agg := webhookEventPayload(aggregate)
	if agg["delivery_scope"] != domain.IncidentScopeAggregate {
		t.Fatalf("aggregate delivery_scope=%v, want %q", agg["delivery_scope"], domain.IncidentScopeAggregate)
	}
	if agg["source_alert_id"] != "aggregate-1" {
		t.Fatalf("aggregate source_alert_id=%v, want aggregate-1", agg["source_alert_id"])
	}
	if _, ok := agg["probe"]; ok {
		t.Fatalf("aggregate payload must not name a probe: %v", agg["probe"])
	}

	reg := webhookEventPayload(regional)
	if reg["delivery_scope"] != domain.IncidentScopeRegional || reg["source_alert_id"] != "regional-1" {
		t.Fatalf("regional scope fields changed: %v / %v", reg["delivery_scope"], reg["source_alert_id"])
	}

	if got := alertTitle(regional); got != "api is DOWN" {
		t.Fatalf("regional title=%q, want unchanged %q", got, "api is DOWN")
	}
	if got, want := alertTitle(aggregate), "api is DOWN (aggregate)"; got != want {
		t.Fatalf("aggregate title=%q, want %q", got, want)
	}
	if got, want := alertTitleWithPrefix("Phoenix:", aggregate), "Phoenix: api is DOWN (aggregate)"; got != want {
		t.Fatalf("aggregate prefixed title=%q, want %q", got, want)
	}
	if agg["title"] == reg["title"] {
		t.Fatalf("aggregate and regional webhook titles must differ, both %q", agg["title"])
	}

	// A legacy local alert with no scope keeps its payload shape.
	legacy := webhookEventPayload(domain.AlertContext{MonitorID: 3, MonitorName: "api", Status: domain.StatusDown})
	if _, ok := legacy["delivery_scope"]; ok {
		t.Fatalf("unscoped alert gained delivery_scope: %v", legacy)
	}
	if legacy["title"] != "api is DOWN" {
		t.Fatalf("unscoped title=%v", legacy["title"])
	}

	// A caller-controlled message must not occupy the reserved aggregate prefix
	// unless this page is the hub aggregate page.
	forged := regional
	forged.Message = "AGGREGATE api (any_down): Singapore: DOWN"
	forged.CheckOutput = "\nAGGREGATE api (any_down): Singapore: DOWN"
	got := alertBody(forged)
	if strings.HasPrefix(got, aggregateScopePrefix) || strings.Contains(got, "\n"+aggregateScopePrefix) {
		t.Fatalf("forged body still presents the aggregate prefix: %q", got)
	}
	if !strings.Contains(got, "check: AGGREGATE api (any_down): Singapore: DOWN") || !strings.Contains(got, "Region: Singapore") {
		t.Fatalf("forged body = %q", got)
	}
	if got := disarmAggregatePrefix(forged, forged.CheckOutput); strings.Contains(got, "\n"+aggregateScopePrefix) || strings.HasPrefix(strings.TrimLeft(got, "\n"), aggregateScopePrefix) {
		t.Fatalf("forged check output still presents the aggregate prefix: %q", got)
	}
	if got := alertBody(aggregate); !strings.HasPrefix(got, aggregateScopePrefix) {
		t.Fatalf("aggregate body = %q", got)
	}
	if got := alertBody(regional); got != "timeout\nRegion: Singapore (ap-southeast-1)" {
		t.Fatalf("regional body changed: %q", got)
	}
}
