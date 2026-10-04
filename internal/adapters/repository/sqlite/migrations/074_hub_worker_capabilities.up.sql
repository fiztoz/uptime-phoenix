-- Hub worker assignment-ownership attestation (M6, verification matrix T34).
-- See the MariaDB migration of the same number for the full rationale.
CREATE TABLE hub_worker_capabilities (
    worker_id TEXT NOT NULL PRIMARY KEY,
    assignment_protocol INTEGER NOT NULL CHECK (assignment_protocol >= 0),
    declared_at TIMESTAMP NOT NULL,
    lease_until TIMESTAMP NOT NULL
);
CREATE INDEX idx_hub_worker_capabilities_lease ON hub_worker_capabilities (lease_until);
