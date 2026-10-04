ALTER TABLE monitor_probe_assignments
    DROP CONSTRAINT chk_probe_resource_binding,
    DROP COLUMN resource_binding_kind,
    DROP COLUMN resource_binding_key;
