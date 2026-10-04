package mariadb

import (
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
)

// AlertRepo implements assignment-scoped alert persistence on MariaDB.
type AlertRepo struct{ *repository.AlertStore }

// NewAlertRepo creates the local compatibility repository.
func NewAlertRepo(db *bun.DB) *AlertRepo {
	return &AlertRepo{AlertStore: repository.NewAlertStore(db, translateError)}
}
