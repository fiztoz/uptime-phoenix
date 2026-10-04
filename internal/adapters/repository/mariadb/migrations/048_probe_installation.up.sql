-- Singleton installation identity and key confirmation record.
CREATE TABLE probe_installation (
    id TINYINT NOT NULL PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    key_hash VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (key_hash REGEXP '^[0-9a-f]{64}$'),
    protocol_floor INT NOT NULL DEFAULT 1 CHECK (protocol_floor >= 1),
    authority_epoch BIGINT NOT NULL DEFAULT 1 CHECK (authority_epoch >= 1),
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL
) ENGINE=InnoDB;
