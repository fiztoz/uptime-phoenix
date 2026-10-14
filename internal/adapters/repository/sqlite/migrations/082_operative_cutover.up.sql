-- Operative aggregate/both cutover storage (explicit opt-in). Regional default
-- unchanged. Genuine aggregate work is separate from aggregate_standby_*.
-- History-bearing operative rows intentionally carry no REFERENCES to monitors
-- or config snapshots: monitor/config deletion must never erase source gates,
-- provider-attempt authority, or barrier history ("don't erase unresolved
-- authority"). Application evidence therefore also survives snapshot retention.
CREATE TABLE operative_cutover_state (
    monitor_id INTEGER PRIMARY KEY CHECK (monitor_id > 0),
    hub_id TEXT NOT NULL CHECK (length(hub_id) = 36),
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 36),
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch > 0),
    lease_until TIMESTAMP NOT NULL,
    due_at TIMESTAMP NOT NULL,
    evaluated_at TIMESTAMP NULL,
    barrier_id TEXT NULL CHECK (barrier_id IS NULL OR length(barrier_id) = 36),
    phase TEXT NOT NULL CHECK (phase IN ('preparing','quiescing','quiesced','applying','active','draining','released','blocked')),
    alert_delivery TEXT NOT NULL DEFAULT 'regional' CHECK (alert_delivery IN ('regional','aggregate','both')),
    assignment_revision INTEGER NOT NULL DEFAULT 0 CHECK (assignment_revision >= 0),
    transition_version INTEGER NOT NULL DEFAULT 0 CHECK (transition_version >= 0),
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    CHECK ((barrier_id IS NULL) = (phase = 'preparing'))
);
CREATE TABLE operative_cutover_barriers (
    barrier_id TEXT PRIMARY KEY CHECK (length(barrier_id) = 36),
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    hub_id TEXT NOT NULL CHECK (length(hub_id) = 36),
    alert_delivery TEXT NOT NULL CHECK (alert_delivery IN ('aggregate','both')),
    assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
    round_id TEXT NOT NULL CHECK (length(round_id) = 36),
    request_round INTEGER NOT NULL CHECK (request_round > 0),
    created_at TIMESTAMP NOT NULL,
    activated_at TIMESTAMP NULL
);
CREATE INDEX operative_cutover_barriers_monitor ON operative_cutover_barriers(monitor_id, created_at);
CREATE TABLE operative_cutover_members (
    barrier_id TEXT NOT NULL CHECK (length(barrier_id) = 36),
    probe_id TEXT NOT NULL CHECK (probe_id = 'local' OR length(probe_id) = 36),
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    generation INTEGER NOT NULL CHECK (generation > 0),
    assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
    config_revision INTEGER NOT NULL CHECK (config_revision > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    stream_id TEXT NOT NULL CHECK (stream_id = '00000000-0000-4000-8000-000000000001' OR length(stream_id) = 36),
    alert_delivery TEXT NOT NULL CHECK (alert_delivery IN ('aggregate','both')),
    preparation_command_id TEXT NOT NULL CHECK (length(preparation_command_id) = 36),
    quiesce_command_id TEXT NULL CHECK (quiesce_command_id IS NULL OR length(quiesce_command_id) = 36),
    apply_command_id TEXT NULL CHECK (apply_command_id IS NULL OR length(apply_command_id) = 36),
    resume_command_id TEXT NULL CHECK (resume_command_id IS NULL OR length(resume_command_id) = 36),
    quiesced_at TIMESTAMP NULL,
    applied_at TIMESTAMP NULL,
    resumed_at TIMESTAMP NULL,
    PRIMARY KEY (barrier_id, probe_id)
);
CREATE TABLE operative_cutover_commands (
    command_id TEXT PRIMARY KEY CHECK (length(command_id) = 36),
    barrier_id TEXT NOT NULL CHECK (length(barrier_id) = 36),
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    probe_id TEXT NOT NULL CHECK (probe_id = 'local' OR length(probe_id) = 36),
    kind TEXT NOT NULL CHECK (kind IN ('delivery.quiesce','delivery.apply','delivery.resume')),
    issued_at TIMESTAMP NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    outcome_status TEXT NOT NULL DEFAULT '' CHECK (outcome_status IN ('','applied','already_applied','already_resolved','rejected','expired')),
    outcome_code TEXT NOT NULL DEFAULT '',
    outcome_at TIMESTAMP NULL,
    UNIQUE (barrier_id, probe_id, kind, command_id),
    CHECK ((outcome_status = '' AND outcome_at IS NULL AND outcome_code = '') OR (outcome_status <> '' AND outcome_at IS NOT NULL AND expires_at > issued_at))
);
CREATE INDEX operative_cutover_commands_open ON operative_cutover_commands(barrier_id, kind);
CREATE TABLE operative_source_gates (
    probe_id TEXT NOT NULL CHECK (probe_id = 'local' OR length(probe_id) = 36),
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    barrier_id TEXT NOT NULL CHECK (length(barrier_id) = 36),
    mode TEXT NOT NULL CHECK (mode IN ('aggregate','both')),
    state TEXT NOT NULL CHECK (state IN ('held','released')),
    granted_at TIMESTAMP NOT NULL,
    released_at TIMESTAMP NULL,
    PRIMARY KEY (probe_id, monitor_id, barrier_id),
    CHECK ((state = 'held' AND released_at IS NULL) OR (state = 'released' AND released_at IS NOT NULL AND released_at >= granted_at))
);
CREATE INDEX operative_source_gates_hold ON operative_source_gates(probe_id, monitor_id, state);
CREATE TABLE operative_local_applications (
    probe_id TEXT NOT NULL CHECK (probe_id = 'local'),
    config_revision INTEGER NOT NULL CHECK (config_revision > 0),
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    barrier_id TEXT NOT NULL CHECK (length(barrier_id) = 36),
    assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
    generation INTEGER NOT NULL CHECK (generation > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    stream_id TEXT NOT NULL CHECK (stream_id = '00000000-0000-4000-8000-000000000001'),
    alert_delivery TEXT NOT NULL CHECK (alert_delivery IN ('aggregate','both')),
    applied_at TIMESTAMP NOT NULL,
    PRIMARY KEY (probe_id, config_revision, monitor_id, barrier_id)
);
CREATE TABLE operative_source_capabilities (
    probe_id TEXT PRIMARY KEY CHECK (length(probe_id) = 36 AND probe_id <> 'local'),
    capability TEXT NOT NULL CHECK (length(capability) BETWEEN 1 AND 128),
    connection_generation INTEGER NOT NULL CHECK (connection_generation > 0),
    attested_at TIMESTAMP NOT NULL
);
CREATE TABLE operative_provider_attempts (
    attempt_id TEXT PRIMARY KEY CHECK (length(attempt_id) = 36),
    scope TEXT NOT NULL CHECK (scope IN ('aggregate','source')),
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    probe_id TEXT NOT NULL DEFAULT '' CHECK (probe_id = '' OR probe_id = 'local' OR length(probe_id) = 36),
    delivery_id TEXT NOT NULL CHECK (length(delivery_id) = 36),
    claim_attempt INTEGER NOT NULL CHECK (claim_attempt > 0),
    notification_id INTEGER NOT NULL DEFAULT 0 CHECK (notification_id >= 0),
    settings_sha256 TEXT NOT NULL DEFAULT '' CHECK (settings_sha256 = '' OR (length(settings_sha256) = 64 AND settings_sha256 NOT GLOB '*[^0-9a-f]*')),
    budget_until TIMESTAMP NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('in_flight','succeeded','ambiguous','not_started')),
    created_at TIMESTAMP NOT NULL,
    finished_at TIMESTAMP NULL,
    UNIQUE (delivery_id, claim_attempt, scope),
    CHECK ((state = 'in_flight' AND finished_at IS NULL) OR (state <> 'in_flight' AND finished_at IS NOT NULL))
);
CREATE INDEX operative_provider_attempts_monitor ON operative_provider_attempts(monitor_id, state);
-- Capture upgrade ambiguity, not a fabricated past provider outcome. These
-- timestamps mark migration observation, not the legacy request's I/O lifetime.
INSERT INTO operative_provider_attempts (attempt_id, scope, monitor_id, probe_id,
 delivery_id, claim_attempt, notification_id, budget_until, state, created_at, finished_at)
SELECT delivery_id, 'source', monitor_id, probe_id, delivery_id, attempt, notification_id,
 strftime('%Y-%m-%d %H:%M:%f', 'now'), 'ambiguous',
 strftime('%Y-%m-%d %H:%M:%f', 'now'), strftime('%Y-%m-%d %H:%M:%f', 'now')
FROM probe_delivery_intents
WHERE probe_id = 'local' AND event_kind IN ('status_change','incident_summary')
 AND attempt > 0 AND status IN ('leased','retrying','failed');
CREATE TABLE operative_aggregate_incidents (
    incident_id TEXT PRIMARY KEY CHECK (length(incident_id) = 36),
    hub_id TEXT NOT NULL CHECK (length(hub_id) = 36),
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    barrier_id TEXT NOT NULL CHECK (length(barrier_id) = 36),
    assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
    health_policy TEXT NOT NULL CHECK (health_policy IN ('any_down','all_down')),
    alert_delivery TEXT NOT NULL CHECK (alert_delivery IN ('aggregate','both')),
    transition_version INTEGER NOT NULL CHECK (transition_version > 0),
    status TEXT NOT NULL CHECK (status IN ('open','resolved','admin_closed')),
    opened_at TIMESTAMP NOT NULL,
    closed_at TIMESTAMP NULL,
    close_reason TEXT NOT NULL DEFAULT '' CHECK (close_reason IN ('','recovered','drained','mode_changed','administrative')),
    CHECK ((status = 'open' AND closed_at IS NULL AND close_reason = '') OR (status <> 'open' AND closed_at IS NOT NULL AND closed_at >= opened_at AND close_reason <> ''))
);
CREATE INDEX operative_aggregate_incidents_monitor ON operative_aggregate_incidents(monitor_id, transition_version);
CREATE TABLE operative_aggregate_intents (
    intent_id TEXT PRIMARY KEY CHECK (length(intent_id) = 36),
    incident_id TEXT NOT NULL REFERENCES operative_aggregate_incidents(incident_id) ON DELETE CASCADE,
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    barrier_id TEXT NOT NULL CHECK (length(barrier_id) = 36),
    transition_version INTEGER NOT NULL CHECK (transition_version > 0),
    notification_id INTEGER NOT NULL CHECK (notification_id > 0),
    settings_sha256 TEXT NOT NULL CHECK (length(settings_sha256) = 64 AND settings_sha256 NOT GLOB '*[^0-9a-f]*'),
    include_target INTEGER NOT NULL CHECK (include_target IN (0, 1)),
    notification_snapshot TEXT NOT NULL,
    template_snapshot TEXT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('down','repeat','recovery')),
    status TEXT NOT NULL CHECK (status IN ('pending','sent','superseded','failed')),
    created_at TIMESTAMP NOT NULL,
    finalized_at TIMESTAMP NULL,
    reason TEXT NOT NULL DEFAULT '' CHECK (reason IN ('','superseded','drained','channel_changed','administrative')),
    UNIQUE (incident_id, transition_version, notification_id, settings_sha256),
    CHECK ((status = 'pending' AND finalized_at IS NULL AND reason = '') OR (status <> 'pending' AND finalized_at IS NOT NULL AND finalized_at >= created_at))
);
CREATE TABLE operative_aggregate_deliveries (
    delivery_id TEXT PRIMARY KEY CHECK (length(delivery_id) = 36),
    intent_id TEXT NOT NULL UNIQUE CHECK (length(intent_id) = 36),
    incident_id TEXT NOT NULL CHECK (length(incident_id) = 36),
    monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
    barrier_id TEXT NOT NULL CHECK (length(barrier_id) = 36),
    primary_probe_id TEXT NOT NULL CHECK (primary_probe_id = 'local' OR length(primary_probe_id) = 36),
    notification_id INTEGER NOT NULL CHECK (notification_id > 0),
    transition_version INTEGER NOT NULL CHECK (transition_version > 0),
    event_kind TEXT NOT NULL CHECK (event_kind IN ('status_change')),
    available_at TIMESTAMP NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','leased','sent','retrying','failed','superseded')),
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    lease_owner TEXT NOT NULL DEFAULT '' CHECK (lease_owner = '' OR length(lease_owner) = 36),
    lease_epoch INTEGER NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_token TEXT NOT NULL DEFAULT '' CHECK (length(lease_token) <= 64),
    leased_at TIMESTAMP NULL,
    lease_until TIMESTAMP NULL,
    error_code TEXT NOT NULL DEFAULT '',
    outcome_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL,
    CHECK ((status = 'leased' AND lease_token <> '' AND leased_at IS NOT NULL AND lease_until IS NOT NULL AND lease_until > leased_at) OR (status <> 'leased' AND lease_until IS NULL))
);
CREATE INDEX operative_aggregate_deliveries_due ON operative_aggregate_deliveries(monitor_id, status, available_at);
