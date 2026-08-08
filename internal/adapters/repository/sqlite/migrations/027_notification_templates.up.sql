CREATE TABLE notification_templates (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id        INTEGER,
    name           TEXT NOT NULL,
    provider       TEXT NOT NULL,
    title_template TEXT NOT NULL DEFAULT '',
    body_template  TEXT NOT NULL,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX idx_notification_templates_provider ON notification_templates(provider);

ALTER TABLE notifications
    ADD COLUMN template_id INTEGER REFERENCES notification_templates(id) ON DELETE SET NULL;

CREATE INDEX idx_notifications_template ON notifications(template_id);
