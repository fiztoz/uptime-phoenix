-- Standby planning ONLY. No table permits pending/leased/sent provider work.
CREATE TABLE aggregate_standby_state (
 monitor_id BIGINT PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
 hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(length(hub_id)=36),
 owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(length(owner_id)=36),
 lease_epoch BIGINT NOT NULL CHECK(lease_epoch>0),
 lease_until DATETIME(6) NOT NULL,
 due_at DATETIME(6) NOT NULL,
 evaluated_at DATETIME(6) NULL,
 transition_version BIGINT NOT NULL DEFAULT 0 CHECK(transition_version>=0),
 next_repeat_at DATETIME(6) NULL,
 last_planned_at DATETIME(6) NULL,
 assignment_revision BIGINT NOT NULL DEFAULT 0 CHECK(assignment_revision>=0),
 policy_effective_at DATETIME(6) NULL,
 CHECK((assignment_revision=0 AND policy_effective_at IS NULL) OR (assignment_revision>0 AND policy_effective_at IS NOT NULL))
) ENGINE=InnoDB;
CREATE TABLE aggregate_standby_incidents (
 incident_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK(length(incident_id)=36),
 hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(length(hub_id)=36),
 monitor_id BIGINT NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
 open_monitor_id BIGINT NULL UNIQUE,
 assignment_revision BIGINT NOT NULL CHECK(assignment_revision>0),
 health_policy VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(health_policy IN ('any_down','all_down')),
 alert_delivery VARCHAR(9) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(alert_delivery IN ('aggregate','both')),
 transition_version BIGINT NOT NULL CHECK(transition_version>0),
 status VARCHAR(12) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(status IN ('open','resolved','admin_closed')),
 opened_at DATETIME(6) NOT NULL,
 closed_at DATETIME(6) NULL,
 close_reason VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK(close_reason IN ('','recovered','delivery_mode_changed','health_policy_changed','assignment_revision_changed')),
 CHECK((status='open' AND open_monitor_id=monitor_id AND open_monitor_id IS NOT NULL AND closed_at IS NULL AND close_reason='') OR
       (status<>'open' AND open_monitor_id IS NULL AND closed_at IS NOT NULL AND closed_at>=opened_at AND close_reason<>'')),
 CHECK((status='resolved' AND close_reason='recovered') OR (status='admin_closed' AND close_reason<>'recovered') OR status='open')
) ENGINE=InnoDB;
CREATE INDEX aggregate_standby_incidents_monitor ON aggregate_standby_incidents(monitor_id,transition_version);
CREATE TABLE aggregate_standby_intents (
 intent_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK(length(intent_id)=36),
 incident_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL REFERENCES aggregate_standby_incidents(incident_id) ON DELETE CASCADE,
 transition_version BIGINT NOT NULL CHECK(transition_version>0),
 notification_id BIGINT NOT NULL CHECK(notification_id>0),
 channel_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(length(channel_sha256)=64 AND channel_sha256 NOT REGEXP '[^0-9a-f]'),
 kind VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(kind IN ('down','repeat','recovery')),
 status VARCHAR(12) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(status IN ('held','invalidated')),
 created_at DATETIME(6) NOT NULL,
 invalidated_at DATETIME(6) NULL,
 reason VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK(reason IN ('','superseded','channel_changed','administrative')),
 UNIQUE(incident_id,transition_version,notification_id,channel_sha256),
 CHECK((status='held' AND invalidated_at IS NULL AND reason='') OR
       (status='invalidated' AND invalidated_at IS NOT NULL AND invalidated_at>=created_at AND reason<>''))
) ENGINE=InnoDB;
