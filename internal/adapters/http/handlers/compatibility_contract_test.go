package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/ws"
)

func readBaseline(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "probe", "testdata", "v1", "baseline", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBaselineFixturesBindExistingHandlerViews(t *testing.T) {
	var heartbeat heartbeatView
	if err := json.Unmarshal(readBaseline(t, "heartbeat.json"), &heartbeat); err != nil || heartbeat.Message != "OK" || heartbeat.Status != "up" {
		t.Fatalf("HTTP heartbeat: %+v, %v", heartbeat, err)
	}
	var monitor MonitorView
	if err := json.Unmarshal(readBaseline(t, "monitor.json"), &monitor); err != nil || len(monitor.AcceptedStatusCodes) == 0 {
		t.Fatalf("monitor accepted_statuscodes: %+v, %v", monitor, err)
	}
	var alert AlertView
	if err := json.Unmarshal(readBaseline(t, "alert.json"), &alert); err != nil || alert.Message == "" {
		t.Fatalf("alert message: %+v, %v", alert, err)
	}
	var maintenance MaintenanceView
	if err := json.Unmarshal(readBaseline(t, "maintenance.json"), &maintenance); err != nil || len(maintenance.MonitorIDs) != 1 {
		t.Fatalf("maintenance monitor_ids: %+v, %v", maintenance, err)
	}
	var template NotificationTemplateView
	if err := json.Unmarshal(readBaseline(t, "notification-template.json"), &template); err != nil || template.Provider == "" {
		t.Fatalf("template: %+v, %v", template, err)
	}
	var browser ws.HeartbeatView
	if err := json.Unmarshal(readBaseline(t, "browser-heartbeat.json"), &browser); err != nil || browser.Msg != "OK" {
		t.Fatalf("browser msg: %+v, %v", browser, err)
	}
	var access struct {
		AccessCode string `json:"access_code"`
	}
	if err := json.Unmarshal(readBaseline(t, "status-page-access.json"), &access); err != nil || access.AccessCode == "" {
		t.Fatalf("access_code: %+v, %v", access, err)
	}
}
