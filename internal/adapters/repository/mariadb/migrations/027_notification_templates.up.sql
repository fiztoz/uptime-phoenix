CREATE TABLE notification_templates (
    id             BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id        BIGINT NULL,
    name           VARCHAR(255) NOT NULL,
    provider       VARCHAR(50) NOT NULL,
    title_template VARCHAR(1000) NOT NULL DEFAULT '',
    body_template  TEXT NOT NULL,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    CONSTRAINT fk_notification_templates_user
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
    INDEX idx_notification_templates_provider (provider)
);

ALTER TABLE notifications
    ADD COLUMN template_id BIGINT NULL AFTER is_default,
    ADD CONSTRAINT fk_notifications_template
        FOREIGN KEY (template_id) REFERENCES notification_templates(id) ON DELETE SET NULL,
    ADD INDEX idx_notifications_template (template_id);
