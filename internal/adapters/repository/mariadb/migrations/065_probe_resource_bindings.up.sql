ALTER TABLE monitor_probe_assignments
    ADD COLUMN resource_binding_key VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN resource_binding_kind VARCHAR(32) NOT NULL DEFAULT '',
    ADD CONSTRAINT chk_probe_resource_binding CHECK (
        (resource_binding_key = '' AND resource_binding_kind = '') OR
        (probe_id <> 'local' AND resource_binding_key <> '' AND resource_binding_kind IN ('docker_socket', 'docker_api'))
    );
