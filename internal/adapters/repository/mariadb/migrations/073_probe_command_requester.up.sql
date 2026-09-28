ALTER TABLE probe_commands ADD COLUMN IF NOT EXISTS requested_by BIGINT NOT NULL DEFAULT 0 CHECK (requested_by >= 0);
