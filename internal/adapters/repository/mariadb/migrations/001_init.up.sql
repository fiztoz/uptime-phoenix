-- 001_init.up.sql — MariaDB schema for Phoenix
-- Creates all tables for the monitoring system

-- users
CREATE TABLE users (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    username        VARCHAR(255) NOT NULL UNIQUE,
    password_hash   VARCHAR(255) NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    timezone        VARCHAR(150) DEFAULT 'UTC',
    totp_secret     VARBINARY(128),
    totp_enabled    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);

-- proxies (must be created before monitors due to FK)
CREATE TABLE proxies (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    protocol    VARCHAR(10) NOT NULL,
    host        VARCHAR(255) NOT NULL,
    port        INT NOT NULL,
    auth        BOOLEAN NOT NULL DEFAULT FALSE,
    username    VARCHAR(255),
    password    VARCHAR(255),
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    is_default  BOOLEAN NOT NULL DEFAULT FALSE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- docker_hosts (must be created before monitors due to FK)
CREATE TABLE docker_hosts (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    name            VARCHAR(255) NOT NULL,
    docker_daemon   VARCHAR(255) NOT NULL,
    docker_type     VARCHAR(50) NOT NULL DEFAULT 'socket',
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- monitors (per-type config in JSON column)
CREATE TABLE monitors (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    type            VARCHAR(30) NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    `check_interval`        INT NOT NULL DEFAULT 60,
    retry_interval  INT NOT NULL DEFAULT 0,
    max_retries     INT NOT NULL DEFAULT 0,
    timeout         DOUBLE NOT NULL DEFAULT 30.0,
    parent_id       BIGINT,
    weight          INT NOT NULL DEFAULT 2000,
    push_token      VARCHAR(64),
    proxy_id        BIGINT,
    tls_ignore      BOOLEAN NOT NULL DEFAULT FALSE,
    accepted_statuscodes JSON NOT NULL DEFAULT '["200-299"]',
    resend_interval INT NOT NULL DEFAULT 0,
    upside_down     BOOLEAN NOT NULL DEFAULT FALSE,
    config          JSON NOT NULL,
    docker_host_id  BIGINT,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
    FOREIGN KEY (parent_id) REFERENCES monitors(id) ON DELETE SET NULL,
    FOREIGN KEY (proxy_id) REFERENCES proxies(id) ON DELETE SET NULL,
    FOREIGN KEY (docker_host_id) REFERENCES docker_hosts(id) ON DELETE SET NULL,
    INDEX idx_monitors_active (active),
    INDEX idx_monitors_type (type),
    INDEX idx_monitors_user (user_id)
);

-- heartbeats (PARTITIONED by RANGE on time, monthly)
CREATE TABLE heartbeats (
    id          BIGINT NOT NULL AUTO_INCREMENT,
    monitor_id  BIGINT NOT NULL,
    status      TINYINT NOT NULL,
    time        TIMESTAMP NOT NULL,
    msg         TEXT,
    ping        INT,
    duration    INT NOT NULL DEFAULT 0,
    important   BOOLEAN NOT NULL DEFAULT FALSE,
    down_count  INT NOT NULL DEFAULT 0,
    PRIMARY KEY (id, time),
    -- MariaDB does not allow FOREIGN KEY on partitioned tables (ERROR 1506).
    INDEX idx_hb_monitor_time (monitor_id, time DESC)
) PARTITION BY RANGE (UNIX_TIMESTAMP(time)) (
    PARTITION p202606 VALUES LESS THAN (UNIX_TIMESTAMP('2026-07-01 00:00:00')),
    PARTITION p202607 VALUES LESS THAN (UNIX_TIMESTAMP('2026-08-01 00:00:00')),
    PARTITION p202608 VALUES LESS THAN (UNIX_TIMESTAMP('2026-09-01 00:00:00')),
    PARTITION p202609 VALUES LESS THAN (UNIX_TIMESTAMP('2026-10-01 00:00:00')),
    PARTITION p202610 VALUES LESS THAN (UNIX_TIMESTAMP('2026-11-01 00:00:00')),
    PARTITION p202611 VALUES LESS THAN (UNIX_TIMESTAMP('2026-12-01 00:00:00')),
    PARTITION p202612 VALUES LESS THAN (UNIX_TIMESTAMP('2027-01-01 00:00:00')),
    PARTITION p202701 VALUES LESS THAN (UNIX_TIMESTAMP('2027-02-01 00:00:00')),
    PARTITION p202702 VALUES LESS THAN (UNIX_TIMESTAMP('2027-03-01 00:00:00')),
    PARTITION p202703 VALUES LESS THAN (UNIX_TIMESTAMP('2027-04-01 00:00:00')),
    PARTITION p202704 VALUES LESS THAN (UNIX_TIMESTAMP('2027-05-01 00:00:00')),
    PARTITION p202705 VALUES LESS THAN (UNIX_TIMESTAMP('2027-06-01 00:00:00')),
    PARTITION p202706 VALUES LESS THAN (UNIX_TIMESTAMP('2027-07-01 00:00:00')),
    PARTITION pmax    VALUES LESS THAN MAXVALUE
);

-- Aggregate rollup tables
CREATE TABLE heartbeat_1m (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id      BIGINT NOT NULL,
    bucket          TIMESTAMP NOT NULL,
    up_count        INT NOT NULL DEFAULT 0,
    down_count      INT NOT NULL DEFAULT 0,
    pending_count   INT NOT NULL DEFAULT 0,
    maint_count     INT NOT NULL DEFAULT 0,
    avg_ping        DOUBLE,
    min_ping        INT,
    max_ping        INT,
    total_checks    INT NOT NULL DEFAULT 0,
    UNIQUE KEY uq_monitor_bucket (monitor_id, bucket),
    INDEX idx_1m_monitor_bucket (monitor_id, bucket DESC)
);

CREATE TABLE heartbeat_1h (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id      BIGINT NOT NULL,
    bucket          TIMESTAMP NOT NULL,
    up_count        INT NOT NULL DEFAULT 0,
    down_count      INT NOT NULL DEFAULT 0,
    pending_count   INT NOT NULL DEFAULT 0,
    maint_count     INT NOT NULL DEFAULT 0,
    avg_ping        DOUBLE,
    min_ping        INT,
    max_ping        INT,
    total_checks    INT NOT NULL DEFAULT 0,
    UNIQUE KEY uq_monitor_bucket (monitor_id, bucket),
    INDEX idx_1h_monitor_bucket (monitor_id, bucket DESC)
);

CREATE TABLE heartbeat_1d (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id      BIGINT NOT NULL,
    bucket          TIMESTAMP NOT NULL,
    up_count        INT NOT NULL DEFAULT 0,
    down_count      INT NOT NULL DEFAULT 0,
    pending_count   INT NOT NULL DEFAULT 0,
    maint_count     INT NOT NULL DEFAULT 0,
    avg_ping        DOUBLE,
    min_ping        INT,
    max_ping        INT,
    total_checks    INT NOT NULL DEFAULT 0,
    UNIQUE KEY uq_monitor_bucket (monitor_id, bucket),
    INDEX idx_1d_monitor_bucket (monitor_id, bucket DESC)
);

-- notifications (per-provider config in JSON)
CREATE TABLE notifications (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT,
    name        VARCHAR(255) NOT NULL,
    type        VARCHAR(50) NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    is_default  BOOLEAN NOT NULL DEFAULT FALSE,
    config      JSON NOT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
    INDEX idx_notif_user (user_id),
    INDEX idx_notif_type (type)
);

-- monitor_notification (many-to-many)
CREATE TABLE monitor_notification (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id      BIGINT NOT NULL,
    notification_id BIGINT NOT NULL,
    UNIQUE KEY uq_pair (monitor_id, notification_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE
);

-- status_pages
CREATE TABLE status_pages (
    id                      BIGINT AUTO_INCREMENT PRIMARY KEY,
    slug                    VARCHAR(255) NOT NULL UNIQUE,
    title                   VARCHAR(255) NOT NULL,
    description             TEXT,
    icon                    VARCHAR(255),
    theme                   VARCHAR(30) NOT NULL DEFAULT 'light',
    published               BOOLEAN NOT NULL DEFAULT TRUE,
    custom_domain           VARCHAR(255),
    password_hash           VARCHAR(255),
    footer_text             TEXT,
    custom_css              TEXT,
    dashboard_style         VARCHAR(30) NOT NULL DEFAULT 'full',
    show_tags               BOOLEAN NOT NULL DEFAULT FALSE,
    auto_resolve_incidents  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at              TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at              TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_sp_domain (custom_domain)
);

-- status_page_cnames (custom domain aliases)
CREATE TABLE status_page_cnames (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    status_page_id  BIGINT NOT NULL,
    domain          VARCHAR(255) NOT NULL UNIQUE,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE
);

-- status_page_monitors
CREATE TABLE status_page_monitors (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    status_page_id  BIGINT NOT NULL,
    monitor_id      BIGINT NOT NULL,
    display_order   INT NOT NULL DEFAULT 1000,
    UNIQUE KEY uq_pair (status_page_id, monitor_id),
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- tags
CREATE TABLE tags (
    id      BIGINT AUTO_INCREMENT PRIMARY KEY,
    name    VARCHAR(255) NOT NULL UNIQUE,
    color   VARCHAR(7) NOT NULL DEFAULT '#666666'
);

-- monitor_tags
CREATE TABLE monitor_tags (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id  BIGINT NOT NULL,
    tag_id      BIGINT NOT NULL,
    value       TEXT,
    UNIQUE KEY uq_pair (monitor_id, tag_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
);

-- maintenance_windows
CREATE TABLE maintenance_windows (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT,
    title       VARCHAR(255) NOT NULL,
    description TEXT NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    strategy    VARCHAR(50) NOT NULL DEFAULT 'single',
    start_date  TIMESTAMP,
    end_date    TIMESTAMP,
    cron_expr   VARCHAR(100),
    duration    INT,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL
);

-- maintenance_window_monitors
CREATE TABLE maintenance_window_monitors (
    id                      BIGINT AUTO_INCREMENT PRIMARY KEY,
    maintenance_window_id   BIGINT NOT NULL,
    monitor_id              BIGINT NOT NULL,
    UNIQUE KEY uq_pair (maintenance_window_id, monitor_id),
    FOREIGN KEY (maintenance_window_id) REFERENCES maintenance_windows(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- api_keys
CREATE TABLE api_keys (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    name        VARCHAR(255) NOT NULL,
    key_hash    VARCHAR(255) NOT NULL UNIQUE,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    expires_at  TIMESTAMP,
    scopes      JSON NOT NULL DEFAULT '["read"]',
    last_used_at TIMESTAMP,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- incidents
CREATE TABLE incidents (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    status_page_id  BIGINT NOT NULL,
    title           VARCHAR(255) NOT NULL,
    content         TEXT NOT NULL,
    style           VARCHAR(30) NOT NULL DEFAULT 'warning',
    pinned          BOOLEAN NOT NULL DEFAULT TRUE,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE
);

-- tls_info (cached TLS cert info per monitor)
CREATE TABLE tls_info (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id  BIGINT NOT NULL UNIQUE,
    info_json   JSON NOT NULL,
    checked_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- settings (key-value app config)
CREATE TABLE settings (
    id      BIGINT AUTO_INCREMENT PRIMARY KEY,
    `setting_key`     VARCHAR(200) NOT NULL UNIQUE,
    value   JSON NOT NULL
);

-- notification_sent_history (rate limiting / dedup)
CREATE TABLE notification_sent_history (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    notification_id BIGINT NOT NULL,
    monitor_id      BIGINT NOT NULL,
    last_sent_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_pair (notification_id, monitor_id),
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);
