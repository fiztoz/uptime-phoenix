-- Standby planning ONLY. No table permits pending/leased/sent provider work.
CREATE TABLE aggregate_standby_state (
 monitor_id INTEGER PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
 hub_id TEXT NOT NULL CHECK(length(hub_id)=36),
 owner_id TEXT NOT NULL CHECK(length(owner_id)=36),
 lease_epoch INTEGER NOT NULL CHECK(lease_epoch>0),
 lease_until TIMESTAMP NOT NULL,
 due_at TIMESTAMP NOT NULL,
 evaluated_at TIMESTAMP NULL,
 transition_version INTEGER NOT NULL DEFAULT 0 CHECK(transition_version>=0),
 next_repeat_at TIMESTAMP NULL,
 last_planned_at TIMESTAMP NULL,
 assignment_revision INTEGER NOT NULL DEFAULT 0 CHECK(assignment_revision>=0),
 policy_effective_at TIMESTAMP NULL,
 CHECK((assignment_revision=0 AND policy_effective_at IS NULL) OR (assignment_revision>0 AND policy_effective_at IS NOT NULL))
);
CREATE TABLE aggregate_standby_incidents (
 incident_id TEXT PRIMARY KEY CHECK(length(incident_id)=36),
 hub_id TEXT NOT NULL CHECK(length(hub_id)=36),
 monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
 open_monitor_id INTEGER NULL UNIQUE,
 assignment_revision INTEGER NOT NULL CHECK(assignment_revision>0),
 health_policy TEXT NOT NULL CHECK(health_policy IN ('any_down','all_down')),
 alert_delivery TEXT NOT NULL CHECK(alert_delivery IN ('aggregate','both')),
 transition_version INTEGER NOT NULL CHECK(transition_version>0),
 status TEXT NOT NULL CHECK(status IN ('open','resolved','admin_closed')),
 opened_at TIMESTAMP NOT NULL,
 closed_at TIMESTAMP NULL,
 close_reason TEXT NOT NULL DEFAULT '' CHECK(close_reason IN ('','recovered','delivery_mode_changed','health_policy_changed','assignment_revision_changed')),
 CHECK((status='open' AND open_monitor_id=monitor_id AND open_monitor_id IS NOT NULL AND closed_at IS NULL AND close_reason='') OR
       (status<>'open' AND open_monitor_id IS NULL AND closed_at IS NOT NULL AND closed_at>=opened_at AND close_reason<>'')),
 CHECK((status='resolved' AND close_reason='recovered') OR (status='admin_closed' AND close_reason<>'recovered') OR status='open')
);
CREATE INDEX aggregate_standby_incidents_monitor ON aggregate_standby_incidents(monitor_id,transition_version);
CREATE TABLE aggregate_standby_intents (
 intent_id TEXT PRIMARY KEY CHECK(length(intent_id)=36),
 incident_id TEXT NOT NULL REFERENCES aggregate_standby_incidents(incident_id) ON DELETE CASCADE,
 transition_version INTEGER NOT NULL CHECK(transition_version>0),
 notification_id INTEGER NOT NULL CHECK(notification_id>0),
 channel_sha256 TEXT NOT NULL CHECK(length(channel_sha256)=64 AND channel_sha256 NOT GLOB '*[^0-9a-f]*'),
 kind TEXT NOT NULL CHECK(kind IN ('down','repeat','recovery')),
 status TEXT NOT NULL CHECK(status IN ('held','invalidated')),
 created_at TIMESTAMP NOT NULL,
 invalidated_at TIMESTAMP NULL,
 reason TEXT NOT NULL DEFAULT '' CHECK(reason IN ('','superseded','channel_changed','administrative')),
 UNIQUE(incident_id,transition_version,notification_id,channel_sha256),
 CHECK((status='held' AND invalidated_at IS NULL AND reason='') OR
       (status='invalidated' AND invalidated_at IS NOT NULL AND invalidated_at>=created_at AND reason<>''))
);
