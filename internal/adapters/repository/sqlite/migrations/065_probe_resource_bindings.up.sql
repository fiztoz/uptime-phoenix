ALTER TABLE monitor_probe_assignments ADD COLUMN resource_binding_key TEXT NOT NULL DEFAULT '' CHECK (length(resource_binding_key) <= 128);
ALTER TABLE monitor_probe_assignments ADD COLUMN resource_binding_kind TEXT NOT NULL DEFAULT '' CHECK (
    (resource_binding_key = '' AND resource_binding_kind = '') OR
    (probe_id <> 'local' AND resource_binding_key <> '' AND resource_binding_kind IN ('docker_socket', 'docker_api'))
);
