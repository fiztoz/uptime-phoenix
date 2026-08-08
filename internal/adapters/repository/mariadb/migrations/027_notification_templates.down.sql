ALTER TABLE notifications
    DROP FOREIGN KEY fk_notifications_template,
    DROP INDEX idx_notifications_template,
    DROP COLUMN template_id;

DROP TABLE IF EXISTS notification_templates;
