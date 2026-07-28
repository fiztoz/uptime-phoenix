package checker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestPingChecker_Validate(t *testing.T) {
	checker := PingChecker{}
	if err := checker.Validate(map[string]any{"hostname": "127.0.0.1"}); err != nil {
		t.Fatalf("Validate valid config: %v", err)
	}
	if err := checker.Validate(nil); err == nil {
		t.Fatal("Validate missing hostname returned nil")
	}
}

func TestPingChecker_Check_Up(t *testing.T) {
	result, err := (PingChecker{}).Check(context.Background(), map[string]any{
		"hostname": "127.0.0.1", "count": float64(1), "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp {
		// Unprivileged ICMP is often blocked on CI runners (and some hosts without
		// net.ipv4.ping_group_range). The checker still returns a clear DOWN
		// diagnostic — skip the UP assertion rather than fail the suite.
		msg := strings.ToLower(result.Message)
		if strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "socket:") {
			t.Skipf("ICMP not permitted in this environment: %s", result.Message)
		}
		t.Fatalf("status = %s; want UP (message: %s)", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "received=1") {
		t.Fatalf("message = %q; want packet receipt evidence", result.Message)
	}
}

func TestPingChecker_Check_Down(t *testing.T) {
	result, err := (PingChecker{}).Check(context.Background(), map[string]any{
		"hostname": "host-that-does-not-exist.invalid", "count": float64(1), "timeout": 0.1,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown {
		t.Fatalf("status = %s; want DOWN", result.Status)
	}
	if result.Message == "" {
		t.Fatal("DOWN result has no diagnostic message")
	}
}

func TestPingChecker_Check_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	result, err := (PingChecker{}).Check(ctx, map[string]any{
		"hostname": "203.0.113.1", "count": float64(3), "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown {
		t.Fatalf("status = %s; want DOWN after context deadline", result.Status)
	}
	if !strings.Contains(strings.ToLower(result.Message), "failed") {
		t.Fatalf("message = %q; want timeout/failure diagnostic", result.Message)
	}
}
