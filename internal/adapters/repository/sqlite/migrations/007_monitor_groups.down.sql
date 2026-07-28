-- 007_monitor_groups.down.sql — SQLite: revert monitor groups
-- Created_at: 2026-07-12
-- Updated_at: 2026-07-12
--
-- Drops monitor_groups and restores monitors.parent_id. No data migration —
-- dev data only; group_id assignments are not carried back to parent_id.
-- See 007_monitor_groups.up.sql for why monitors is rebuilt rather than
-- ALTERed in place.

CREATE TABLE monitors_old (
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
    parent_id           INTEGER,
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
    FOREIGN KEY (parent_id) REFERENCES monitors_old(id) ON DELETE SET NULL,
    FOREIGN KEY (proxy_id) REFERENCES proxies(id) ON DELETE SET NULL,
    FOREIGN KEY (docker_host_id) REFERENCES docker_hosts(id) ON DELETE SET NULL
);

INSERT INTO monitors_old (id, user_id, name, description, type, active, check_interval, retry_interval, max_retries, timeout, weight, push_token, proxy_id, tls_ignore, accepted_statuscodes, resend_interval, upside_down, config, docker_host_id, worker_id, leased_at, created_at, updated_at)
    SELECT id, user_id, name, description, type, active, check_interval, retry_interval, max_retries, timeout, weight, push_token, proxy_id, tls_ignore, accepted_statuscodes, resend_interval, upside_down, config, docker_host_id, worker_id, leased_at, created_at, updated_at
    FROM monitors;

DROP TABLE monitors;
ALTER TABLE monitors_old RENAME TO monitors;

CREATE INDEX idx_monitors_active ON monitors(active);
CREATE INDEX idx_monitors_type ON monitors(type);
CREATE INDEX idx_monitors_user ON monitors(user_id);
CREATE INDEX idx_monitors_lease ON monitors(active, worker_id, leased_at);

DROP TABLE IF EXISTS monitor_groups;
