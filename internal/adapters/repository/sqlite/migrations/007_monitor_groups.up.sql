-- 007_monitor_groups.up.sql — SQLite: first-class monitor groups (folders)
-- Created_at: 2026-07-12
-- Updated_at: 2026-07-12
--
-- Adds monitor_groups and replaces the old monitors.parent_id (a monitor
-- nested under another monitor) with monitors.group_id (a monitor filed
-- under a monitor_group). No data migration: this is dev data only, so
-- parent_id values are dropped rather than carried over to group_id.
--
-- The condition column is named status_condition, not `condition`:
-- CONDITION is a reserved word in MariaDB (used by DECLARE ... CONDITION
-- FOR). Using the same column name in both engines keeps the Bun model and
-- every hand-written query identical across adapters.
--
-- monitors is rebuilt rather than ALTERed in place: SQLite refuses to
-- DROP COLUMN parent_id while it is still referenced by the self FOREIGN
-- KEY (parent_id) REFERENCES monitors(id) declared in 001_init.up.sql
-- ("unknown column \"parent_id\" in foreign key definition"). Dropping and
-- recreating the table (as 005/006 already do for other columns) sidesteps
-- that restriction; SQLite automatically repoints every other table's
-- FOREIGN KEY ... REFERENCES monitors(id) at the recreated table once it is
-- renamed back to "monitors", so heartbeats/tags/etc. keep working.

CREATE TABLE monitor_groups (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id               INTEGER NOT NULL,
    name                  TEXT NOT NULL,
    description           TEXT,
    parent_id             INTEGER,
    status_condition      TEXT NOT NULL DEFAULT 'worst_of_children',
    threshold             INTEGER NOT NULL DEFAULT 0,
    threshold_is_percent  INTEGER NOT NULL DEFAULT 0,
    weight                INTEGER NOT NULL DEFAULT 2000,
    collapsed             INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at            TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (parent_id) REFERENCES monitor_groups(id) ON DELETE SET NULL
);

CREATE INDEX idx_monitor_groups_user ON monitor_groups(user_id);
CREATE INDEX idx_monitor_groups_parent ON monitor_groups(parent_id);

CREATE TABLE monitors_new (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id             INTEGER,
    name                TEXT NOT NULL,
    description         TEXT,
    type                TEXT NOT NULL,
    active              INTEGER NOT NULL DEFAULT 1,
    check_interval       INTEGER NOT NULL DEFAULT 60,
    retry_interval      INTEGER NOT NULL DEFAULT 0,
    max_retries         INTEGER NOT NULL DEFAULT 0,
    timeout             REAL NOT NULL DEFAULT 30.0,
    group_id            INTEGER,
    weight              INTEGER NOT NULL DEFAULT 2000,
    push_token          TEXT,
    proxy_id            INTEGER,
    tls_ignore          INTEGER NOT NULL DEFAULT 0,
    accepted_statuscodes TEXT NOT NULL DEFAULT '["200-299"]',
    resend_interval     INTEGER NOT NULL DEFAULT 0,
    upside_down         INTEGER NOT NULL DEFAULT 0,
    config              TEXT NOT NULL,
    docker_host_id      INTEGER,
    worker_id           TEXT DEFAULT NULL,
    leased_at           TIMESTAMP NULL DEFAULT NULL,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
    FOREIGN KEY (group_id) REFERENCES monitor_groups(id) ON DELETE SET NULL,
    FOREIGN KEY (proxy_id) REFERENCES proxies(id) ON DELETE SET NULL,
    FOREIGN KEY (docker_host_id) REFERENCES docker_hosts(id) ON DELETE SET NULL
);

INSERT INTO monitors_new (id, user_id, name, description, type, active, check_interval, retry_interval, max_retries, timeout, weight, push_token, proxy_id, tls_ignore, accepted_statuscodes, resend_interval, upside_down, config, docker_host_id, worker_id, leased_at, created_at, updated_at)
    SELECT id, user_id, name, description, type, active, check_interval, retry_interval, max_retries, timeout, weight, push_token, proxy_id, tls_ignore, accepted_statuscodes, resend_interval, upside_down, config, docker_host_id, worker_id, leased_at, created_at, updated_at
    FROM monitors;

DROP TABLE monitors;
ALTER TABLE monitors_new RENAME TO monitors;

CREATE INDEX idx_monitors_active ON monitors(active);
CREATE INDEX idx_monitors_type ON monitors(type);
CREATE INDEX idx_monitors_user ON monitors(user_id);
CREATE INDEX idx_monitors_lease ON monitors(active, worker_id, leased_at);
CREATE INDEX idx_monitors_group ON monitors(group_id);
