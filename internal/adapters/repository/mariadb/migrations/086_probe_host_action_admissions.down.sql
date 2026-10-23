-- Any committed admission may have authorized an action, even after signer
-- failure. Erasing consumption would rearm authority. No automatic downgrade,
-- retention or cleanup is permitted while even one admission remains.
DROP TABLE IF EXISTS probe_host_action_admissions_downgrade_guard;
CREATE TABLE probe_host_action_admissions_downgrade_guard (
    require_empty_admissions VARCHAR(16) NOT NULL CHECK (require_empty_admissions IN ('ok'))
) ENGINE=InnoDB;
INSERT INTO probe_host_action_admissions_downgrade_guard (require_empty_admissions)
SELECT CASE WHEN EXISTS (SELECT 1 FROM probe_host_action_admissions) THEN 'blocked' ELSE 'ok' END;
DROP TABLE probe_host_action_admissions_downgrade_guard;
DROP TABLE probe_host_action_admissions;
