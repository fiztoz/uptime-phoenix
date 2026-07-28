package checker

import (
	"context"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestPushChecker_Validate(t *testing.T) {
	checker := PushChecker{}
	if err := checker.Validate(map[string]any{"push_token": "token"}); err != nil {
		t.Fatalf("Validate valid config: %v", err)
	}
	if err := checker.Validate(nil); err == nil {
		t.Fatal("Validate missing token returned nil")
	}
}

// Push monitors are passive: real UP/DOWN heartbeats enter through the push
// HTTP handler. Scheduling the checker must therefore remain PENDING rather
// than manufacturing a healthy or failed observation.
func TestPushChecker_Check_RemainsPendingUntilInboundHeartbeat(t *testing.T) {
	result, err := (PushChecker{}).Check(context.Background(), map[string]any{"push_token": "token"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusPending {
		t.Fatalf("status = %s; want PENDING for passive checker", result.Status)
	}
	if !strings.Contains(result.Message, "awaiting first push") {
		t.Fatalf("message = %q; want passive-state explanation", result.Message)
	}
}
