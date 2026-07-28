-- F2.2: monitor alert lifecycle (firing → acked → resolved).
-- open_monitor_id = monitor_id while open; NULL when resolved. UNIQUE allows
-- only one open alert per monitor (multiple NULLs are permitted).

CREATE TABLE alerts (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id        INTEGER NOT NULL,
    status            TEXT NOT NULL,
    message           TEXT NOT NULL,
    fired_at          TIMESTAMP NOT NULL,
    acked_at          TIMESTAMP NULL,
    acked_by_user_id  INTEGER NULL,
    resolved_at       TIMESTAMP NULL,
    ack_token         TEXT NOT NULL,
    open_monitor_id   INTEGER NULL,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (acked_by_user_id) REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (ack_token),
    UNIQUE (open_monitor_id)
);

CREATE INDEX idx_alerts_monitor ON alerts (monitor_id);
CREATE INDEX idx_alerts_status ON alerts (status);
CREATE INDEX idx_alerts_fired ON alerts (fired_at);
