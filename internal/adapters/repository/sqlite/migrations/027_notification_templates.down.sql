DROP INDEX IF EXISTS idx_notifications_template;
ALTER TABLE notifications DROP COLUMN template_id;
DROP TABLE IF EXISTS notification_templates;
