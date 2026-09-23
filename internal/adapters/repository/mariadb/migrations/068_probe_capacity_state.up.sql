-- Per-probe capacity evidence: exact raw condition samples on the immutable
-- history row and source-evaluated condition state on the current projection.
-- JSON keeps exact measurement values and timestamps independently of the SQL
-- column precision, like tls_json. source_seq on monitor_conditions fences the
-- remote mirror: the highest source sequence wins, so older replay, retired
-- assignment history and stale sessions cannot replace newer promoted state.
--
-- IF NOT EXISTS / IF EXISTS is deliberate. The shared MariaDB test schema is
-- walked down and back up by the registry migration rehearsal, and a plain ADD
-- would fail with a duplicate-column error whenever the column is already
-- present, aborting that restore and stranding every later reader on an older
-- schema shape.
ALTER TABLE probe_observations ADD COLUMN IF NOT EXISTS conditions_json JSON NULL;
ALTER TABLE monitor_probe_state ADD COLUMN IF NOT EXISTS conditions_json JSON NULL;
ALTER TABLE monitor_conditions ADD COLUMN IF NOT EXISTS source_seq BIGINT NOT NULL DEFAULT 0;
