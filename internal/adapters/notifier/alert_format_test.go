package notifier

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestCapacityConditionAlertFormattingAndWebhookShape(t *testing.T) {
	used, limit, percent, threshold := 92.0, 100.0, 92.0, 80.0
	observedAt := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	alert := domain.AlertContext{
		MonitorID: 7, MonitorName: "primary-db", MonitorType: "database", MonitorTarget: "postgres://db/app",
		Status: domain.StatusUp, PreviousStatus: domain.StatusUp,
		EventKind: domain.AlertEventCapacityCondition, Message: "session pool 92/100 exceeds threshold 80%",
		ConditionKind: domain.MonitorConditionSessionPool, ConditionState: domain.ConditionStateWarning,
		ConditionPreviousState: domain.ConditionStateOK, ConditionUsed: &used, ConditionLimit: &limit,
		ConditionPercent: &percent, ConditionThreshold: &threshold, ConditionUnit: "connections",
		ConditionResource: "Session pool", ConditionScope: "cluster", ConditionSource: "pg_stat_database",
		ConditionObservedAt: &observedAt,
	}

	if got := alertTitle(alert); got != "Capacity warning: primary-db — Session pool" {
		t.Fatalf("title=%q", got)
	}
	if got := alertBody(alert); got != alert.Message {
		t.Fatalf("body=%q want=%q", got, alert.Message)
	}
	if got := alertEmoji(alert); got != "⚠️" {
		t.Fatalf("emoji=%q", got)
	}

	payload := webhookEventPayload(alert)
	if payload["event_kind"] != domain.AlertEventCapacityCondition || payload["severity"] != domain.ConditionStateWarning {
		t.Fatalf("payload event/severity=%v/%v", payload["event_kind"], payload["severity"])
	}
	if _, leaked := payload["status"]; leaked {
		t.Fatalf("capacity webhook must not masquerade as an availability status: %v", payload)
	}
	condition, ok := payload["condition"].(map[string]any)
	if !ok || condition["kind"] != domain.MonitorConditionSessionPool || condition["percent"] != &percent {
		t.Fatalf("condition payload=%v", payload["condition"])
	}
}

func TestCapacityConditionRecoveryFormatting(t *testing.T) {
	alert := domain.AlertContext{
		MonitorName: "primary-db", EventKind: domain.AlertEventCapacityCondition,
		ConditionKind: domain.MonitorConditionStorage, ConditionResource: "Database size",
		ConditionState: domain.ConditionStateOK, ConditionPreviousState: domain.ConditionStateWarning,
	}
	if got := alertTitle(alert); got != "Capacity recovered: primary-db — Database size" {
		t.Fatalf("title=%q", got)
	}
	if got := alertEmoji(alert); got != "✅" {
		t.Fatalf("emoji=%q", got)
	}
}
