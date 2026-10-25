-- Any committed activation or admission is durable evidence: an activation row
-- records an operation that may have initialized, registered, started or
-- enrolled a real probe, and an admission may have authorized a host action even
-- after signer failure. Erasing either would hide that history and rearm
-- authority. No automatic downgrade, retention or cleanup is permitted while
-- even one activation row or admission remains.
DROP TABLE IF EXISTS probe_install_activations_downgrade_guard;
CREATE TABLE probe_install_activations_downgrade_guard (
    require_empty_activations VARCHAR(16) NOT NULL CHECK (require_empty_activations IN ('ok'))
) ENGINE=InnoDB;
INSERT INTO probe_install_activations_downgrade_guard (require_empty_activations)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM probe_activation_admissions) THEN 'blocked'
    WHEN EXISTS (SELECT 1 FROM probe_install_activations) THEN 'blocked'
    ELSE 'ok'
END;
DROP TABLE probe_install_activations_downgrade_guard;
DROP TABLE probe_activation_admissions;
DROP TABLE probe_install_activation_steps;
DROP TABLE probe_install_activations;
