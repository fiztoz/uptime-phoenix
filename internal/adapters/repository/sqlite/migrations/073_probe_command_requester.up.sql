ALTER TABLE probe_commands ADD COLUMN requested_by INTEGER NOT NULL DEFAULT 0 CHECK (requested_by >= 0);
