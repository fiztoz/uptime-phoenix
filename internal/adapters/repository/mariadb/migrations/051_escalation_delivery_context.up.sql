-- Preserve step identity across provider retries and process restarts.
ALTER TABLE probe_delivery_intents ADD COLUMN escalation_policy_id BIGINT NOT NULL DEFAULT 0 CHECK (escalation_policy_id >= 0);
ALTER TABLE probe_delivery_intents ADD COLUMN escalation_step INT NOT NULL DEFAULT 0 CHECK (escalation_step >= 0);
