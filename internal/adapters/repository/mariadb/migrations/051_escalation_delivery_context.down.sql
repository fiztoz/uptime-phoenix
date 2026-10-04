-- Stop writers. A downgrade must not erase the identity of retained escalation work.
CREATE TABLE IF NOT EXISTS escalation_delivery_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO escalation_delivery_downgrade_guard (ok) SELECT 0 FROM probe_delivery_intents WHERE escalation_step > 0 OR escalation_policy_id > 0 LIMIT 1;
DROP TABLE escalation_delivery_downgrade_guard;
ALTER TABLE probe_delivery_intents DROP COLUMN escalation_step;
ALTER TABLE probe_delivery_intents DROP COLUMN escalation_policy_id;
