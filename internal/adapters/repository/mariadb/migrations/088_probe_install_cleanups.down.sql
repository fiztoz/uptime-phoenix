-- Any committed cleanup or admission is durable evidence: a cleanup row records
-- an operation that may have removed real host resources, and an admission may
-- have authorized a host action even after signer failure. Erasing either would
-- hide that history, rearm authority and let CreateActivation run again over a
-- host that was already cleaned. No automatic downgrade, retention or cleanup is
-- permitted while even one cleanup row or admission remains.
DROP TABLE IF EXISTS probe_install_cleanups_downgrade_guard;
CREATE TABLE probe_install_cleanups_downgrade_guard (
    require_empty_cleanups VARCHAR(16) NOT NULL CHECK (require_empty_cleanups IN ('ok'))
) ENGINE=InnoDB;
INSERT INTO probe_install_cleanups_downgrade_guard (require_empty_cleanups)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM probe_cleanup_admissions) THEN 'blocked'
    WHEN EXISTS (SELECT 1 FROM probe_install_cleanups) THEN 'blocked'
    ELSE 'ok'
END;
DROP TABLE IF EXISTS probe_install_cleanups_downgrade_guard;
DROP TABLE IF EXISTS probe_cleanup_admissions;
DROP TABLE IF EXISTS probe_install_cleanups;
