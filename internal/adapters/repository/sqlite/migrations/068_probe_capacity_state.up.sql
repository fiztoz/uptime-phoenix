-- Per-probe capacity evidence: exact raw condition samples on the immutable
-- history row and source-evaluated condition state on the current projection.
-- JSON keeps exact measurement values and timestamps independently of the SQL
-- column precision, like tls_json. source_seq on monitor_conditions fences the
-- remote mirror: the highest source sequence wins, so older replay, retired
-- assignment history and stale sessions cannot replace newer promoted state.
ALTER TABLE probe_observations ADD COLUMN conditions_json TEXT NULL CHECK (conditions_json IS NULL OR json_valid(conditions_json));
ALTER TABLE monitor_probe_state ADD COLUMN conditions_json TEXT NULL CHECK (conditions_json IS NULL OR json_valid(conditions_json));
ALTER TABLE monitor_conditions ADD COLUMN source_seq INTEGER NOT NULL DEFAULT 0;
