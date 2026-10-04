package mariadb

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Monitor creation already locks local registration before monitor/assignment
// rows. Updates and FK-cascading deletes need the same order: applied local
// configuration reads hold a SERIALIZABLE shared registration lock before
// traversing the monitor graph. Acquiring it after mutation creates an inversion.
func (r *MonitorRepo) mutateWithLocalSourceLock(ctx context.Context, isolation sql.IsolationLevel, mutate func(context.Context, bun.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := r.db.RunInTx(ctx, &sql.TxOptions{Isolation: isolation}, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewUpdate().Table("probes").Set("revision = revision").Where("id = ?", domain.LocalProbeID).Exec(ctx); err != nil {
				return err
			}
			// An unchanged UPDATE may report zero affected rows. Verify the
			// mutex row exists rather than accepting a missing registration.
			exists, err := tx.NewSelect().Table("probes").Where("id = ?", domain.LocalProbeID).Exists(ctx)
			if err != nil {
				return err
			}
			if !exists {
				return ports.ErrNotFound
			}
			return mutate(ctx, tx)
		})
		// Other concurrent InnoDB paths (for example worker lease claims) do
		// not share this mutex. Retry only a transaction that InnoDB has fully
		// rolled back; no provider, event publication or service side effect
		// occurs in this callback. Timeouts and all other errors remain errors.
		var conflict *mysql.MySQLError
		if attempt >= 3 || ctx.Err() != nil || !errors.As(err, &conflict) || conflict.Number != 1213 {
			return err
		}
	}
}
