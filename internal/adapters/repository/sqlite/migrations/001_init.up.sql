-- 001_init.up.sql — SQLite schema for Phoenix
-- SQLite-compatible variant: TEXT for JSON, INTEGER PRIMARY KEY AUTOINCREMENT, etc.

-- users
CREATE TABLE users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    active          INTEGER NOT NULL DEFAULT 1,
    timezone        TEXT DEFAULT 'UTC',
    totp_secret     BLOB,
    totp_enabled    INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- proxies (must be created before monitors due to FK)
CREATE TABLE proxies (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL,
    protocol    TEXT NOT NULL,
    host        TEXT NOT NULL,
    port        INTEGER NOT NULL,
    auth        INTEGER NOT NULL DEFAULT 0,
    username    TEXT,
    password    TEXT,
    active      INTEGER NOT NULL DEFAULT 1,
    is_default  INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- docker_hosts (must be created before monitors due to FK)
CREATE TABLE docker_hosts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL,
    name            TEXT NOT NULL,
    docker_daemon   TEXT NOT NULL,
    docker_type     TEXT NOT NULL DEFAULT 'socket',
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- monitors (per-type config stored as TEXT/JSON)
CREATE TABLE monitors (
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
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
    FOREIGN KEY (parent_id) REFERENCES monitors(id) ON DELETE SET NULL,
    FOREIGN KEY (proxy_id) REFERENCES proxies(id) ON DELETE SET NULL,
    FOREIGN KEY (docker_host_id) REFERENCES docker_hosts(id) ON DELETE SET NULL
);

CREATE INDEX idx_monitors_active ON monitors(active);
CREATE INDEX idx_monitors_type ON monitors(type);
CREATE INDEX idx_monitors_user ON monitors(user_id);

-- heartbeats (no partitioning in SQLite — single table)
CREATE TABLE heartbeats (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id  INTEGER NOT NULL,
    status      INTEGER NOT NULL,
    time        TEXT NOT NULL,
    msg         TEXT,
    ping        INTEGER,
    duration    INTEGER NOT NULL DEFAULT 0,
    important   INTEGER NOT NULL DEFAULT 0,
    down_count  INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

CREATE INDEX idx_hb_monitor_time ON heartbeats(monitor_id, time DESC);

-- Aggregate rollup tables
CREATE TABLE heartbeat_1m (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, bucket)
);

CREATE INDEX idx_1m_monitor_bucket ON heartbeat_1m(monitor_id, bucket DESC);

CREATE TABLE heartbeat_1h (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, bucket)
);

CREATE INDEX idx_1h_monitor_bucket ON heartbeat_1h(monitor_id, bucket DESC);

CREATE TABLE heartbeat_1d (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, bucket)
);

CREATE INDEX idx_1d_monitor_bucket ON heartbeat_1d(monitor_id, bucket DESC);

-- notifications (per-provider config stored as TEXT/JSON)
CREATE TABLE notifications (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    active      INTEGER NOT NULL DEFAULT 1,
    is_default  INTEGER NOT NULL DEFAULT 0,
    config      TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX idx_notif_user ON notifications(user_id);
CREATE INDEX idx_notif_type ON notifications(type);

-- monitor_notification (many-to-many)
CREATE TABLE monitor_notification (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    notification_id INTEGER NOT NULL,
    UNIQUE (monitor_id, notification_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE
);

-- status_pages
CREATE TABLE status_pages (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    slug                    TEXT NOT NULL UNIQUE,
    title                   TEXT NOT NULL,
    description             TEXT,
    icon                    TEXT,
    theme                   TEXT NOT NULL DEFAULT 'light',
    published               INTEGER NOT NULL DEFAULT 1,
    custom_domain           TEXT,
    password_hash           TEXT,
    footer_text             TEXT,
    custom_css              TEXT,
    dashboard_style         TEXT NOT NULL DEFAULT 'full',
    show_tags               INTEGER NOT NULL DEFAULT 0,
    auto_resolve_incidents  INTEGER NOT NULL DEFAULT 0,
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at              TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_sp_domain ON status_pages(custom_domain);

-- status_page_cnames (custom domain aliases)
CREATE TABLE status_page_cnames (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    status_page_id  INTEGER NOT NULL,
    domain          TEXT NOT NULL UNIQUE,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE
);

-- status_page_monitors
CREATE TABLE status_page_monitors (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    status_page_id  INTEGER NOT NULL,
    monitor_id      INTEGER NOT NULL,
    display_order   INTEGER NOT NULL DEFAULT 1000,
    UNIQUE (status_page_id, monitor_id),
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- tags
CREATE TABLE tags (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    name    TEXT NOT NULL UNIQUE,
    color   TEXT NOT NULL DEFAULT '#666666'
);

-- monitor_tags
CREATE TABLE monitor_tags (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id  INTEGER NOT NULL,
    tag_id      INTEGER NOT NULL,
    value       TEXT,
    UNIQUE (monitor_id, tag_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
);

-- maintenance_windows
CREATE TABLE maintenance_windows (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER,
    title       TEXT NOT NULL,
    description TEXT NOT NULL,
    active      INTEGER NOT NULL DEFAULT 1,
    strategy    TEXT NOT NULL DEFAULT 'single',
    start_date  TEXT,
    end_date    TEXT,
    cron_expr   TEXT,
    duration    INTEGER,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL
);

-- maintenance_window_monitors
CREATE TABLE maintenance_window_monitors (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    maintenance_window_id   INTEGER NOT NULL,
    monitor_id              INTEGER NOT NULL,
    UNIQUE (maintenance_window_id, monitor_id),
    FOREIGN KEY (maintenance_window_id) REFERENCES maintenance_windows(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- api_keys
CREATE TABLE api_keys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL,
    name        TEXT NOT NULL,
    key_hash    TEXT NOT NULL UNIQUE,
    active      INTEGER NOT NULL DEFAULT 1,
    expires_at  TEXT,
    scopes      TEXT NOT NULL DEFAULT '["read"]',
    last_used_at TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- incidents
CREATE TABLE incidents (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    status_page_id  INTEGER NOT NULL,
    title           TEXT NOT NULL,
    content         TEXT NOT NULL,
    style           TEXT NOT NULL DEFAULT 'warning',
    pinned          INTEGER NOT NULL DEFAULT 1,
    active          INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE
);

-- tls_info (cached TLS cert info per monitor)
CREATE TABLE tls_info (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id  INTEGER NOT NULL UNIQUE,
    info_json   TEXT NOT NULL,
    checked_at  TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- settings (key-value app config)
CREATE TABLE settings (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    setting_key     TEXT NOT NULL UNIQUE,
    value   TEXT NOT NULL
);

-- notification_sent_history (rate limiting / dedup)
CREATE TABLE notification_sent_history (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    notification_id INTEGER NOT NULL,
    monitor_id      INTEGER NOT NULL,
    last_sent_at    TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (notification_id, monitor_id),
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);
