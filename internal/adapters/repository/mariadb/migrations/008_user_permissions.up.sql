-- 008_user_permissions.up.sql — MariaDB: per-user RBAC grants + capability flags
-- Created_at: 2026-07-12
-- Updated_at: 2026-07-12
--
-- Two independent pieces:
--
-- 1. users.can_manage_notifications / users.can_manage_maintenance — capability
--    flags a non-admin can hold. They are the ONLY write powers a non-admin can
--    be given; monitors and groups stay admin-only to mutate. Default FALSE so
--    every pre-existing non-admin user stays read-only. Pre-existing admins are
--    unaffected: IsAdmin already implies both capabilities in the service, so
--    unlike 006 (which had to promote existing users to admin) there is nothing
--    to backfill.
--
-- 2. user_permissions — the view grants themselves. One row grants ONE user view
--    access to ONE resource: a monitor (monitor_id set) or a group (group_id
--    set), never both. A group grant is recursive over the group tree (expanded
--    in AccessService, not here).
--
-- The CHECK constraint (MariaDB >= 10.2 enforces these) encodes the
-- exactly-one-target rule at the DB level, so a bad grant cannot be written even
-- if a future caller bypasses the domain guard.
--
-- The two UNIQUE keys make a grant idempotent per (user, resource). InnoDB
-- treats NULLs as distinct in a UNIQUE index, so one user's many group-grant
-- rows (monitor_id NULL) do not collide on uq_user_permissions_monitor, and
-- vice versa — same semantics as the SQLite side.
--
-- Every FK is named explicitly (fk_user_permissions_*) rather than left to
-- InnoDB's auto-naming, so the down migration can drop the table by name without
-- depending on constraint-ordering guesswork (see 007's monitors_ibfk_2 note).
-- Column types match the referenced PKs exactly: users.id, monitors.id and
-- monitor_groups.id are all BIGINT.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS can_manage_notifications BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS can_manage_maintenance BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE user_permissions (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    monitor_id  BIGINT DEFAULT NULL,
    group_id    BIGINT DEFAULT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_user_permissions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT fk_user_permissions_monitor FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    CONSTRAINT fk_user_permissions_group FOREIGN KEY (group_id) REFERENCES monitor_groups(id) ON DELETE CASCADE,
    CONSTRAINT chk_user_permissions_one_target CHECK ((monitor_id IS NULL) <> (group_id IS NULL)),
    UNIQUE KEY uq_user_permissions_monitor (user_id, monitor_id),
    UNIQUE KEY uq_user_permissions_group (user_id, group_id),
    INDEX idx_user_permissions_user (user_id)
);
