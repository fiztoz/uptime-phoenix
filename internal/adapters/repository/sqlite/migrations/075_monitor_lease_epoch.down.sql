-- Revert worker lease fencing. SQLite learned DROP COLUMN in 3.35; the
-- modernc.org/sqlite runtime used here is well past that.
ALTER TABLE monitors DROP COLUMN lease_epoch;
