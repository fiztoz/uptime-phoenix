package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// AlertSourceRepository resolves a regional source identity to its legacy alert.
// Implementations preserve their local or explicitly bound assignment scope.
// The caller must authorize access independently of this storage lookup.
type AlertSourceRepository interface {
	GetBySourceAlertID(ctx context.Context, sourceAlertID string) (*domain.Alert, error)
}
