-- 009_group_notifications.up.sql — MariaDB: notifications attached to a monitor group
-- Created_at: 2026-07-13
-- Updated_at: 2026-07-13
--
-- Two pieces:
--
-- 1. group_notifications — the many-to-many link between a monitor group
--    (folder) and a notification. It mirrors monitor_notification exactly
--    (UNIQUE pair, both FKs CASCADE) but means something different: a group
--    alerts on its OWN derived status — the rollup its status_condition
--    produces — and never inherits the link down to the monitors inside it.
--
--    Both FKs cascade on delete so a deleted group or a deleted notification
--    cannot leave a dangling link behind that a later row reusing the same
--    AUTO_INCREMENT id would silently inherit. Same reasoning as 008's grants.
--
--    The FK constraints are NAMED. InnoDB would otherwise auto-generate
--    group_notifications_ibfk_1/2, and 007 is a standing lesson in how much
--    guesswork an unnamed constraint costs the next migration that has to drop it.
--
-- 2. monitor_groups.last_status — the group's last OBSERVED derived status,
--    nullable (NULL = never evaluated). TINYINT: domain.Status is 0..3.
--
--    This column exists for RACE SAFETY, not for display. Two sharded workers
--    can process heartbeats for two monitors in the same group at the same
--    instant; both recompute the rollup as DOWN, and an in-memory last-status
--    map would let both send an alert. Instead the transition is CLAIMED with a
--    compare-and-set:
--
--        UPDATE monitor_groups SET last_status = :new
--         WHERE id = :id AND last_status <=> :old      -- MariaDB's null-safe =
--
--    RowsAffected == 1 means this worker won the transition and may alert; 0
--    means another worker already moved the group and this one must stay quiet.
--    (The CAS is only ever issued when :new <> :old, so the "no rows changed"
--    case MariaDB reports as 0 affected rows cannot be confused with a loss.)
--    The equivalent SQLite migration uses `last_status IS :old` for the same
--    null-safe comparison, so both engines are observably identical.
--
--    It is written ONLY by that CAS: MonitorGroupRepo.Update excludes the column
--    so an admin PUT round-tripping a stale group object cannot clobber an
--    alerting decision a worker just made.

CREATE TABLE group_notifications (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    notification_id BIGINT NOT NULL,
    group_id        BIGINT NOT NULL,
    UNIQUE KEY uq_group_notification (group_id, notification_id),
    CONSTRAINT fk_group_notifications_group
        FOREIGN KEY (group_id) REFERENCES monitor_groups(id) ON DELETE CASCADE,
    CONSTRAINT fk_group_notifications_notification
        FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE,
    INDEX idx_group_notifications_group (group_id),
    INDEX idx_group_notifications_notification (notification_id)
);

ALTER TABLE monitor_groups ADD COLUMN IF NOT EXISTS last_status TINYINT DEFAULT NULL;
