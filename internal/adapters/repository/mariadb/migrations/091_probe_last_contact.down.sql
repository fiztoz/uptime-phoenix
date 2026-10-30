-- Revert hub-observed last contact. An older binary never reads or writes the
-- column; last seen falls back to its earlier derivation.
ALTER TABLE probe_sessions DROP COLUMN IF EXISTS last_contact_at;
