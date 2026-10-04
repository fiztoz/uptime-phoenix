-- Refuse to drop the column while any mirrored certificate incident or
-- certificate delivery outcome exists. A downgrade must never silently discard
-- source certificate history. The guard deliberately names only columns that
-- outlive this migration, so it still reports the guard (rather than an unrelated
-- unknown-column error) when the schema is already older than 067.
CREATE TEMPORARY TABLE probe_certificate_paging_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO probe_certificate_paging_downgrade_guard SELECT COUNT(*) FROM probe_incidents WHERE subject_kind = 'certificate';
INSERT INTO probe_certificate_paging_downgrade_guard SELECT COUNT(*) FROM probe_delivery_events WHERE event_kind = 'certificate_expiry';
DROP TABLE probe_certificate_paging_downgrade_guard;
ALTER TABLE probe_incidents DROP COLUMN IF EXISTS certificate_not_after;
