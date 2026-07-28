-- F5 Sprint 14: stable operator keys for declarative config-as-code.
-- Maps (resource_type, key_name) → resource_id so YAML documents never depend
-- on auto-increment IDs. See docs/F5-S14-CONFIG-AS-CODE-CONTRACTS.md.

CREATE TABLE config_keys (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    resource_type TEXT NOT NULL,
    key_name      TEXT NOT NULL,
    resource_id   INTEGER NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (resource_type, key_name),
    UNIQUE (resource_type, resource_id)
);

CREATE INDEX idx_config_keys_resource ON config_keys(resource_type, resource_id);
