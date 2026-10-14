-- Explicit downgrade discards pending-only round history; no mode change.
DROP TABLE IF EXISTS probe_delivery_preparation_requests;
DROP TABLE IF EXISTS probe_delivery_preparation_members;
DROP TABLE IF EXISTS probe_delivery_preparation_rounds;
