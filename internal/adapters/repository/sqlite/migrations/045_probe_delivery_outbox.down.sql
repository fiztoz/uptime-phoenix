-- Stop all writers. Refuse to discard queued work or completed source receipts.
CREATE TABLE IF NOT EXISTS probe_delivery_outbox_down_guard (
    state_count INTEGER NOT NULL CHECK (state_count = 0)
);
DELETE FROM probe_delivery_outbox_down_guard;
INSERT INTO probe_delivery_outbox_down_guard (state_count)
SELECT COUNT(*) FROM probe_delivery_intents;
DROP TABLE probe_delivery_intents;
DROP TABLE probe_delivery_outbox_down_guard;
