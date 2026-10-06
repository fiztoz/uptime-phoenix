-- Revert worker lease fencing. An older binary never reads or writes
-- lease_epoch, so dropping it restores the pre-075 lease contract:
-- ownership is worker_id + leased_at only and epoch-based fencing degrades
-- away with the code that enforced it.
ALTER TABLE monitors DROP COLUMN lease_epoch;
