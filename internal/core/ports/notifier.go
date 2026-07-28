package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// NotificationSender defines the interface that every notification provider must implement.
type NotificationSender interface {
	Type() string
	Validate(config map[string]any) error
	Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error
}
