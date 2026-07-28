package scheduler

import "context"

// heartbeatRecordContext returns a detached context for DB/event writes after a check.
// Recording must never inherit check timeouts or scheduler cancellation; no deadline
// is applied so SQLite lock retries can complete under concurrent monitor load.
func heartbeatRecordContext() context.Context {
	return context.Background()
}
