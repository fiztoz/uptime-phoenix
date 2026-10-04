-- Hub worker assignment-ownership attestation (M6, verification matrix T34).
--
-- A sharded hub worker that executes checks declares the probe assignment
-- protocol it enforces. Remote activation fails closed while any worker holding
-- a live monitor lease has not attested the required protocol, so a mixed-version
-- rollout cannot hand a monitor to a probe while an unaware worker still runs it
-- locally. A worker built before assignment ownership cannot be changed
-- retroactively, but it does claim monitor leases, which makes it observable.
--
-- No secret material. Rows are pruned once their declared lease expires.
CREATE TABLE IF NOT EXISTS hub_worker_capabilities (
    worker_id VARCHAR(128) NOT NULL PRIMARY KEY,
    assignment_protocol INT NOT NULL CHECK (assignment_protocol >= 0),
    declared_at DATETIME(6) NOT NULL,
    lease_until DATETIME(6) NOT NULL,
    KEY idx_hub_worker_capabilities_lease (lease_until)
) ENGINE=InnoDB;
