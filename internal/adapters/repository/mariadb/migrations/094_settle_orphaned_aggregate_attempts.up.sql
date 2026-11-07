-- Settle aggregate provider attempts an older hub left in_flight under a
-- delivery it had already reconciled to a terminal status (#219).
--
-- Before this release, claim reconciliation marked a crashed delivery
-- failed/provider_outcome_unknown but never rewrote its expired in_flight grant,
-- and the claim selector never revisits a terminal delivery, so those rows
-- stayed in_flight with finished_at NULL forever. The current reconciler
-- settles them as ambiguous at its observation time; this backfill applies the
-- same decision to rows reconciled before the upgrade, using the delivery's
-- recorded outcome_at as that observation time.
--
-- Only expired grants under a terminal delivery are touched: a terminal
-- delivery is never re-leased, and an expired grant can no longer be running.
-- Ambiguous is never a fabricated success. Re-running is a no-op.
UPDATE operative_provider_attempts AS pa
JOIN operative_aggregate_deliveries AS d ON d.delivery_id = pa.delivery_id
SET pa.state = 'ambiguous',
    pa.finished_at = COALESCE(d.outcome_at, UTC_TIMESTAMP(6))
WHERE pa.scope = 'aggregate'
  AND pa.state = 'in_flight'
  AND pa.budget_until < UTC_TIMESTAMP(6)
  AND d.status IN ('sent', 'failed', 'superseded');
