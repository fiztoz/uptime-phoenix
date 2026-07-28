ALTER TABLE status_pages
    ADD COLUMN sla_target DECIMAL(6,3) NULL AFTER auto_resolve_incidents;
