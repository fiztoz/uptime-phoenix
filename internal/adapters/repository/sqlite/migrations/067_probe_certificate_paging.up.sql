-- Certificate paging: the immutable subject expiry of a mirrored certificate
-- incident, stored in this table's textual timestamp form like its siblings.
-- Threshold alone cannot identify an incident, because the same threshold recurs
-- across renewals. X.509 validity instants are whole seconds, so this is
-- lossless for the identity; the source stays the only authority on when a
-- threshold was actually delivered.
ALTER TABLE probe_incidents ADD COLUMN certificate_not_after TEXT NULL;
