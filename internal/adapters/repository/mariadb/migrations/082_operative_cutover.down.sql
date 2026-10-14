-- Explicit downgrade discards operative cutover history. Unresolved authority
-- must never be erased silently: the guard table below makes this migration
-- FAIL LOUDLY while any provider attempt is in flight/ambiguous, any source
-- hold is still held, or any barrier has not finished draining. Resolve the
-- attempts (FinishSourceProviderAttempt) and drain/release the barriers first;
-- an operator recovery contract exists for ambiguous attempts and must be used
-- instead of fabricating completion here.
DROP TABLE IF EXISTS operative_cutover_downgrade_guard;
CREATE TABLE operative_cutover_downgrade_guard (
    require_no_unresolved VARCHAR(16) NOT NULL CHECK (require_no_unresolved IN ('ok'))
) ENGINE=InnoDB;
INSERT INTO operative_cutover_downgrade_guard (require_no_unresolved)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM operative_provider_attempts WHERE state IN ('in_flight','ambiguous')) THEN 'blocked'
    WHEN EXISTS (SELECT 1 FROM operative_source_gates WHERE state = 'held') THEN 'blocked'
    WHEN EXISTS (SELECT 1 FROM operative_cutover_state WHERE barrier_id IS NOT NULL AND phase NOT IN ('preparing','released')) THEN 'blocked'
    ELSE 'ok'
END;
DROP TABLE operative_cutover_downgrade_guard;
DROP TABLE operative_aggregate_deliveries;
DROP TABLE operative_aggregate_intents;
DROP TABLE operative_aggregate_incidents;
DROP TABLE operative_provider_attempts;
DROP TABLE operative_source_capabilities;
DROP TABLE operative_local_applications;
DROP TABLE operative_source_gates;
DROP TABLE operative_cutover_commands;
DROP TABLE operative_cutover_members;
DROP TABLE operative_cutover_barriers;
DROP TABLE operative_cutover_state;
