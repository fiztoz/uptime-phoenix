-- F2.2: monitor alert lifecycle (firing → acked → resolved).
-- open_monitor_id = monitor_id while open; NULL when resolved. UNIQUE allows
-- only one open alert per monitor (multiple NULLs are permitted).

CREATE TABLE alerts (
    id                BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id        BIGINT NOT NULL,
    status            VARCHAR(20) NOT NULL,
    message           TEXT NOT NULL,
    fired_at          TIMESTAMP NOT NULL,
    acked_at          TIMESTAMP NULL DEFAULT NULL,
    acked_by_user_id  BIGINT NULL DEFAULT NULL,
    resolved_at       TIMESTAMP NULL DEFAULT NULL,
    ack_token         VARCHAR(64) NOT NULL,
    open_monitor_id   BIGINT NULL DEFAULT NULL,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (acked_by_user_id) REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE KEY uq_alerts_ack_token (ack_token),
    UNIQUE KEY uq_alerts_open_monitor (open_monitor_id),
    INDEX idx_alerts_monitor (monitor_id),
    INDEX idx_alerts_status (status),
    INDEX idx_alerts_fired (fired_at)
);
