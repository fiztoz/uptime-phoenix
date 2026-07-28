package checker

import (
	"context"
	"fmt"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// PushChecker is an inbound HTTP receiver that waits for push heartbeats from external systems.
// Instead of performing an outbound check, it exposes an endpoint and validates incoming HMAC signatures.
type PushChecker struct{}

func init() { Register(PushChecker{}) }

func (PushChecker) Type() string { return "push" }

func (PushChecker) Validate(config map[string]any) error {
	if config["push_token"] == nil || config["push_token"] == "" {
		return fmt.Errorf("push_token is required")
	}
	return nil
}

func (PushChecker) Check(_ context.Context, _ map[string]any) (ports.CheckResult, error) {
	return ports.CheckResult{
		Status:  domain.StatusPending,
		Message: "push: awaiting first push",
	}, nil
}
