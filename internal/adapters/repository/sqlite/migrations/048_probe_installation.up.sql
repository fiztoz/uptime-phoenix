-- Singleton installation identity and key confirmation record.
CREATE TABLE probe_installation (
    id INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    hub_id TEXT NOT NULL CHECK (LENGTH(hub_id) = 36),
    key_hash TEXT NOT NULL CHECK (LENGTH(key_hash) = 64),
    protocol_floor INTEGER NOT NULL DEFAULT 1 CHECK (protocol_floor >= 1),
    authority_epoch INTEGER NOT NULL DEFAULT 1 CHECK (authority_epoch >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
