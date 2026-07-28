-- 007_monitor_groups.up.sql — MariaDB: first-class monitor groups (folders)
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
-- parent_id's foreign key has no explicit name in 001_init.up.sql, so
-- InnoDB auto-generated one. Verified live against MariaDB 11.8 seeded from
-- this repo's actual 001-006 migrations: InnoDB names constraints by FK
-- declaration order within the CREATE TABLE, so parent_id — the second FK
-- declared on monitors, right after user_id — is monitors_ibfk_2. The
-- second DROP FOREIGN KEY IF EXISTS below additionally covers a database
-- that has already been through one down/up cycle of this same migration,
-- where the restored parent_id FK is named fk_monitors_parent instead (see
-- 007_monitor_groups.down.sql) — both forms were exercised end to end
-- (up -> down -> up -> down -> up) against a real MariaDB container.

CREATE TABLE monitor_groups (
    id                    BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id               BIGINT NOT NULL,
    name                  VARCHAR(255) NOT NULL,
    description           TEXT,
    parent_id             BIGINT,
    status_condition      VARCHAR(30) NOT NULL DEFAULT 'worst_of_children',
    threshold             INT NOT NULL DEFAULT 0,
    threshold_is_percent  BOOLEAN NOT NULL DEFAULT FALSE,
    weight                INT NOT NULL DEFAULT 2000,
    collapsed             BOOLEAN NOT NULL DEFAULT FALSE,
    created_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (parent_id) REFERENCES monitor_groups(id) ON DELETE SET NULL,
    INDEX idx_monitor_groups_user (user_id),
    INDEX idx_monitor_groups_parent (parent_id)
);

ALTER TABLE monitors
    DROP FOREIGN KEY IF EXISTS monitors_ibfk_2,
    DROP FOREIGN KEY IF EXISTS fk_monitors_parent,
    DROP COLUMN parent_id,
    ADD COLUMN IF NOT EXISTS group_id BIGINT DEFAULT NULL,
    ADD CONSTRAINT fk_monitors_group FOREIGN KEY (group_id) REFERENCES monitor_groups(id) ON DELETE SET NULL,
    ADD INDEX idx_monitors_group (group_id);
