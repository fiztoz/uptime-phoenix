-- F5 Sprint 14: stable operator keys for declarative config-as-code.
-- Maps (resource_type, key_name) → resource_id so YAML documents never depend
-- on auto-increment IDs. See docs/F5-S14-CONFIG-AS-CODE-CONTRACTS.md.

CREATE TABLE config_keys (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    resource_type VARCHAR(40)  NOT NULL,
    key_name      VARCHAR(128) NOT NULL,
    resource_id   BIGINT       NOT NULL,
    created_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_config_keys_type_name (resource_type, key_name),
    UNIQUE KEY uq_config_keys_type_id (resource_type, resource_id),
    INDEX idx_config_keys_resource (resource_type, resource_id)
);
