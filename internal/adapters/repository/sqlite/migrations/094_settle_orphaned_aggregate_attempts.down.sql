-- Data-only backfill: an older binary treats ambiguous and in_flight past its
-- budget the same way, so there is nothing to revert.
SELECT 1;
