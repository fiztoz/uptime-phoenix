-- Explicit downgrade may remove terminal SSH preflight discovery history only.
-- A downgrade must refuse nonterminal receipts — including expired rows that
-- have not yet been reconciled to interrupted — because those rows still name
-- live queued/running/cancel-requested work whose executor may be about to
-- finish. Observe reconciles expiry; finish or observe every operation first.
DROP TABLE IF EXISTS probe_host_preflights_downgrade_guard;
CREATE TABLE probe_host_preflights_downgrade_guard (
    require_no_live_receipts TEXT NOT NULL CHECK (require_no_live_receipts IN ('ok'))
);
INSERT INTO probe_host_preflights_downgrade_guard (require_no_live_receipts)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM probe_host_preflights WHERE state IN ('queued', 'running', 'cancel_requested')) THEN 'blocked'
    ELSE 'ok'
END;
DROP TABLE probe_host_preflights_downgrade_guard;
DROP TABLE probe_host_preflights;
