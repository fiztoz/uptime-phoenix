ALTER TABLE monitor_probe_state ADD COLUMN ping INTEGER NOT NULL DEFAULT 0;
ALTER TABLE monitor_probe_state ADD COLUMN message TEXT NOT NULL DEFAULT '';
-- Existing current rows must retain the exact observation fields when a
-- same-sequence snapshot arrives immediately after upgrade.
UPDATE monitor_probe_state SET
 ping = COALESCE((SELECT o.ping FROM probe_observations AS o
  WHERE o.stream_id = monitor_probe_state.stream_id AND o.seq = monitor_probe_state.seq
   AND o.monitor_id = monitor_probe_state.monitor_id AND o.probe_id = monitor_probe_state.probe_id
   AND o.assignment_generation = monitor_probe_state.assignment_generation), 0),
 message = COALESCE((SELECT o.message FROM probe_observations AS o
  WHERE o.stream_id = monitor_probe_state.stream_id AND o.seq = monitor_probe_state.seq
   AND o.monitor_id = monitor_probe_state.monitor_id AND o.probe_id = monitor_probe_state.probe_id
   AND o.assignment_generation = monitor_probe_state.assignment_generation), '');
ALTER TABLE monitor_probe_state ADD COLUMN active_source_alert_id TEXT;
CREATE TABLE probe_state_receipts (
 probe_id TEXT NOT NULL,
 stream_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 sha256 VARCHAR(64) NOT NULL,
 config_revision INTEGER NOT NULL,
 last_created_seq INTEGER NOT NULL CHECK (last_created_seq >= 0),
 created_at TIMESTAMP NOT NULL,
 applied_at TIMESTAMP NOT NULL,
 state_count INTEGER NOT NULL,
 PRIMARY KEY (probe_id, stream_id),
 FOREIGN KEY (probe_id, stream_id) REFERENCES probe_streams(probe_id, stream_id) ON DELETE RESTRICT
);
CREATE TABLE probe_missing_state (
 monitor_id INTEGER NOT NULL,
 probe_id TEXT NOT NULL,
 stream_id TEXT NOT NULL,
 assignment_generation INTEGER NOT NULL,
 config_revision INTEGER NOT NULL,
 snapshot_seq INTEGER NOT NULL CHECK (snapshot_seq >= 0),
 created_at TIMESTAMP NOT NULL,
 applied_at TIMESTAMP NOT NULL,
 PRIMARY KEY (monitor_id, probe_id),
 FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
 FOREIGN KEY (probe_id, stream_id) REFERENCES probe_streams(probe_id, stream_id) ON DELETE RESTRICT
);
