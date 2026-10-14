-- Operative aggregate/both cutover storage (explicit opt-in). Regional default
-- unchanged. Genuine aggregate work is separate from aggregate_standby_*.
-- History-bearing operative rows intentionally carry no REFERENCES to monitors
-- or config snapshots: monitor/config deletion must never erase source gates,
-- provider-attempt authority, or barrier history ("don't erase unresolved
-- authority"). Application evidence therefore also survives snapshot retention.
CREATE TABLE operative_cutover_state (
    monitor_id BIGINT PRIMARY KEY CHECK (monitor_id > 0),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(owner_id) = 36),
    lease_epoch BIGINT NOT NULL CHECK (lease_epoch > 0),
    lease_until DATETIME(6) NOT NULL,
    due_at DATETIME(6) NOT NULL,
    evaluated_at DATETIME(6) NULL,
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (barrier_id IS NULL OR CHAR_LENGTH(barrier_id) = 36),
    phase VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (phase IN ('preparing','quiescing','quiesced','applying','active','draining','released','blocked')),
    alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'regional' CHECK (alert_delivery IN ('regional','aggregate','both')),
    assignment_revision BIGINT NOT NULL DEFAULT 0 CHECK (assignment_revision >= 0),
    transition_version BIGINT NOT NULL DEFAULT 0 CHECK (transition_version >= 0),
    reason VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    CHECK ((barrier_id IS NULL) = (phase = 'preparing'))
) ENGINE=InnoDB;
CREATE TABLE operative_cutover_barriers (
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (CHAR_LENGTH(barrier_id) = 36),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (alert_delivery IN ('aggregate','both')),
    assignment_revision BIGINT NOT NULL CHECK (assignment_revision > 0),
    round_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(round_id) = 36),
    request_round BIGINT NOT NULL CHECK (request_round > 0),
    created_at DATETIME(6) NOT NULL,
    activated_at DATETIME(6) NULL
) ENGINE=InnoDB;
CREATE INDEX operative_cutover_barriers_monitor ON operative_cutover_barriers(monitor_id, created_at);
CREATE TABLE operative_cutover_members (
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(barrier_id) = 36),
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (probe_id = 'local' OR CHAR_LENGTH(probe_id) = 36),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    generation BIGINT NOT NULL CHECK (generation > 0),
    assignment_revision BIGINT NOT NULL CHECK (assignment_revision > 0),
    config_revision BIGINT NOT NULL CHECK (config_revision > 0),
    sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (sha256 NOT REGEXP '[^0-9a-f]'),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (stream_id = '00000000-0000-4000-8000-000000000001' OR CHAR_LENGTH(stream_id) = 36),
    alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (alert_delivery IN ('aggregate','both')),
    preparation_command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(preparation_command_id) = 36),
    quiesce_command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (quiesce_command_id IS NULL OR CHAR_LENGTH(quiesce_command_id) = 36),
    apply_command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (apply_command_id IS NULL OR CHAR_LENGTH(apply_command_id) = 36),
    resume_command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (resume_command_id IS NULL OR CHAR_LENGTH(resume_command_id) = 36),
    quiesced_at DATETIME(6) NULL,
    applied_at DATETIME(6) NULL,
    resumed_at DATETIME(6) NULL,
    PRIMARY KEY (barrier_id, probe_id)
) ENGINE=InnoDB;
CREATE TABLE operative_cutover_commands (
    command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (CHAR_LENGTH(command_id) = 36),
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(barrier_id) = 36),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (probe_id = 'local' OR CHAR_LENGTH(probe_id) = 36),
    kind VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('delivery.quiesce','delivery.apply','delivery.resume')),
    issued_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    outcome_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK (outcome_status IN ('','applied','already_applied','already_resolved','rejected','expired')),
    outcome_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    outcome_at DATETIME(6) NULL,
    UNIQUE (barrier_id, probe_id, kind, command_id),
    CHECK ((outcome_status = '' AND outcome_at IS NULL AND outcome_code = '') OR (outcome_status <> '' AND outcome_at IS NOT NULL AND expires_at > issued_at))
) ENGINE=InnoDB;
CREATE INDEX operative_cutover_commands_open ON operative_cutover_commands(barrier_id, kind);
CREATE TABLE operative_source_gates (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (probe_id = 'local' OR CHAR_LENGTH(probe_id) = 36),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(barrier_id) = 36),
    mode VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (mode IN ('aggregate','both')),
    state VARCHAR(12) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('held','released')),
    granted_at DATETIME(6) NOT NULL,
    released_at DATETIME(6) NULL,
    PRIMARY KEY (probe_id, monitor_id, barrier_id),
    CHECK ((state = 'held' AND released_at IS NULL) OR (state = 'released' AND released_at IS NOT NULL AND released_at >= granted_at))
) ENGINE=InnoDB;
CREATE INDEX operative_source_gates_hold ON operative_source_gates(probe_id, monitor_id, state);
CREATE TABLE operative_local_applications (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (probe_id = 'local'),
    config_revision BIGINT NOT NULL CHECK (config_revision > 0),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(barrier_id) = 36),
    assignment_revision BIGINT NOT NULL CHECK (assignment_revision > 0),
    generation BIGINT NOT NULL CHECK (generation > 0),
    sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (sha256 NOT REGEXP '[^0-9a-f]'),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (stream_id = '00000000-0000-4000-8000-000000000001'),
    alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (alert_delivery IN ('aggregate','both')),
    applied_at DATETIME(6) NOT NULL,
    PRIMARY KEY (probe_id, config_revision, monitor_id, barrier_id)
) ENGINE=InnoDB;
CREATE TABLE operative_source_capabilities (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (CHAR_LENGTH(probe_id) = 36 AND probe_id <> 'local'),
    capability VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(capability) BETWEEN 1 AND 128),
    connection_generation BIGINT NOT NULL CHECK (connection_generation > 0),
    attested_at DATETIME(6) NOT NULL
) ENGINE=InnoDB;
CREATE TABLE operative_provider_attempts (
    attempt_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (CHAR_LENGTH(attempt_id) = 36),
    scope VARCHAR(9) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (scope IN ('aggregate','source')),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK (probe_id = '' OR probe_id = 'local' OR CHAR_LENGTH(probe_id) = 36),
    delivery_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(delivery_id) = 36),
    claim_attempt BIGINT NOT NULL CHECK (claim_attempt > 0),
    notification_id BIGINT NOT NULL DEFAULT 0 CHECK (notification_id >= 0),
    settings_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK (settings_sha256 = '' OR settings_sha256 NOT REGEXP '[^0-9a-f]'),
    budget_until DATETIME(6) NOT NULL,
    state VARCHAR(12) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('in_flight','succeeded','ambiguous','not_started')),
    created_at DATETIME(6) NOT NULL,
    finished_at DATETIME(6) NULL,
    UNIQUE (delivery_id, claim_attempt, scope),
    CHECK ((state = 'in_flight' AND finished_at IS NULL) OR (state <> 'in_flight' AND finished_at IS NOT NULL))
) ENGINE=InnoDB;
CREATE INDEX operative_provider_attempts_monitor ON operative_provider_attempts(monitor_id, state);
-- Migration observation of unknown legacy I/O, never a past success or stop proof.
INSERT INTO operative_provider_attempts (attempt_id, scope, monitor_id, probe_id,
 delivery_id, claim_attempt, notification_id, budget_until, state, created_at, finished_at)
SELECT delivery_id, 'source', monitor_id, probe_id, delivery_id, attempt, notification_id,
 UTC_TIMESTAMP(6), 'ambiguous', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)
FROM probe_delivery_intents
WHERE probe_id = 'local' AND event_kind IN ('status_change','incident_summary')
 AND attempt > 0 AND status IN ('leased','retrying','failed');
CREATE TABLE operative_aggregate_incidents (
    incident_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (CHAR_LENGTH(incident_id) = 36),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(barrier_id) = 36),
    assignment_revision BIGINT NOT NULL CHECK (assignment_revision > 0),
    health_policy VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (health_policy IN ('any_down','all_down')),
    alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (alert_delivery IN ('aggregate','both')),
    transition_version BIGINT NOT NULL CHECK (transition_version > 0),
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (status IN ('open','resolved','admin_closed')),
    opened_at DATETIME(6) NOT NULL,
    closed_at DATETIME(6) NULL,
    close_reason VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK (close_reason IN ('','recovered','drained','mode_changed','administrative')),
    CHECK ((status = 'open' AND closed_at IS NULL AND close_reason = '') OR (status <> 'open' AND closed_at IS NOT NULL AND closed_at >= opened_at AND close_reason <> ''))
) ENGINE=InnoDB;
CREATE INDEX operative_aggregate_incidents_monitor ON operative_aggregate_incidents(monitor_id, transition_version);
CREATE TABLE operative_aggregate_intents (
    intent_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (CHAR_LENGTH(intent_id) = 36),
    incident_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL REFERENCES operative_aggregate_incidents(incident_id) ON DELETE CASCADE,
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(barrier_id) = 36),
    transition_version BIGINT NOT NULL CHECK (transition_version > 0),
    notification_id BIGINT NOT NULL CHECK (notification_id > 0),
    settings_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (settings_sha256 NOT REGEXP '[^0-9a-f]'),
    include_target TINYINT(1) NOT NULL CHECK (include_target IN (0, 1)),
    notification_snapshot TEXT NOT NULL,
    template_snapshot TEXT NULL,
    kind VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('down','repeat','recovery')),
    status VARCHAR(12) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (status IN ('pending','sent','superseded','failed')),
    created_at DATETIME(6) NOT NULL,
    finalized_at DATETIME(6) NULL,
    reason VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK (reason IN ('','superseded','drained','channel_changed','administrative')),
    UNIQUE (incident_id, transition_version, notification_id, settings_sha256),
    CHECK ((status = 'pending' AND finalized_at IS NULL AND reason = '') OR (status <> 'pending' AND finalized_at IS NOT NULL AND finalized_at >= created_at))
) ENGINE=InnoDB;
CREATE TABLE operative_aggregate_deliveries (
    delivery_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (CHAR_LENGTH(delivery_id) = 36),
    intent_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE CHECK (CHAR_LENGTH(intent_id) = 36),
    incident_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(incident_id) = 36),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    barrier_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(barrier_id) = 36),
    primary_probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (primary_probe_id = 'local' OR CHAR_LENGTH(primary_probe_id) = 36),
    notification_id BIGINT NOT NULL CHECK (notification_id > 0),
    transition_version BIGINT NOT NULL CHECK (transition_version > 0),
    event_kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (event_kind IN ('status_change')),
    available_at DATETIME(6) NOT NULL,
    status VARCHAR(12) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (status IN ('pending','leased','sent','retrying','failed','superseded')),
    attempt BIGINT NOT NULL DEFAULT 0 CHECK (attempt >= 0 AND attempt < 9223372036854775807),
    lease_owner VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK (lease_owner = '' OR CHAR_LENGTH(lease_owner) = 36),
    lease_epoch BIGINT NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_token VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' CHECK (CHAR_LENGTH(lease_token) <= 64),
    leased_at DATETIME(6) NULL,
    lease_until DATETIME(6) NULL,
    error_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    outcome_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    CHECK ((status = 'leased' AND lease_token <> '' AND leased_at IS NOT NULL AND lease_until IS NOT NULL AND lease_until > leased_at) OR (status <> 'leased' AND lease_until IS NULL))
) ENGINE=InnoDB;
CREATE INDEX operative_aggregate_deliveries_due ON operative_aggregate_deliveries(monitor_id, status, available_at);
