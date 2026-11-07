-- Settle aggregate provider attempts an older hub left in_flight under a
-- delivery it had already reconciled to a terminal status (#219). See the
-- MariaDB migration of the same number for the full rationale.
UPDATE operative_provider_attempts
SET state = 'ambiguous',
    finished_at = COALESCE(
        (SELECT d.outcome_at FROM operative_aggregate_deliveries AS d
         WHERE d.delivery_id = operative_provider_attempts.delivery_id),
        strftime('%Y-%m-%d %H:%M:%f', 'now'))
WHERE scope = 'aggregate'
  AND state = 'in_flight'
  AND budget_until < strftime('%Y-%m-%d %H:%M:%f', 'now')
  AND delivery_id IN (SELECT delivery_id FROM operative_aggregate_deliveries
                      WHERE status IN ('sent', 'failed', 'superseded'));
