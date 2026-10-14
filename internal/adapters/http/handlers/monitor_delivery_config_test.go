package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type deliveryConfigHTTPReader struct {
	*regionalReadFake
	evidence map[string]services.DeliveryConfigEvidence
	err      error
	reads    int
	userID   int64
	monitor  int64
	revision int64
}

func (r *deliveryConfigHTTPReader) AcknowledgedDeliveryConfigs(_ context.Context, userID, monitorID, revision int64, _ time.Time) (map[string]services.DeliveryConfigEvidence, error) {
	r.reads++
	r.userID, r.monitor, r.revision = userID, monitorID, revision
	return r.evidence, r.err
}

func deliveryConfigHTTPFixture() *deliveryConfigHTTPReader {
	r := &deliveryConfigHTTPReader{regionalReadFake: regionalFixture(), evidence: make(map[string]services.DeliveryConfigEvidence)}
	r.assignments.Set.AlertDelivery = domain.AlertDeliveryAggregate
	for _, a := range r.assignments.Set.Assignments {
		if a.ProbeID == domain.LocalProbeID {
			r.evidence[a.ProbeID] = services.DeliveryConfigEvidence{State: services.DeliveryConfigLocalUnavailable}
		} else {
			r.evidence[a.ProbeID] = services.DeliveryConfigEvidence{State: services.DeliveryConfigAcknowledged, ConfigRevision: 9007199254740993, Generation: a.Generation, AlertDelivery: domain.AlertDeliveryAggregate}
		}
	}
	return r
}

func TestDeliveryConfigHTTPOptInAndWire(t *testing.T) {
	r := deliveryConfigHTTPFixture()
	h := NewMonitorRegionalHandlers(r, true)
	base := regionalRequest(h, "/api/monitors/7/probes", "7", 1)
	for _, query := range []string{"include_delivery_config=false", "unknown_parameter=true", "a=%ZZ", "x=1;y=2", "a=%ZZ&include_delivery_config=false"} {
		rec := regionalRequest(h, "/api/monitors/7/probes?"+query, "7", 1)
		if rec.Code != 200 || rec.Body.String() != base.Body.String() {
			t.Fatal("default/false GET shape changed")
		}
	}
	if r.reads != 0 || strings.Contains(base.Body.String(), "delivery_config_evidence") {
		t.Fatal("default GET performed an expensive configuration read")
	}
	rec := regionalRequest(h, "/api/monitors/7/probes?include_delivery_config=true", "7", 1)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || r.reads != 1 || r.userID != 1 || r.monitor != 7 || r.revision != 9007199254740993 {
		t.Fatal("opt-in read did not carry scoped identity/revision")
	}
	var view MonitorProbeAssignmentsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.AlertDeliveryPending || view.AlertDelivery != "aggregate" {
		t.Fatal("configuration acknowledgement became runtime activation")
	}
	for _, row := range view.Assignments {
		e := row.DeliveryConfigEvidence
		if e == nil {
			t.Fatal("requested evidence is missing")
		}
		if row.ProbeID == domain.LocalProbeID {
			if e.State != services.DeliveryConfigLocalUnavailable || e.ConfigRevision != nil || e.Generation != nil || e.AlertDelivery != nil {
				t.Fatal("local evidence was invented")
			}
		} else if e.State != services.DeliveryConfigAcknowledged || e.ConfigRevision == nil || *e.ConfigRevision != "9007199254740993" || e.Generation == nil || *e.Generation != "9007199254740993" || e.AlertDelivery == nil || *e.AlertDelivery != "aggregate" {
			t.Fatal("exact document facts or decimal strings lost")
		}
	}
}

func TestDeliveryConfigHTTPStrictQuery(t *testing.T) {
	for _, query := range []string{"include_delivery_config", "include_delivery_config=", "include_delivery_config=1", "include_delivery_config=True", "include_delivery_config=false&include_delivery_config=true", "include_delivery_config=true&include_delivery_config=true", "include_delivery_config=true&%69nclude_delivery_config=false", "include_delivery_config=%ZZ"} {
		t.Run(query, func(t *testing.T) {
			r := deliveryConfigHTTPFixture()
			rec := regionalRequest(NewMonitorRegionalHandlers(r, true), "/api/monitors/7/probes?"+query, "7", 1)
			if rec.Code != 400 || r.reads != 0 || r.calls != 0 {
				t.Fatal("invalid query performed a scoped/storage read")
			}
		})
	}
}

func TestDeliveryConfigHTTPReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"revision changed", services.ErrAssignmentReadChanged, 409},
		{"scope revoked", ports.ErrNotFound, 404},
		{"unavailable", errors.New("private-decoder-details"), 503},
		{"canceled", context.Canceled, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := deliveryConfigHTTPFixture()
			r.err = tc.err
			rec := regionalRequest(NewMonitorRegionalHandlers(r, true), "/api/monitors/7/probes?include_delivery_config=true", "7", 1)
			if rec.Code != tc.code || strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "config_revision") {
				t.Fatal("failed read leaked details or a partial configuration view")
			}
		})
	}
	r := deliveryConfigHTTPFixture()
	r.regionalReadFake.err = ports.ErrNotFound
	if rec := regionalRequest(NewMonitorRegionalHandlers(r, true), "/api/monitors/7/probes?include_delivery_config=true", "7", 1); rec.Code != 404 || r.reads != 0 {
		t.Fatal("hidden monitor performed a document read")
	}
	if rec := regionalRequest(NewMonitorRegionalHandlers(regionalFixture(), true), "/api/monitors/7/probes?include_delivery_config=true", "7", 1); rec.Code != 503 {
		t.Fatal("unsupported reader returned success")
	}
	r = deliveryConfigHTTPFixture()
	r.evidence = nil
	if rec := regionalRequest(NewMonitorRegionalHandlers(r, true), "/api/monitors/7/probes?include_delivery_config=true", "7", 1); rec.Code != 503 {
		t.Fatal("incomplete projection returned success")
	}
}
