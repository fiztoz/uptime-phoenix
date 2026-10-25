-- An admission of an image_cache or container attempt may have authorized a
-- real Docker action, even after signer failure. Erasing it would rearm
-- authority, and narrowing the kind CHECK under it would leave rows the 086
-- schema cannot hold (MariaDB does not revalidate existing rows on this ALTER).
-- No automatic downgrade is permitted while even one such admission remains.
DROP TABLE IF EXISTS probe_docker_host_actions_downgrade_guard;
CREATE TABLE probe_docker_host_actions_downgrade_guard (
    require_no_docker_admissions VARCHAR(16) NOT NULL CHECK (require_no_docker_admissions IN ('ok'))
) ENGINE=InnoDB;
INSERT INTO probe_docker_host_actions_downgrade_guard (require_no_docker_admissions)
SELECT CASE WHEN EXISTS (SELECT 1 FROM probe_host_action_admissions WHERE kind IN ('image_cache', 'container')) THEN 'blocked' ELSE 'ok' END;
DROP TABLE probe_docker_host_actions_downgrade_guard;
-- Restore 086's exact column definition and kind CHECK.
ALTER TABLE probe_host_action_admissions
    MODIFY kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('data_directory', 'unit_file', 'service'));
