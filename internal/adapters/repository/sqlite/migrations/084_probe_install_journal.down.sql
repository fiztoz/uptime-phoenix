-- Explicit downgrade may remove only uneventful terminal installation history:
-- terminal operations whose resources are planned/not-started or applied
-- SHARED entries (never owned). A downgrade must refuse every nonterminal
-- operation — including expired rows not yet reconciled to interrupted, which
-- still name live queued/running/resources-ready/cancel-requested work — every
-- in-flight or unknown resource, and every applied OWNED resource. A shared
-- image-cache record is history, not cleanup ownership. Observe/finish every
-- operation first; retirement and recovery are a future explicit protocol.
DROP TABLE IF EXISTS probe_install_journal_downgrade_guard;
CREATE TABLE probe_install_journal_downgrade_guard (
    require_removable_history TEXT NOT NULL CHECK (require_removable_history IN ('ok'))
);
INSERT INTO probe_install_journal_downgrade_guard (require_removable_history)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM probe_install_operations WHERE state IN ('queued', 'running', 'resources_ready', 'cancel_requested')) THEN 'blocked'
    WHEN EXISTS (SELECT 1 FROM probe_install_resources WHERE state IN ('in_flight', 'unknown')) THEN 'blocked'
    WHEN EXISTS (SELECT 1 FROM probe_install_resources WHERE state = 'applied' AND owned = 1) THEN 'blocked'
    ELSE 'ok'
END;
DROP TABLE probe_install_journal_downgrade_guard;
DROP TABLE probe_install_resources;
DROP TABLE probe_install_operations;
