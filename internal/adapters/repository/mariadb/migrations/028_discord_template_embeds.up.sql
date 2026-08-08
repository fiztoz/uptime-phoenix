ALTER TABLE notification_templates
    ADD COLUMN config JSON NOT NULL DEFAULT '{}' AFTER body_template;
